package rpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"

	commonv1 "github.com/agentnet/agentnet/proto/gen/anet/common/v1"
	icsv1 "github.com/agentnet/agentnet/proto/gen/anet/ics/v1"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	"github.com/agentnet/agentnet/services/ics"
	"github.com/agentnet/agentnet/services/tcs"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Config struct {
	Domain, TrustDomain string
	Certificate         tls.Certificate
	ClientCAs           *x509.CertPool
	Operations          map[string]tcs.Operation
}
type control struct {
	tcsv1.UnimplementedTaskControlServer
	store  *tcs.Store
	domain string
	ops    map[string]tcs.Operation
}
type policy struct {
	icsv1.UnimplementedIdentityPolicyServer
	service *ics.Service
	ops     map[string]tcs.Operation
}

func New(db *pgxpool.Pool, store *tcs.Store, icsService *ics.Service, c Config) (*grpc.Server, error) {
	if db == nil || store == nil || icsService == nil || c.Domain == "" || c.TrustDomain == "" || len(c.Certificate.Certificate) == 0 || c.Certificate.PrivateKey == nil || c.ClientCAs == nil || len(c.ClientCAs.Subjects()) == 0 || len(c.Operations) == 0 {
		return nil, tcs.Invalid
	}
	ops := map[string]tcs.Operation{}
	for target, op := range c.Operations {
		if op.DataClass == "" || (op.Sensitivity != "sensitive" && op.Sensitivity != "standard") {
			return nil, tcs.Invalid
		}
		ops[target] = op
	}
	auth := Authenticator{db, c.Domain, c.TrustDomain}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{c.Certificate}, ClientCAs: c.ClientCAs, ClientAuth: tls.RequireAndVerifyClientCert}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)), grpc.ChainUnaryInterceptor(auth.Unary), grpc.ChainStreamInterceptor(auth.Stream), grpc.MaxRecvMsgSize(2<<20), grpc.MaxSendMsgSize(4<<20))
	tcsv1.RegisterTaskControlServer(server, &control{store: store, domain: c.Domain, ops: ops})
	icsv1.RegisterIdentityPolicyServer(server, &policy{service: icsService, ops: ops})
	return server, nil
}
func rpcError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "request cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "request deadline")
	}
	var code tcs.Code
	if errors.As(err, &code) {
		grpcCode := codes.FailedPrecondition
		switch code {
		case tcs.Invalid:
			grpcCode = codes.InvalidArgument
		case tcs.Denied, tcs.DecisionMismatch:
			grpcCode = codes.PermissionDenied
		case tcs.Conflict, tcs.EffectConflict, tcs.ResultConflict:
			grpcCode = codes.AlreadyExists
		case tcs.VersionConflict, tcs.Fenced:
			grpcCode = codes.Aborted
		case tcs.BudgetExhausted:
			grpcCode = codes.ResourceExhausted
		}
		s := status.New(grpcCode, string(code))
		with, detailErr := s.WithDetails(&errdetails.ErrorInfo{Reason: string(code), Domain: "agentnet"})
		if detailErr == nil {
			return with.Err()
		}
		return s.Err()
	}
	var dbErr *pgconn.PgError
	if errors.As(err, &dbErr) && (dbErr.Code == "40001" || dbErr.Code == "40P01") {
		return status.Error(codes.Aborted, "transaction retry required")
	}
	return status.Error(codes.Internal, "internal service error")
}
func envelope(p *commonv1.SignedEnvelope) (tcs.Envelope, error) {
	if p == nil || p.KeyId == "" || len(p.Payload) == 0 || len(p.Payload) > 1<<20 || len(p.Signature) != 64 || len(p.ProtoReflect().GetUnknown()) > 0 {
		return tcs.Envelope{}, tcs.Invalid
	}
	return tcs.Envelope{KeyID: p.KeyId, Payload: p.Payload, Signature: p.Signature}, nil
}
func wire(e tcs.Envelope) *commonv1.SignedEnvelope {
	return &commonv1.SignedEnvelope{KeyId: e.KeyID, Payload: e.Payload, Signature: e.Signature}
}
func strict(p proto.Message, allowed ...protoreflect.FieldNumber) bool {
	if p == nil || !p.ProtoReflect().IsValid() {
		return false
	}
	m := p.ProtoReflect()
	if len(m.GetUnknown()) > 0 {
		return false
	}
	valid := true
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if len(allowed) > 0 {
			found := false
			for _, n := range allowed {
				found = found || n == f.Number()
			}
			if !found {
				valid = false
				return false
			}
		}
		if f.Message() != nil {
			if f.IsList() {
				for i := 0; i < v.List().Len(); i++ {
					if !strict(v.List().Get(i).Message().Interface()) {
						valid = false
						return false
					}
				}
			} else if !strict(v.Message().Interface()) {
				valid = false
				return false
			}
		}
		return true
	})
	return valid
}
func (c *control) SubmitCommand(ctx context.Context, r *tcsv1.SubmitCommandRequest) (*tcsv1.AdmissionResult, error) {
	who, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(r, 13, 14) {
		return nil, rpcError(tcs.Invalid)
	}
	command, err := envelope(r.SignedCommand)
	if err != nil {
		return nil, rpcError(err)
	}
	root, err := envelope(r.RootPermit)
	if err != nil {
		return nil, rpcError(err)
	}
	a, err := c.store.SubmitAuthorized(ctx, who, command, root)
	if err != nil {
		return nil, rpcError(err)
	}
	d, err := c.store.Details(ctx, who, a.TaskID)
	if err != nil {
		return nil, rpcError(err)
	}
	result := &tcsv1.AdmissionResult{TaskId: a.TaskID, RootGrantId: a.RootGrantID}
	if a.Existing {
		result.Outcome = tcsv1.AdmissionResult_EXISTING
	}
	for _, receipt := range d.Receipts {
		if receipt.ID == a.ReceiptID {
			result.AcceptedReceipt = receiptPB(receipt)
		}
	}
	return result, nil
}
func receiptPB(r tcs.ReceiptDetails) *tcsv1.Receipt {
	return &tcsv1.Receipt{ReceiptId: r.ID, Kind: r.Kind, IssuerDomainId: r.Issuer, TaskId: r.Task, CommandId: r.Command, EffectId: r.Effect, Verifier: r.Verifier, PolicyVersion: r.Policy, EvidenceManifestRef: r.Manifest, ResultRef: r.Result, IssuedAt: timestamppb.New(r.At)}
}
func (c *control) GetTask(ctx context.Context, r *tcsv1.GetTaskRequest) (*tcsv1.TaskSnapshot, error) {
	who, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(r) {
		return nil, rpcError(tcs.Invalid)
	}
	d, err := c.store.Details(ctx, who, r.TaskId)
	if err != nil {
		return nil, rpcError(err)
	}
	out := &tcsv1.TaskSnapshot{TaskId: d.ID, RootTaskId: d.Root, ParentTaskId: d.Parent, Status: d.Status, TaskVersion: d.Version, CurrentEpoch: d.Epoch, CancelVersion: d.CancelVersion, AssigneeAgentId: d.Agent, AssigneeDomainId: d.AssigneeDomain, BudgetFrozen: d.Frozen}
	if d.Deadline != nil {
		out.Deadline = timestamppb.New(*d.Deadline)
	}
	for _, f := range d.Effects {
		out.Effects = append(out.Effects, &tcsv1.EffectState{EffectId: f.ID, TaskId: f.Task, CommandId: f.Command, TargetRef: f.Target, Capability: f.Capability, State: f.State, ParamsDigest: f.ParamsDigest, AuthorityDigest: f.AuthorityDigest, ResultRef: f.ResultRef, Epoch: f.Epoch})
	}
	for _, receipt := range d.Receipts {
		out.Receipts = append(out.Receipts, receiptPB(receipt))
	}
	for _, b := range d.Budgets {
		out.Budget = append(out.Budget, &tcsv1.BudgetAccountView{AccountId: b.ID, Dimension: b.Dimension, BudgetLimit: b.Limit, Settled: b.Settled, Held: b.Held, Delegated: b.Delegated})
	}
	return out, nil
}
func (c *control) AdmitEffect(ctx context.Context, r *tcsv1.AdmitEffectRequest) (*tcsv1.EffectAdmission, error) {
	who, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(r, 1, 3, 4, 5) || r.Effect == nil {
		return nil, rpcError(tcs.Invalid)
	}
	auth, err := envelope(r.SignedDecision)
	if err != nil {
		return nil, rpcError(err)
	}
	f := r.Effect
	op, ok := c.ops[f.TargetRef]
	if !ok || (f.Capability != "" && f.Capability != op.Capability) {
		return nil, rpcError(tcs.Denied)
	}
	p := tcs.Proposal{EffectID: f.EffectId, TaskID: f.TaskId, CommandID: f.CommandId, Target: f.TargetRef, ParamsDigest: f.ParamsDigest, AuthorityDigest: f.AuthorityDigest, LeaseID: r.LeaseId, Sensitivity: f.Sensitivity, DataClass: f.DataClass, Epoch: f.Epoch, ExpectedVersion: r.ExpectedTaskVersion, Reservations: map[string]string{}, UpperBounds: map[string]string{}}
	for _, h := range f.Reservations {
		if h == nil || p.Reservations[h.Dimension] != "" {
			return nil, rpcError(tcs.Invalid)
		}
		p.Reservations[h.Dimension] = h.ReservationRef
		p.UpperBounds[h.Dimension] = h.UpperBound
	}
	e, err := c.store.Admit(ctx, who, p, auth)
	if err != nil {
		return nil, rpcError(err)
	}
	out := &tcsv1.EffectAdmission{State: e.State}
	if e.Existing {
		out.Outcome = tcsv1.EffectAdmission_EXISTING
	}
	return out, nil
}
func (c *control) ReportEffectResult(ctx context.Context, r *tcsv1.ReportEffectResultRequest) (*tcsv1.EffectState, error) {
	if _, err := caller(ctx); err != nil {
		return nil, err
	}
	if !strict(r, 7) {
		return nil, rpcError(tcs.Invalid)
	}
	signed, err := envelope(r.SignedResult)
	if err != nil {
		return nil, rpcError(err)
	}
	e, err := c.store.Resolve(ctx, signed)
	if err != nil {
		return nil, rpcError(err)
	}
	var result tcs.Result
	if json.Unmarshal(signed.Payload, &result) != nil {
		return nil, rpcError(tcs.Invalid)
	}
	return &tcsv1.EffectState{EffectId: e.ID, State: e.State, TaskId: result.Task, ParamsDigest: result.ParamsDigest, ResultRef: result.Evidence}, nil
}
func (c *control) CancelTask(ctx context.Context, r *tcsv1.CancelTaskRequest) (*tcsv1.CancelResult, error) {
	who, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(r) {
		return nil, rpcError(tcs.Invalid)
	}
	version, existing, err := c.store.CancelDetailed(ctx, who, r.TaskId)
	if err != nil {
		return nil, rpcError(err)
	}
	return &tcsv1.CancelResult{CancelVersion: version, AlreadyCancelled: existing}, nil
}
func (c *control) ReserveBudget(ctx context.Context, r *tcsv1.ReserveBudgetRequest) (*tcsv1.Reservation, error) {
	who, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(r) || r.AttemptId != "" || r.EffectId != "" || r.SoftTtlSeconds != 0 {
		return nil, rpcError(tcs.Invalid)
	}
	reserved, err := c.store.Reserve(ctx, who, r.TaskId, r.Dimension, r.Purpose, r.UpperBound)
	if err != nil {
		return nil, rpcError(err)
	}
	d, err := c.store.Reservation(ctx, who, r.TaskId, reserved.ID)
	if err != nil {
		return nil, rpcError(err)
	}
	return reservationPB(d), nil
}
func reservationPB(d tcs.ReservationDetails) *tcsv1.Reservation {
	return &tcsv1.Reservation{ReservationId: d.ID, AccountId: d.Account, State: d.State, Amount: d.Amount}
}
func (c *control) ReleaseReservation(ctx context.Context, r *tcsv1.ReleaseReservationRequest) (*tcsv1.Reservation, error) {
	who, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(r) {
		return nil, rpcError(tcs.Invalid)
	}
	if err = c.store.ReleaseUnbound(ctx, who, r.TaskId, r.ReservationId); err != nil {
		return nil, rpcError(err)
	}
	d, err := c.store.Reservation(ctx, who, r.TaskId, r.ReservationId)
	if err != nil {
		return nil, rpcError(err)
	}
	return reservationPB(d), nil
}
func leasePB(l tcs.Lease) *tcsv1.Lease {
	return &tcsv1.Lease{LeaseId: l.ID, Epoch: l.Epoch, ExpiresAt: timestamppb.New(l.ExpiresAt), TaskVersion: l.Version}
}
func (c *control) AcquireLease(ctx context.Context, r *tcsv1.AcquireLeaseRequest) (*tcsv1.Lease, error) {
	who, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(r) {
		return nil, rpcError(tcs.Invalid)
	}
	if r.WorkerInstanceId != "" && r.WorkerInstanceId != who.Instance {
		return nil, rpcError(tcs.Denied)
	}
	l, err := c.store.AcquireLease(ctx, who, r.TaskId)
	if err != nil {
		return nil, rpcError(err)
	}
	return leasePB(l), nil
}
func (c *control) RenewLease(ctx context.Context, r *tcsv1.RenewLeaseRequest) (*tcsv1.Lease, error) {
	who, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(r) {
		return nil, rpcError(tcs.Invalid)
	}
	if r.WorkerInstanceId != "" && r.WorkerInstanceId != who.Instance {
		return nil, rpcError(tcs.Denied)
	}
	l, err := c.store.RenewLease(ctx, who, r.TaskId, r.LeaseId)
	if err != nil {
		return nil, rpcError(err)
	}
	return leasePB(l), nil
}
func (p *policy) IssueRootPermit(ctx context.Context, r *icsv1.IssueRootPermitRequest) (*icsv1.RootPermit, error) {
	who, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(r) {
		return nil, rpcError(tcs.Invalid)
	}
	command, err := envelope(r.SignedCommand)
	if err != nil {
		return nil, rpcError(err)
	}
	out, err := p.service.IssueRoot(ctx, who, command)
	if err != nil {
		return nil, rpcError(err)
	}
	return &icsv1.RootPermit{SignedPermit: wire(out)}, nil
}
func (p *policy) Decide(ctx context.Context, r *icsv1.DecideRequest) (*icsv1.Decision, error) {
	who, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(r) {
		return nil, rpcError(tcs.Invalid)
	}
	op, ok := p.ops[r.TargetRef]
	if !ok || (r.Action != "" && r.Action != op.Action) || (r.Resource != "" && r.Resource != op.Resource) {
		return nil, rpcError(tcs.Denied)
	}
	out, err := p.service.Decide(ctx, who, ics.Request{GrantID: r.GrantId, TaskID: r.TaskId, Target: r.TargetRef, ParamsDigest: r.ParamsDigest, AuthorityDigest: r.AuthorityDigest, Audience: r.AudienceDomainId, Sensitivity: r.Sensitivity, DataClass: r.DataClass})
	if err != nil {
		return nil, rpcError(err)
	}
	var d tcs.Decision
	if json.Unmarshal(out.Payload, &d) != nil {
		return nil, rpcError(tcs.Invalid)
	}
	return &icsv1.Decision{DecisionId: d.ID, PolicyVersion: d.PolicyVersion, Allow: d.Allow, At: timestamppb.New(d.At), ExpiresAt: timestamppb.New(d.ExpiresAt), SubjectPrincipalId: d.Subject, ActorAgentId: d.Agent, ActorInstanceId: d.Instance, GrantId: d.Grant, TaskId: d.Task, Action: d.Action, Resource: d.Resource, ParamsDigest: d.ParamsDigest, AuthorityDigest: d.AuthorityDigest, AudienceDomainId: d.Audience, Sensitivity: d.Sensitivity, DataClass: d.DataClass, TargetRef: d.Target, SignedDecision: wire(out)}, nil
}
func (p *policy) Revoke(ctx context.Context, r *icsv1.RevokeRequest) (*icsv1.RevokeResult, error) {
	who, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(r) || r.GetGrantId() == "" {
		return nil, status.Error(codes.Unimplemented, "only owned root-grant revocation is implemented")
	}
	at, err := p.service.RevokeGrant(ctx, who, r.GetGrantId(), r.Reason)
	if err != nil {
		return nil, rpcError(err)
	}
	return &icsv1.RevokeResult{EffectiveAt: timestamppb.New(at)}, nil
}
