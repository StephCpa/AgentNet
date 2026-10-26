package rpc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentnet/agentnet/internal/signed"
	gwv1 "github.com/agentnet/agentnet/proto/gen/anet/gw/v1"
	icsv1 "github.com/agentnet/agentnet/proto/gen/anet/ics/v1"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	"github.com/agentnet/agentnet/services/gateway"
	"github.com/agentnet/agentnet/services/ics"
	service "github.com/agentnet/agentnet/services/rpc"
	"github.com/agentnet/agentnet/services/tcs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func gatewayClient(t *testing.T, f *fixture, ttl time.Duration) gwv1.GatewayClient {
	t.Helper()
	g, err := gateway.New(f.db, f.store, f.ics, gateway.Config{Domain: f.domain, KeyID: "gateway", Key: f.gwKey, CommandTTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	server, err := service.New(f.db, f.store, f.ics, service.Config{Domain: f.domain, TrustDomain: "test.agentnet", Certificate: f.serverCert, ClientCAs: f.roots, Operations: f.cfg.Operations, Gateway: g})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.address = listener.Addr().String()
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	return gwv1.NewGatewayClient(f.connect(f.workerCert))
}
func intent(f *fixture) *gwv1.SubmitCommandRequest {
	raw, err := json.Marshal(gateway.Intent{Kind: "anet.intent/1", ID: f.command.ID, ContractID: f.command.ContractID, ContractVersion: f.command.ContractVersion, Contract: f.command.Contract, Limits: f.command.Limits, Root: f.command.Root})
	if err != nil {
		f.t.Fatal(err)
	}
	return &gwv1.SubmitCommandRequest{IntentJson: raw}
}
func storedCommand(f *fixture) tcs.Envelope {
	var raw []byte
	if err := f.db.QueryRow(f.ctx, `SELECT signed_command FROM gateway_commands WHERE domain_id=$1 AND command_id=$2`, f.domain, f.command.ID).Scan(&raw); err != nil {
		f.t.Fatal(err)
	}
	var e tcs.Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		f.t.Fatal(err)
	}
	return e
}
func TestRPCGatewayDurableSigningAndReceiptRecovery(t *testing.T) {
	f := setup(t)
	g := gatewayClient(t, f, 0)
	request := intent(f)
	first, err := g.SubmitCommand(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	saved := storedCommand(f)
	var submission tcs.Submission
	if err = signed.Decode(saved, f.cfg.CommandKeys, &submission); err != nil {
		t.Fatal(err)
	}
	if !submission.Caller.SameIdentity(f.caller) || submission.Command.Audience != f.domain || submission.Command.NotAfter.After(time.Now().Add(5*time.Minute)) {
		t.Fatal("server binding missing")
	}
	// Replace the RPC service and revoke consent after T1: the original receipt
	// remains recoverable without issuing a new permit or creating another grant.
	next := cert(t, f.ca, f.caKey, "spiffe://test.agentnet/agent-instance/"+f.caller.Instance, false, time.Hour)
	f.exec(`UPDATE agent_instances SET cred_hash=$2,credential_version=credential_version+1 WHERE instance_id=$1`, f.caller.Instance, service.Fingerprint(next.Leaf))
	f.workerCert = next
	f.exec(`UPDATE root_consents SET revoked_at=clock_timestamp(),version=version+1 WHERE consent_id=$1`, f.consent)
	g = gatewayClient(t, f, 0)
	request.IntentJson = append([]byte(" \n"), request.IntentJson...)
	again, err := g.SubmitCommand(f.ctx, request)
	if err != nil || again.GetOutcome() != tcsv1.AdmissionResult_EXISTING || again.GetTaskId() != first.TaskId || !proto.Equal(again.GetAcceptedReceipt(), first.AcceptedReceipt) || again.GetRootGrantId() != first.RootGrantId {
		t.Fatalf("lost-reply recovery: %v %v", again, err)
	}
	after := storedCommand(f)
	if string(saved.Payload) != string(after.Payload) || string(saved.Signature) != string(after.Signature) {
		t.Fatal("retry renewed signature or expiry")
	}
	if f.count(`SELECT count(*) FROM tasks WHERE domain_id=$1`, f.domain) != 1 || f.count(`SELECT count(*) FROM grants WHERE task_id=$1`, first.TaskId) != 1 {
		t.Fatal("duplicate task or grant")
	}
}
func TestRPCGatewayRejectsForgeryConflictAndBudget(t *testing.T) {
	f := setup(t)
	g := gatewayClient(t, f, 0)
	for _, field := range []string{`"caller":{"Principal":"admin"}`, `"not_after":"2099-01-01T00:00:00Z"`, `"audience":"foreign"`, `"root_permit":{}`, `"CredentialHash":"forged"`} {
		r := intent(f)
		r.IntentJson = append(r.IntentJson[:len(r.IntentJson)-1], []byte(","+field+"}")...)
		_, err := g.SubmitCommand(f.ctx, r)
		code(t, err, codes.InvalidArgument)
	}
	r := intent(f)
	r.IntentJson = append(r.IntentJson[:len(r.IntentJson)-1], []byte(`,"command_id":"duplicate"}`)...)
	_, err := g.SubmitCommand(f.ctx, r)
	code(t, err, codes.InvalidArgument)
	if f.count(`SELECT count(*) FROM gateway_commands WHERE domain_id=$1`, f.domain) != 0 {
		t.Fatal("forgery bound a command")
	}
	f.command.Limits["money"] = "11"
	_, err = g.SubmitCommand(f.ctx, intent(f))
	code(t, err, codes.ResourceExhausted)
	if f.count(`SELECT count(*) FROM tasks WHERE domain_id=$1`, f.domain) != 0 {
		t.Fatal("over-budget task accepted")
	}
	saved := storedCommand(f)
	f.command.Limits["money"] = "10"
	_, err = g.SubmitCommand(f.ctx, intent(f))
	code(t, err, codes.AlreadyExists)
	if string(saved.Payload) != string(storedCommand(f).Payload) {
		t.Fatal("conflict overwrote original binding")
	}
	// Another authenticated instance cannot reuse the command namespace or read
	// its receipt, even if it knows the original intent.
	f.command.Limits["money"] = "11"
	other := gwv1.NewGatewayClient(f.connect(f.adapterCert))
	_, err = other.SubmitCommand(f.ctx, intent(f))
	code(t, err, codes.AlreadyExists)
	if f.count(`SELECT count(*) FROM audit_log WHERE domain_id=$1 AND action='rpc.request' AND decision='deny'`, f.domain) != 9 {
		t.Fatal("missing denial audit")
	}
}
func TestRPCGatewayConcurrentFirstSubmission(t *testing.T) {
	f := setup(t)
	g := gatewayClient(t, f, 0)
	r := intent(f)
	const n = 8
	var wg sync.WaitGroup
	results := make(chan *tcsv1.AdmissionResult, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); out, err := g.SubmitCommand(f.ctx, r); results <- out; errs <- err }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var task string
	fresh := 0
	for out := range results {
		if task != "" && task != out.TaskId {
			t.Fatal("two tasks")
		}
		task = out.TaskId
		if out.Outcome != tcsv1.AdmissionResult_EXISTING {
			fresh++
		}
	}
	if fresh != 1 || f.count(`SELECT count(*) FROM gateway_commands WHERE domain_id=$1`, f.domain) != 1 || f.count(`SELECT count(*) FROM audit_log WHERE domain_id=$1 AND action='gw.command_bound'`, f.domain) != 1 {
		t.Fatal("non-atomic first binding")
	}
}
func TestRPCGatewayExpirySurvivesInboxCleanup(t *testing.T) {
	f := setup(t)
	g := gatewayClient(t, f, time.Second)
	r := intent(f)
	if _, err := g.SubmitCommand(f.ctx, r); err != nil {
		t.Fatal(err)
	}
	saved := storedCommand(f)
	f.exec(`UPDATE command_inbox SET expires_at=not_after WHERE command_id=$1`, f.command.ID)
	time.Sleep(1100 * time.Millisecond)
	f.exec(`DELETE FROM command_inbox WHERE command_id=$1 AND expires_at<clock_timestamp()`, f.command.ID)
	if f.count(`SELECT count(*) FROM command_inbox WHERE command_id=$1`, f.command.ID) != 0 {
		t.Fatal("inbox was not cleaned")
	}
	g = gatewayClient(t, f, 5*time.Minute)
	_, err := g.SubmitCommand(f.ctx, r)
	code(t, err, codes.FailedPrecondition)
	if string(saved.Payload) != string(storedCommand(f).Payload) || f.count(`SELECT count(*) FROM tasks WHERE domain_id=$1`, f.domain) != 1 {
		t.Fatal("expired command re-signed or duplicated")
	}
}
func auditConstraint(t *testing.T, f *fixture, action string) func() {
	t.Helper()
	name := "fail_" + strings.ReplaceAll(f.domain, "-", "")
	f.exec(fmt.Sprintf(`ALTER TABLE audit_log ADD CONSTRAINT %s CHECK (domain_id <> '%s' OR action <> '%s')`, name, f.domain, action))
	active := true
	drop := func() {
		if active {
			f.exec(`ALTER TABLE audit_log DROP CONSTRAINT ` + name)
			active = false
		}
	}
	t.Cleanup(drop)
	return drop
}

