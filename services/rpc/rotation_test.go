package rpc_test

import (
	"testing"
	"time"

	icsv1 "github.com/agentnet/agentnet/proto/gen/anet/ics/v1"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	service "github.com/agentnet/agentnet/services/rpc"
	"google.golang.org/grpc/codes"
)

func waitForLock(t *testing.T, f *fixture, prefix string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.count(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE $1`, prefix) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("request never waited on expected row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func rotateCredential(t *testing.T, f *fixture) {
	t.Helper()
	next := cert(t, f.ca, f.caKey, "spiffe://test.agentnet/agent-instance/"+f.caller.Instance, false, time.Hour)
	f.exec(`UPDATE agent_instances SET cred_hash=$2,credential_version=credential_version+1 WHERE instance_id=$1`, f.caller.Instance, service.Fingerprint(next.Leaf))
}
func TestRPCCredentialRotationDuringAdmissionWait(t *testing.T) {
	f := setup(t)
	a := f.submit()
	proposal, _ := f.proposal(a)
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, `SELECT account_id FROM budget_accounts WHERE task_id=$1 FOR UPDATE`, a.TaskId); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.control.AdmitEffect(f.ctx, proposal); done <- err }()
	waitForLock(t, f, "SELECT account_id FROM budget_accounts WHERE account_id=%")
	rotateCredential(t, f)
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	code(t, <-done, codes.PermissionDenied)
	if f.count(`SELECT count(*) FROM effects WHERE task_id=$1`, a.TaskId) != 0 {
		t.Fatal("old credential admitted after rotation during lock wait")
	}
	_, err = f.control.GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: a.TaskId})
	code(t, err, codes.Unauthenticated)
}
func TestRPCCredentialRotationDuringPermitWait(t *testing.T) {
	f := setup(t)
	request := &icsv1.IssueRootPermitRequest{SignedCommand: f.commandEnvelope(f.command)}
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, `SELECT consent_id FROM root_consents WHERE consent_id=$1 FOR UPDATE`, f.consent); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.policy.IssueRootPermit(f.ctx, request); done <- err }()
	waitForLock(t, f, "SELECT rights,task_limits,%")
	rotateCredential(t, f)
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	code(t, <-done, codes.PermissionDenied)
}
