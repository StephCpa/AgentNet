package rpc_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentnet/agentnet/internal/authority"
	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/internal/signed"
	commonv1 "github.com/agentnet/agentnet/proto/gen/anet/common/v1"
	icsv1 "github.com/agentnet/agentnet/proto/gen/anet/ics/v1"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	"github.com/agentnet/agentnet/services/ics"
	service "github.com/agentnet/agentnet/services/rpc"
	"github.com/agentnet/agentnet/services/tcs"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type fixture struct {
	t                                   *testing.T
	ctx                                 context.Context
	db                                  *pgxpool.Pool
	store                               *tcs.Store
	ics                                 *ics.Service
	server                              *grpc.Server
	address                             string
	domain, consent                     string
	caller, adapter                     tcs.Caller
	command                             tcs.Command
	cfg                                 tcs.Config
	gwKey, icsKey, adapterKey, caKey    ed25519.PrivateKey
	ca                                  *x509.Certificate
	roots                               *x509.CertPool
	workerCert, adapterCert, serverCert tls.Certificate
	conn                                *grpc.ClientConn
	control                             tcsv1.TaskControlClient
	policy                              icsv1.IdentityPolicyClient
}

func key(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, private
}
func cert(t *testing.T, ca *x509.Certificate, caKey ed25519.PrivateKey, uri string, server bool, life time.Duration) tls.Certificate {
	t.Helper()
	pub, private := key(t)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "not-an-identity"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(life), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if uri != "" {
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		template.URIs = []*url.URL{u}
	}
	if server {
		template.DNSNames = []string{"localhost"}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, pub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private, Leaf: leaf}
}
func setup(t *testing.T) *fixture {
	t.Helper()
	if os.Getenv("ANET_TEST_DSN") == "" {
		t.Skip("requires test/schema/run.py --kernel-tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	db, err := pgxpool.New(ctx, os.Getenv("ANET_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	var id string
	if err = db.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ctx: ctx, db: db, domain: "domain-" + id, consent: "consent-" + id, caller: tcs.Caller{Principal: "user-" + id, Agent: "agent-" + id, Instance: "instance-" + id}, adapter: tcs.Caller{Principal: "service-" + id, Agent: "adapter-" + id, Instance: "adapter-instance-" + id}}
	caPub, caKey := key(t)
	f.caKey = caKey
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture CA"}, IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, root, root, caPub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	f.ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	f.roots = x509.NewCertPool()
	f.roots.AddCert(f.ca)
	f.workerCert = cert(t, f.ca, caKey, "spiffe://test.agentnet/agent-instance/"+f.caller.Instance, false, time.Hour)
	f.adapterCert = cert(t, f.ca, caKey, "spiffe://test.agentnet/agent-instance/"+f.adapter.Instance, false, time.Hour)
	f.serverCert = cert(t, f.ca, caKey, "", true, time.Hour)
	f.exec(`INSERT INTO domains(domain_id,name,gateway_endpoint,trust_anchor) VALUES($1,'test','https://invalid.test','{}')`, f.domain)
	f.register(f.caller, f.workerCert, "user")
	f.register(f.adapter, f.adapterCert, "service")
	f.exec(`INSERT INTO instance_roles(instance_id,role) VALUES($1,'adapter')`, f.adapter.Instance)
	gwPub, gwKey := key(t)
	icsPub, icsKey := key(t)
	adapterPub, adapterKey := key(t)
	f.gwKey = gwKey
	f.icsKey = icsKey
	f.adapterKey = adapterKey
	f.cfg = tcs.Config{Domain: f.domain, CommandKeys: map[string]ed25519.PublicKey{"gateway": gwPub}, RootKeys: map[string]ed25519.PublicKey{"ics": icsPub}, DecisionKeys: map[string]ed25519.PublicKey{"ics": icsPub}, ResultKeys: map[string]ed25519.PublicKey{"adapter": adapterPub}, Operations: map[string]tcs.Operation{"bank": {Action: "pay", Resource: "account:test", Purpose: "tool_call", Capability: "queryable", Dimensions: []string{"money", "tokens", "calls", "messages"}, Sensitivity: "sensitive", DataClass: "internal"}}}
	f.store, err = tcs.New(db, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.ics, err = ics.New(db, ics.Config{Domain: f.domain, KeyID: "ics", Key: icsKey, CommandKeys: f.cfg.CommandKeys, Operations: f.cfg.Operations})
	if err != nil {
		t.Fatal(err)
	}
	f.server, err = service.New(db, f.store, f.ics, service.Config{Domain: f.domain, TrustDomain: "test.agentnet", Certificate: f.serverCert, ClientCAs: f.roots, Operations: f.cfg.Operations})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.address = listener.Addr().String()
	go f.server.Serve(listener)
	t.Cleanup(f.server.Stop)
	f.conn = f.connect(f.workerCert)
	f.control = tcsv1.NewTaskControlClient(f.conn)
	f.policy = icsv1.NewIdentityPolicyClient(f.conn)
	rights := authority.Rights{Actions: []string{"pay"}, Resources: []string{"account:test"}, DataClasses: []string{"internal"}, Spatial: authority.Spatial{Domains: []string{f.domain}, EgressOnlyViaGateway: true}}
	rb, _ := json.Marshal(rights)
	f.exec(`INSERT INTO root_consents(consent_id,domain_id,subject_principal_id,actor_agent_id,rights,task_limits,not_before,expires_at,policy_version) VALUES($1,$2,$3,$4,$5::jsonb,'{"money":"10","tokens":"10","calls":"10","messages":"10"}',clock_timestamp()-interval '1 minute',clock_timestamp()+interval '2 hours','policy/1')`, f.consent, f.domain, f.caller.Principal, f.caller.Agent, string(rb))
	body, _ := json.Marshal(map[string]any{"deadline": time.Now().UTC().Add(time.Hour), "input_refs": []any{map[string]any{"version": 1, "digest": "sha256:" + strings.Repeat("a", 64)}}, "output_schema": map[string]string{"type": "object"}, "acceptance_policy": map[string]string{"verifier": "fixture"}})
	f.command = tcs.Command{ID: "command-" + id, Audience: f.domain, NotAfter: time.Now().UTC().Add(time.Hour), ContractID: "contract-" + id, ContractVersion: 1, Contract: body, Limits: map[string]string{"money": "10", "tokens": "10", "calls": "10", "messages": "10"}, Root: &tcs.RootRequest{ConsentID: f.consent, Rights: rights}}
	return f
}
func (f *fixture) register(c tcs.Caller, certificate tls.Certificate, kind string) {
	f.exec(`INSERT INTO principals(principal_id,kind,domain_id) VALUES($1,$2,$3)`, c.Principal, kind, f.domain)
	f.exec(`INSERT INTO agents(agent_id,domain_id,owner_principal_id,handle,card_digest) VALUES($1,$2,$3,$1,'test')`, c.Agent, f.domain, c.Principal)
	f.exec(`INSERT INTO agent_instances(instance_id,agent_id,cred_hash,spiffe_id) VALUES($1,$2,$3,$4)`, c.Instance, c.Agent, service.Fingerprint(certificate.Leaf), certificate.Leaf.URIs[0].String())
}
func (f *fixture) connect(certificate tls.Certificate) *grpc.ClientConn {
	f.t.Helper()
	cfg := &tls.Config{RootCAs: f.roots, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	if len(certificate.Certificate) > 0 {
		cfg.Certificates = []tls.Certificate{certificate}
	}
	conn, err := grpc.NewClient(f.address, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { conn.Close() })
	return conn
}
func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}
func pb(e tcs.Envelope) *commonv1.SignedEnvelope {
	return &commonv1.SignedEnvelope{KeyId: e.KeyID, Payload: e.Payload, Signature: e.Signature}
}
func (f *fixture) commandEnvelope(cmd tcs.Command) *commonv1.SignedEnvelope {
	f.t.Helper()
	e, err := signed.Encode("gateway", f.gwKey, tcs.Submission{Kind: "anet.command/1", Caller: f.caller, Command: cmd})
	if err != nil {
		f.t.Fatal(err)
	}
	return pb(e)
}
func (f *fixture) request() *tcsv1.SubmitCommandRequest {
	f.t.Helper()
	command := f.commandEnvelope(f.command)
	permit, err := f.policy.IssueRootPermit(f.ctx, &icsv1.IssueRootPermitRequest{SignedCommand: command})
	if err != nil {
		f.t.Fatal(err)
	}
	return &tcsv1.SubmitCommandRequest{SignedCommand: command, RootPermit: permit.SignedPermit}
}
func (f *fixture) submit() *tcsv1.AdmissionResult {
	f.t.Helper()
	out, err := f.control.SubmitCommand(f.ctx, f.request())
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}
func code(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("want %v, got %v", want, err)
	}
}
func (f *fixture) proposal(a *tcsv1.AdmissionResult) (*tcsv1.AdmitEffectRequest, *icsv1.Decision) {
	f.t.Helper()
	lease, err := f.control.AcquireLease(f.ctx, &tcsv1.AcquireLeaseRequest{TaskId: a.TaskId})
	if err != nil {
		f.t.Fatal(err)
	}
	params, _ := canonical.Digest([]byte(`{"amount":"2"}`))
	decision, err := f.policy.Decide(f.ctx, &icsv1.DecideRequest{GrantId: a.RootGrantId, TaskId: a.TaskId, TargetRef: "bank", ParamsDigest: params, AudienceDomainId: f.domain, Sensitivity: "sensitive", DataClass: "internal"})
	if err != nil {
		f.t.Fatal(err)
	}
	p := &tcsv1.EffectProposal{EffectId: "effect-" + f.domain, TaskId: a.TaskId, CommandId: f.command.ID, TargetRef: "bank", ParamsDigest: params, AuthorityDigest: decision.AuthorityDigest, Epoch: lease.Epoch, Sensitivity: "sensitive", DataClass: "internal"}
	for _, dim := range []string{"money", "tokens", "calls", "messages"} {
		r, err := f.control.ReserveBudget(f.ctx, &tcsv1.ReserveBudgetRequest{TaskId: a.TaskId, Dimension: dim, Purpose: "tool_call", UpperBound: "4"})
		if err != nil {
			f.t.Fatal(err)
		}
		p.Reservations = append(p.Reservations, &tcsv1.DimensionalReservation{Dimension: dim, ReservationRef: r.ReservationId, UpperBound: "3"})
	}
	return &tcsv1.AdmitEffectRequest{Effect: p, LeaseId: lease.LeaseId, ExpectedTaskVersion: lease.TaskVersion, SignedDecision: decision.SignedDecision}, decision
}
func TestRPCAuthenticatedLifecycle(t *testing.T) {
	f := setup(t)
	request := f.request()
	a, err := f.control.SubmitCommand(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.control.SubmitCommand(f.ctx, request)
	if err != nil || again.Outcome != tcsv1.AdmissionResult_EXISTING || !proto.Equal(a.AcceptedReceipt, again.AcceptedReceipt) || a.RootGrantId != again.RootGrantId {
		t.Fatal(again, err)
	}
	if f.count(`SELECT count(*) FROM grants WHERE task_id=$1`, a.TaskId) != 1 {
		t.Fatal("root grant not atomic/idempotent")
	}
	proposal, decision := f.proposal(a)
	if decision.PolicyVersion != "policy/1" || decision.ActorInstanceId != f.caller.Instance || decision.SignedDecision == nil {
		t.Fatal("decision not bound")
	}
	if _, err = f.control.AdmitEffect(f.ctx, proposal); err != nil {
		t.Fatal(err)
	}
	snap, err := f.control.GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: a.TaskId})
	if err != nil || len(snap.Effects) != 1 || len(snap.Budget) != 4 || len(snap.Receipts) != 1 {
		t.Fatal(snap, err)
	}
	if _, err = f.control.ReleaseReservation(f.ctx, &tcsv1.ReleaseReservationRequest{TaskId: a.TaskId, ReservationId: proposal.Effect.Reservations[0].ReservationRef}); status.Code(err) != codes.FailedPrecondition {
		t.Fatal(err)
	}
	cancelled, err := f.control.CancelTask(f.ctx, &tcsv1.CancelTaskRequest{TaskId: a.TaskId})
	if err != nil || cancelled.AlreadyCancelled {
		t.Fatal(cancelled, err)
	}
	cancelled, err = f.control.CancelTask(f.ctx, &tcsv1.CancelTaskRequest{TaskId: a.TaskId})
	if err != nil || !cancelled.AlreadyCancelled || cancelled.CancelVersion != 1 {
		t.Fatal(cancelled, err)
	}
	result := tcs.Result{Kind: "anet.result/1", Task: a.TaskId, Effect: proposal.Effect.EffectId, ParamsDigest: proposal.Effect.ParamsDigest, Audience: f.domain, Outcome: "COMMITTED", Evidence: "fixture:business-receipt", UsageSource: "provider", Usage: map[string]string{"money": "2", "tokens": "2", "calls": "2", "messages": "2"}}
	e, err := signed.Encode("adapter", f.adapterKey, result)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.control.ReportEffectResult(f.ctx, &tcsv1.ReportEffectResultRequest{SignedResult: pb(e)})
	code(t, err, codes.PermissionDenied)
	adapter := tcsv1.NewTaskControlClient(f.connect(f.adapterCert))
	if _, err = adapter.ReportEffectResult(f.ctx, &tcsv1.ReportEffectResultRequest{SignedResult: pb(e)}); err != nil {
		t.Fatal(err)
	}
	snap, err = f.control.GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: a.TaskId})
	if err != nil || snap.Status != "CANCELLED" || snap.Effects[0].State != "COMMITTED" {
		t.Fatal(snap, err)
	}
	if f.count(`SELECT sum(settled)::int FROM budget_accounts WHERE task_id=$1`, a.TaskId) != 8 {
		t.Fatal("network settlement wrong")
	}
}
func TestRPCCertificateAndBodyIdentity(t *testing.T) {
	f := setup(t)
	t.Run("no-client-certificate", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(f.ctx, time.Second)
		defer cancel()
		_, err := tcsv1.NewTaskControlClient(f.connect(tls.Certificate{})).GetTask(ctx, &tcsv1.GetTaskRequest{TaskId: "anything"})
		if err == nil {
			t.Fatal("anonymous RPC accepted")
		}
	})
	t.Run("unregistered-client", func(t *testing.T) {
		unknown := cert(t, f.ca, f.caKey, "spiffe://test.agentnet/agent-instance/unregistered", false, time.Hour)
		_, err := tcsv1.NewTaskControlClient(f.connect(unknown)).GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: "anything"})
		code(t, err, codes.Unauthenticated)
	})
	t.Run("legacy-body", func(t *testing.T) {
		r := f.request()
		r.Caller = &tcsv1.PrincipalRef{PrincipalId: "admin"}
		_, err := f.control.SubmitCommand(f.ctx, r)
		code(t, err, codes.InvalidArgument)
	})
	t.Run("metadata-spoof", func(t *testing.T) {
		ctx := metadata.NewOutgoingContext(f.ctx, metadata.Pairs("caller", "admin", "role", "adapter", "instance", f.adapter.Instance))
		_, err := f.control.ReportEffectResult(ctx, &tcsv1.ReportEffectResultRequest{})
		code(t, err, codes.PermissionDenied)
	})
	t.Run("signed-other-caller", func(t *testing.T) {
		e, err := signed.Encode("gateway", f.gwKey, tcs.Submission{Kind: "anet.command/1", Caller: f.adapter, Command: f.command})
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.policy.IssueRootPermit(f.ctx, &icsv1.IssueRootPermitRequest{SignedCommand: pb(e)})
		code(t, err, codes.PermissionDenied)
	})
}
func TestRPCRevocationOnExistingTLSChannel(t *testing.T) {
	f := setup(t)
	a := f.submit()
	if _, err := f.control.GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: a.TaskId}); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE agent_instances SET revoked_at=clock_timestamp() WHERE instance_id=$1`, f.caller.Instance)
	_, err := f.control.GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: a.TaskId})
	code(t, err, codes.Unauthenticated)
}
func TestRPCRegisteredWrongSPIFFEAndLongCertificate(t *testing.T) {
	for _, mode := range []string{"uri", "lifetime"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			uri := "spiffe://test.agentnet/agent-instance/" + f.caller.Instance
			life := time.Hour
			if mode == "uri" {
				uri = "spiffe://another.agentnet/agent-instance/" + f.caller.Instance
			} else {
				life = 25 * time.Hour
			}
			bad := cert(t, f.ca, f.caKey, uri, false, life)
			f.exec(`UPDATE agent_instances SET cred_hash=$2,spiffe_id=$3 WHERE instance_id=$1`, f.caller.Instance, service.Fingerprint(bad.Leaf), uri)
			_, err := tcsv1.NewTaskControlClient(f.connect(bad)).GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: "anything"})
			code(t, err, codes.Unauthenticated)
		})
	}
}
func TestRPCConsentSubsetAndRevokedPermit(t *testing.T) {
	f := setup(t)
	tooMuch := f.command
	tooMuch.Limits = map[string]string{"money": "11", "tokens": "10", "calls": "10", "messages": "10"}
	_, err := f.policy.IssueRootPermit(f.ctx, &icsv1.IssueRootPermitRequest{SignedCommand: f.commandEnvelope(tooMuch)})
	code(t, err, codes.ResourceExhausted)
	broad := f.command
	root := *f.command.Root
	broad.Root = &root
	root.Rights.Resources = []string{"account:someone-else"}
	_, err = f.policy.IssueRootPermit(f.ctx, &icsv1.IssueRootPermitRequest{SignedCommand: f.commandEnvelope(broad)})
	code(t, err, codes.PermissionDenied)
	request := f.request()
	f.exec(`UPDATE root_consents SET revoked_at=clock_timestamp(),version=version+1 WHERE consent_id=$1`, f.consent)
	_, err = f.control.SubmitCommand(f.ctx, request)
	code(t, err, codes.PermissionDenied)
	if f.count(`SELECT count(*) FROM tasks WHERE domain_id=$1`, f.domain) != 0 || f.count(`SELECT count(*) FROM grants WHERE consent_id=$1`, f.consent) != 0 {
		t.Fatal("revoked permit partially committed")
	}
}
func TestRPCGrantRevocationAndUnsignedDecision(t *testing.T) {
	f := setup(t)
	a := f.submit()
	proposal, _ := f.proposal(a)
	unsigned := proto.Clone(proposal).(*tcsv1.AdmitEffectRequest)
	unsigned.SignedDecision = nil
	unsigned.Authz = &tcsv1.AuthorizationDecision{Allow: true}
	_, err := f.control.AdmitEffect(f.ctx, unsigned)
	code(t, err, codes.InvalidArgument)
	revoked, err := f.policy.Revoke(f.ctx, &icsv1.RevokeRequest{Target: &icsv1.RevokeRequest_GrantId{GrantId: a.RootGrantId}, Reason: "owner withdrawal"})
	if err != nil || revoked.EffectiveAt == nil {
		t.Fatal(revoked, err)
	}
	_, err = f.control.AdmitEffect(f.ctx, proposal)
	code(t, err, codes.PermissionDenied)
	if f.count(`SELECT count(*) FROM effects WHERE task_id=$1`, a.TaskId) != 0 {
		t.Fatal("revoked decision admitted")
	}
}
func TestRPCPolicyClassAndVersionBinding(t *testing.T) {
	f := setup(t)
	a := f.submit()
	proposal, decision := f.proposal(a)
	_, err := f.policy.Decide(f.ctx, &icsv1.DecideRequest{GrantId: a.RootGrantId, TaskId: a.TaskId, TargetRef: "bank", ParamsDigest: proposal.Effect.ParamsDigest, AudienceDomainId: f.domain, Sensitivity: "standard", DataClass: "public"})
	code(t, err, codes.PermissionDenied)
	if !strings.HasPrefix(decision.AuthorityDigest, "sha256:") {
		t.Fatal("missing authoritative snapshot")
	}
	f.exec(`UPDATE grants SET policy_version='policy/2' WHERE grant_id=$1`, a.RootGrantId)
	_, err = f.control.AdmitEffect(f.ctx, proposal)
	code(t, err, codes.PermissionDenied)
}
func TestRPCConsentVersionInvalidatesDecision(t *testing.T) {
	f := setup(t)
	a := f.submit()
	proposal, _ := f.proposal(a)
	// The signed decision and grant are unchanged; a new authoritative consent
	// revision alone must block T2, even on the existing TLS connection.
	f.exec(`UPDATE root_consents SET version=version+1,policy_version='policy/2' WHERE consent_id=$1`, f.consent)
	_, err := f.control.AdmitEffect(f.ctx, proposal)
	code(t, err, codes.PermissionDenied)
	if f.count(`SELECT count(*) FROM effects WHERE task_id=$1`, a.TaskId) != 0 {
		t.Fatal("stale consent revision admitted")
	}
}
func TestRPCAdapterRoleRevocationOnExistingChannel(t *testing.T) {
	f := setup(t)
	client := tcsv1.NewTaskControlClient(f.connect(f.adapterCert))
	// InvalidArgument proves the adapter passed the role gate; the same request
	// after role withdrawal must be stopped before payload validation.
	_, err := client.ReportEffectResult(f.ctx, &tcsv1.ReportEffectResultRequest{})
	code(t, err, codes.InvalidArgument)
	f.exec(`DELETE FROM instance_roles WHERE instance_id=$1 AND role='adapter'`, f.adapter.Instance)
	_, err = client.ReportEffectResult(f.ctx, &tcsv1.ReportEffectResultRequest{})
	code(t, err, codes.PermissionDenied)
}
func TestRPCLeaseIdentityAndCrossOwner(t *testing.T) {
	f := setup(t)
	a := f.submit()
	_, err := f.control.AcquireLease(f.ctx, &tcsv1.AcquireLeaseRequest{TaskId: a.TaskId, WorkerInstanceId: f.adapter.Instance})
	code(t, err, codes.PermissionDenied)
	_, err = tcsv1.NewTaskControlClient(f.connect(f.adapterCert)).GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: a.TaskId})
	code(t, err, codes.PermissionDenied)
	if _, err = f.control.CreateSubtask(f.ctx, &tcsv1.CreateSubtaskRequest{ParentTaskId: a.TaskId}); status.Code(err) != codes.Unimplemented {
		t.Fatal(err)
	}
}
func TestRPCT1RootRollbackAndErrorDetails(t *testing.T) {
	f := setup(t)
	request := f.request()
	name := "fail_root_" + strings.ReplaceAll(f.domain, "-", "")
	f.exec(`ALTER TABLE outbox ADD CONSTRAINT ` + name + ` CHECK(dedup_key NOT LIKE 'accepted:` + f.domain + `:%') NOT VALID`)
	defer f.exec(`ALTER TABLE outbox DROP CONSTRAINT ` + name)
	_, err := f.control.SubmitCommand(f.ctx, request)
	code(t, err, codes.Internal)
	if strings.Contains(err.Error(), "constraint") || strings.Contains(err.Error(), "INSERT") {
		t.Fatal("SQL leaked")
	}
	if f.count(`SELECT count(*) FROM tasks WHERE domain_id=$1`, f.domain) != 0 || f.count(`SELECT count(*) FROM grants WHERE consent_id=$1`, f.consent) != 0 {
		t.Fatal("root grant escaped T1 rollback")
	}
	_, err = f.control.SubmitCommand(f.ctx, &tcsv1.SubmitCommandRequest{})
	code(t, err, codes.InvalidArgument)
	found := false
	for _, detail := range status.Convert(err).Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.Reason == string(tcs.Invalid) {
			found = true
		}
	}
	if !found {
		t.Fatal("missing ANET error details")
	}
}
func TestRPCRootPermitSignatureAndIndependentCeiling(t *testing.T) {
	f := setup(t)
	request := f.request()
	tampered := proto.Clone(request).(*tcsv1.SubmitCommandRequest)
	tampered.RootPermit.Signature[0] ^= 1
	_, err := f.control.SubmitCommand(f.ctx, tampered)
	code(t, err, codes.PermissionDenied)
	var permit tcs.RootPermit
	if err = json.Unmarshal(request.RootPermit.Payload, &permit); err != nil {
		t.Fatal(err)
	}
	cmd := f.command
	cmd.Limits = map[string]string{"money": "100", "tokens": "10", "calls": "10", "messages": "10"}
	command := f.commandEnvelope(cmd)
	permit.CommandDigest, _ = canonical.Digest(command.Payload)
	root, err := signed.Encode("ics", f.icsKey, permit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.control.SubmitCommand(f.ctx, &tcsv1.SubmitCommandRequest{SignedCommand: command, RootPermit: pb(root)})
	code(t, err, codes.ResourceExhausted)
	if f.count(`SELECT count(*) FROM tasks WHERE domain_id=$1`, f.domain) != 0 {
		t.Fatal("issuer bug bypassed independent consent ceiling")
	}
}

// Used by the executable test as well as normal TLS fixtures.
func pemFiles(certificate tls.Certificate) ([]byte, []byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