func TestRPCGatewayAcceptanceAuditRollback(t *testing.T) {
	f := setup(t)
	g := gatewayClient(t, f, 0)
	drop := auditConstraint(t, f, "tcs.command_accepted")
	r := intent(f)
	_, err := g.SubmitCommand(f.ctx, r)
	code(t, err, codes.Internal)
	if f.count(`SELECT count(*) FROM tasks WHERE domain_id=$1`, f.domain) != 0 || f.count(`SELECT count(*) FROM receipts WHERE issuer_domain_id=$1`, f.domain) != 0 || f.count(`SELECT count(*) FROM command_inbox WHERE audience_domain_id=$1`, f.domain) != 0 {
		t.Fatal("T1 survived acceptance audit failure")
	}
	if f.count(`SELECT count(*) FROM gateway_commands WHERE domain_id=$1 AND root_permit IS NOT NULL`, f.domain) != 1 {
		t.Fatal("recovery binding was lost")
	}
	drop()
	out, err := g.SubmitCommand(f.ctx, r)
	if err != nil || out.GetTaskId() == "" {
		t.Fatalf("recovery after audit failure: %v %v", out, err)
	}
	if f.count(`SELECT count(*) FROM audit_log WHERE domain_id=$1 AND action='tcs.command_accepted' AND task_id=$2`, f.domain, out.TaskId) != 1 {
		t.Fatal("missing atomic acceptance audit")
	}
}
func TestRPCGatewayAuditRollback(t *testing.T) {
	f := setup(t)
	g := gatewayClient(t, f, 0)
	auditConstraint(t, f, "gw.command_bound")
	_, err := g.SubmitCommand(f.ctx, intent(f))
	code(t, err, codes.Internal)
	if f.count(`SELECT count(*) FROM gateway_commands WHERE domain_id=$1`, f.domain) != 0 || f.count(`SELECT count(*) FROM tasks WHERE domain_id=$1`, f.domain) != 0 {
		t.Fatal("binding survived failed atomic audit")
	}
	if f.count(`SELECT count(*) FROM audit_log WHERE domain_id=$1 AND action='rpc.request' AND decision='unknown'`, f.domain) != 1 {
		t.Fatal("internal error not audited as unknown")
	}
}
func TestRPCDenialAuditFailure(t *testing.T) {
	f := setup(t)
	g := gatewayClient(t, f, 0)
	auditConstraint(t, f, "rpc.request")
	_, err := g.SubmitCommand(f.ctx, &gwv1.SubmitCommandRequest{IntentJson: []byte(`{"secret":"do-not-log"}`)})
	code(t, err, codes.Unavailable)
	if f.count(`SELECT count(*) FROM gateway_commands WHERE domain_id=$1`, f.domain) != 0 {
		t.Fatal("malformed request reached signer")
	}
}

