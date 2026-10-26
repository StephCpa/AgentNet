package rpc_test

import (
	"encoding/json"
	"net"
	"testing"

	localadapter "github.com/agentnet/agentnet/adapters/local"
	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/internal/localtx"
	icsv1 "github.com/agentnet/agentnet/proto/gen/anet/ics/v1"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	"github.com/agentnet/agentnet/services/ics"
	service "github.com/agentnet/agentnet/services/rpc"
	"github.com/agentnet/agentnet/services/tcs"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

func addRecords(f *fixture) {
	f.cfg.Operations["records"] = tcs.Operation{Action: "record.put", Resource: "records:test", Capability: "local-tx", Purpose: "tool_call", Dimensions: []string{"money", "tokens", "calls", "messages"}, Sensitivity: "sensitive", DataClass: "internal"}
	f.cfg.LocalHandlers = map[string]localtx.Handler{"records": localadapter.Records{}}
	f.command.Root.Rights.Actions = append(f.command.Root.Rights.Actions, "record.put")
	f.command.Root.Rights.Resources = append(f.command.Root.Rights.Resources, "records:test")
	rights, _ := json.Marshal(f.command.Root.Rights)
	f.exec("UPDATE root_consents SET rights=$2::jsonb,version=version+1 WHERE consent_id=$1", f.consent, string(rights))
}

func recordRequest(f *fixture, a *tcsv1.AdmissionResult) *tcsv1.ExecuteLocalEffectRequest {
	f.t.Helper()
	lease, err := f.control.AcquireLease(f.ctx, &tcsv1.AcquireLeaseRequest{TaskId: a.TaskId})
	if err != nil {
		f.t.Fatal(err)
	}
	raw := []byte(`{"key":"daemon-record","expected_version":0,"value":{"written":"atomically"}}`)
	digest, _ := canonical.Digest(raw)
	d, err := f.policy.Decide(f.ctx, &icsv1.DecideRequest{GrantId: a.RootGrantId, TaskId: a.TaskId, TargetRef: "records", ParamsDigest: digest, AudienceDomainId: f.domain, Sensitivity: "sensitive", DataClass: "internal"})
	if err != nil {
		f.t.Fatal(err)
	}
	p := &tcsv1.EffectProposal{EffectId: "local-" + a.TaskId, TaskId: a.TaskId, CommandId: f.command.ID, TargetRef: "records", Capability: "local-tx", ParamsDigest: digest, AuthorityDigest: d.AuthorityDigest, Epoch: lease.Epoch, Sensitivity: "sensitive", DataClass: "internal"}
	for _, dim := range []string{"money", "tokens", "calls", "messages"} {
		h, err := f.control.ReserveBudget(f.ctx, &tcsv1.ReserveBudgetRequest{TaskId: a.TaskId, Dimension: dim, Purpose: "tool_call", UpperBound: "2"})
		if err != nil {
			f.t.Fatal(err)
		}
		p.Reservations = append(p.Reservations, &tcsv1.DimensionalReservation{Dimension: dim, ReservationRef: h.ReservationId, UpperBound: "1"})
	}
	return &tcsv1.ExecuteLocalEffectRequest{Admission: &tcsv1.AdmitEffectRequest{Effect: p, LeaseId: lease.LeaseId, ExpectedTaskVersion: lease.TaskVersion, SignedDecision: d.SignedDecision}, ParametersJson: raw}
}

func TestRPCLocalTransactionBoundary(t *testing.T) {
	f := setup(t)
	addRecords(f)
	var err error
	f.store, err = tcs.New(f.db, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.ics, err = ics.New(f.db, ics.Config{Domain: f.domain, KeyID: "ics", Key: f.icsKey, CommandKeys: f.cfg.CommandKeys, Operations: f.cfg.Operations})
	if err != nil {
		t.Fatal(err)
	}
	s, err := service.New(f.db, f.store, f.ics, service.Config{Domain: f.domain, TrustDomain: "test.agentnet", Certificate: f.serverCert, ClientCAs: f.roots, Operations: f.cfg.Operations})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l)
	t.Cleanup(s.Stop)
	f.address = l.Addr().String()
	conn := f.connect(f.workerCert)
	f.control = tcsv1.NewTaskControlClient(conn)
	f.policy = icsv1.NewIdentityPolicyClient(conn)
	a := f.submit()
	r := recordRequest(f, a)
	_, err = f.control.AdmitEffect(f.ctx, r.Admission)
	code(t, err, codes.InvalidArgument)
	bad := proto.Clone(r).(*tcsv1.ExecuteLocalEffectRequest)
	bad.ParametersJson = []byte(`{"key":"substituted"}`)
	_, err = f.control.ExecuteLocalEffect(f.ctx, bad)
	code(t, err, codes.PermissionDenied)
	bad = proto.Clone(r).(*tcsv1.ExecuteLocalEffectRequest)
	bad.Admission.SignedDecision = nil
	_, err = f.control.ExecuteLocalEffect(f.ctx, bad)
	code(t, err, codes.InvalidArgument)
	first, err := f.control.ExecuteLocalEffect(f.ctx, r)
	if err != nil || first.GetAdmission().GetState() != "COMMITTED" || first.GetUsage()["calls"] != "1" {
		t.Fatal(first, err)
	}
	_, err = f.control.CancelTask(f.ctx, &tcsv1.CancelTaskRequest{TaskId: a.TaskId})
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.control.ExecuteLocalEffect(f.ctx, r)
	if err != nil || again.GetAdmission().GetOutcome() != tcsv1.EffectAdmission_EXISTING || first.ResultRef != again.ResultRef {
		t.Fatal(again, err)
	}
	x, _ := canonical.Digest(first.ResultJson)
	y, _ := canonical.Digest(again.ResultJson)
	if x != y {
		t.Fatal("replay changed result")
	}
	if f.count("SELECT count(*) FROM local_records WHERE domain_id=$1", f.domain) != 1 || f.count("SELECT count(*) FROM effect_execution WHERE effect_id=$1", r.Admission.Effect.EffectId) != 0 {
		t.Fatal("incorrect business or dispatch facts")
	}
}