func TestRPCDenialAuditIdentityScope(t *testing.T) {
	f := setup(t)
	// Successful TLS authentication does not establish an AgentNet identity.
	unknown := cert(t, f.ca, f.caKey, "spiffe://test.agentnet/agent-instance/unknown", false, time.Hour)
	control := tcsv1.NewTaskControlClient(f.connect(unknown))
	_, err := control.GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: "untrusted-body-marker"})
	code(t, err, codes.Unauthenticated)
	if f.count(`SELECT count(*) FROM audit_log WHERE domain_id=$1 AND action='rpc.request' AND principal_id IS NULL AND agent_instance_id IS NULL AND reason_code='Unauthenticated'`, f.domain) != 1 {
		t.Fatal("failed authentication acquired trusted identity in audit")
	}
	stream, err := f.control.ListUnknownEffects(f.ctx, &tcsv1.ListUnknownEffectsRequest{})
	if err == nil {
		_, err = stream.Recv()
	}
	code(t, err, codes.PermissionDenied)
	if f.count(`SELECT count(*) FROM audit_log WHERE domain_id=$1 AND action='rpc.request' AND principal_id=$2 AND agent_instance_id=$3 AND reason_code='PermissionDenied'`, f.domain, f.caller.Principal, f.caller.Instance) != 1 {
		t.Fatal("missing authenticated role denial")
	}
	if f.count(`SELECT count(*) FROM audit_log WHERE domain_id=$1 AND row_to_json(audit_log)::text LIKE '%untrusted-body-marker%'`, f.domain) != 0 {
		t.Fatal("untrusted body leaked into audit")
	}
}

func TestRPCGatewayCommittedButResponseUnavailable(t *testing.T) {
	f := setup(t)
	g := gatewayClient(t, f, 0)
	// Deterministic database fault between T1's last authority check and the
	// response read: the accepted task survives, but the caller is now revoked.
	name := "revoke_" + strings.ReplaceAll(f.domain, "-", "")
	f.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$ BEGIN IF NEW.issuer_domain_id='%s' AND NEW.kind='ACCEPTED' THEN UPDATE agent_instances SET revoked_at=clock_timestamp(),credential_version=credential_version+1 WHERE instance_id='%s'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql`, name, f.domain, f.caller.Instance))
	f.exec(fmt.Sprintf(`CREATE TRIGGER %s AFTER INSERT ON receipts FOR EACH ROW EXECUTE FUNCTION %s()`, name, name))
	t.Cleanup(func() { f.exec(`DROP TRIGGER ` + name + ` ON receipts`); f.exec(`DROP FUNCTION ` + name + `()`) })
	_, err := g.SubmitCommand(f.ctx, intent(f))
	code(t, err, codes.Unavailable)
	if f.count(`SELECT count(*) FROM tasks WHERE domain_id=$1`, f.domain) != 1 || f.count(`SELECT count(*) FROM receipts WHERE issuer_domain_id=$1 AND kind='ACCEPTED'`, f.domain) != 1 {
		t.Fatal("committed acceptance was lost")
	}
	if f.count(`SELECT count(*) FROM audit_log WHERE domain_id=$1 AND action='rpc.request' AND decision='unknown' AND reason_code='Unavailable'`, f.domain) != 1 {
		t.Fatal("committed operation reported as definite denial")
	}
}
func TestRPCGatewayPendingPermitAndCancelledRequest(t *testing.T) {
	f := setup(t)
	var err error
	f.ics, err = ics.New(f.db, ics.Config{Domain: f.domain, KeyID: "ics", Key: f.icsKey, CommandKeys: f.cfg.CommandKeys, Operations: f.cfg.Operations, PermitTTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	g := gatewayClient(t, f, 0)
	r := intent(f)
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "anet.command:"+f.domain+":"+f.command.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := g.SubmitCommand(ctx, r); done <- err }()
	waitForLock(t, f, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))")
	cancel()
	if status.Code(<-done) != codes.Canceled {
		t.Fatal("call not cancelled")
	}
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.count(`SELECT count(*) FROM audit_log WHERE domain_id=$1 AND action='rpc.request' AND decision='unknown'`, f.domain) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("cancelled RPC omitted audit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f.count(`SELECT count(*) FROM tasks WHERE domain_id=$1`, f.domain) != 0 {
		t.Fatal("cancelled pending request committed")
	}
	time.Sleep(1100 * time.Millisecond)
	saved := storedCommand(f)
	g = gatewayClient(t, f, 0)
	out, err := g.SubmitCommand(f.ctx, r)
	if err != nil || out.GetTaskId() == "" {
		t.Fatalf("expired pending permit did not reauthorize: %v %v", out, err)
	}
	if string(saved.Payload) != string(storedCommand(f).Payload) {
		t.Fatal("permit refresh changed command")
	}
}

func TestRPCCertificateExpiryDuringLockWait(t *testing.T) {
	for _, stage := range []string{"gateway", "permit", "admission"} {
		t.Run(stage, func(t *testing.T) {
			f := setup(t)
			var proposal *tcsv1.AdmitEffectRequest
			var accepted *tcsv1.AdmissionResult
			if stage == "admission" {
				accepted = f.submit()
				proposal, _ = f.proposal(accepted)
			}
			short := cert(t, f.ca, f.caKey, "spiffe://test.agentnet/agent-instance/"+f.caller.Instance, false, 2*time.Second)
			f.exec(`UPDATE agent_instances SET cred_hash=$2,credential_version=credential_version+1 WHERE instance_id=$1`, f.caller.Instance, service.Fingerprint(short.Leaf))
			f.workerCert = short
			tx, err := f.db.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(f.ctx)
			done := make(chan error, 1)
			var prefix string
			switch stage {
			case "gateway":
				if _, err = tx.Exec(f.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "anet.gateway:"+f.domain+":"+f.command.ID); err != nil {
					t.Fatal(err)
				}
				g := gatewayClient(t, f, 0)
				r := intent(f)
				go func() { _, e := g.SubmitCommand(f.ctx, r); done <- e }()
				prefix = "SELECT pg_advisory_xact_lock(hashtextextended($1,0))"
			case "permit":
				if _, err = tx.Exec(f.ctx, `SELECT consent_id FROM root_consents WHERE consent_id=$1 FOR UPDATE`, f.consent); err != nil {
					t.Fatal(err)
				}
				p := icsv1.NewIdentityPolicyClient(f.connect(short))
				r := &icsv1.IssueRootPermitRequest{SignedCommand: f.commandEnvelope(f.command)}
				go func() { _, e := p.IssueRootPermit(f.ctx, r); done <- e }()
				prefix = "SELECT rights,task_limits,%"
			case "admission":
				if _, err = tx.Exec(f.ctx, `SELECT account_id FROM budget_accounts WHERE task_id=$1 FOR UPDATE`, accepted.TaskId); err != nil {
					t.Fatal(err)
				}
				c := tcsv1.NewTaskControlClient(f.connect(short))
				go func() { _, e := c.AdmitEffect(f.ctx, proposal); done <- e }()
				prefix = "SELECT account_id FROM budget_accounts WHERE account_id=%"
			}
			waitForLock(t, f, prefix)
			time.Sleep(time.Until(short.Leaf.NotAfter) + 50*time.Millisecond)
			if err = tx.Commit(f.ctx); err != nil {
				t.Fatal(err)
			}
			code(t, <-done, codes.PermissionDenied)
			if f.count(`SELECT count(*) FROM gateway_commands WHERE domain_id=$1`, f.domain) != 0 || f.count(`SELECT count(*) FROM effects WHERE task_id IN(SELECT task_id FROM tasks WHERE domain_id=$1)`, f.domain) != 0 {
				t.Fatal("expired credential produced a new binding/effect")
			}
		})
	}
}
