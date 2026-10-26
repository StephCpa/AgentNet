package rpc_test

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	icsv1 "github.com/agentnet/agentnet/proto/gen/anet/ics/v1"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// Provision authoritative imported facts only. This is not a CreateSubtask API
// or budget-transfer test; those transactions remain the next implementation step.
func grantChain(t *testing.T, f *fixture) []*tcsv1.AdmissionResult {
	t.Helper()
	chain := []*tcsv1.AdmissionResult{}
	for i := 0; i < 3; i++ {
		f.command.ID = fmt.Sprintf("chain-%s-%d", f.domain, i)
		chain = append(chain, f.submit())
	}
	rights := f.command.Root.Rights
	rights.DepthLeft = 2
	rights.DelegationScope = []string{"compute"}
	raw, _ := json.Marshal(rights)
	f.exec("UPDATE root_consents SET rights=$2::jsonb,version=version+1 WHERE consent_id=$1", f.consent, string(raw))
	for i, a := range chain {
		rights.DepthLeft = 2 - i
		if i == 2 {
			rights.DelegationScope = nil
		}
		raw, _ = json.Marshal(rights)
		var parentGrant, parentTask any
		if i > 0 {
			parentGrant = chain[i-1].RootGrantId
			parentTask = chain[i-1].TaskId
		}
		f.exec("UPDATE grants SET rights=$2::jsonb,root_grant_id=$3,parent_grant_id=$4,consent_version=consent_version+1 WHERE grant_id=$1", a.RootGrantId, string(raw), chain[0].RootGrantId, parentGrant)
		f.exec("UPDATE tasks SET task_version=task_version+1,root_task_id=$2,parent_task_id=$3,delegation_depth=$4 WHERE task_id=$1", a.TaskId, chain[0].TaskId, parentTask, i)
	}
	return chain
}
func chainDecision(f *fixture, a *tcsv1.AdmissionResult) error {
	_, err := f.policy.Decide(f.ctx, &icsv1.DecideRequest{TaskId: a.TaskId, GrantId: a.RootGrantId, TargetRef: "bank", ParamsDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AudienceDomainId: f.domain, Sensitivity: "sensitive", DataClass: "internal"})
	return err
}
func TestRPCGrantChainAdmissionAndAncestorRevocation(t *testing.T) {
	f := setup(t)
	chain := grantChain(t, f)
	leaf := chain[2]
	p, _ := f.proposal(leaf)
	first, err := f.control.AdmitEffect(f.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.policy.Revoke(f.ctx, &icsv1.RevokeRequest{Target: &icsv1.RevokeRequest_GrantId{GrantId: chain[1].RootGrantId}, Reason: "test ancestor revocation"})
	if err != nil {
		t.Fatal(err)
	}
	code(t, chainDecision(f, leaf), codes.PermissionDenied)
	again, err := f.control.AdmitEffect(f.ctx, p)
	if err != nil || again.State != first.State || again.Outcome != tcsv1.EffectAdmission_EXISTING {
		t.Fatal("committed admission recovery lost", again, err)
	}
	snapshot, err := f.control.GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: leaf.TaskId})
	if err != nil {
		t.Fatal(err)
	}
	p = proto.Clone(p).(*tcsv1.AdmitEffectRequest)
	p.Effect.EffectId += "-new"
	p.ExpectedTaskVersion = snapshot.TaskVersion
	_, err = f.control.AdmitEffect(f.ctx, p)
	code(t, err, codes.PermissionDenied)
	if f.count("SELECT count(*) FROM effects WHERE task_id=$1", leaf.TaskId) != 1 {
		t.Fatal("revoked branch admitted new work")
	}
}
func TestRPCGrantChainRejectsMalformedPaths(t *testing.T) {
	changes := map[string]string{
		"cycle":          "UPDATE grants SET parent_grant_id=grant_id WHERE grant_id=$1",
		"wrong_root":     "UPDATE grants SET root_grant_id=grant_id WHERE grant_id=$2",
		"task_edge":      "UPDATE tasks SET task_version=task_version+1,parent_task_id=NULL WHERE task_id=(SELECT task_id FROM grants WHERE grant_id=$2)",
		"depth":          "UPDATE grants SET rights=jsonb_set(rights,'{depth_left}','2') WHERE grant_id=$2",
		"scope":          "UPDATE grants SET rights=jsonb_set(rights,'{delegation_scope}','[\"admin\"]') WHERE grant_id=$2",
		"consent":        "UPDATE grants SET consent_version=consent_version+1 WHERE grant_id=$2",
		"parent_revoked": "UPDATE grants SET revoked_at=clock_timestamp() WHERE grant_id=$1",
		"parent_expired": "UPDATE grants SET expires_at=clock_timestamp()-interval '1 second' WHERE grant_id=$1",
		"unknown":        "UPDATE grants SET rights=rights||'{\"unimplemented_constraint\":true}'::jsonb WHERE grant_id=$2",
	}
	for name, query := range changes {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			c := grantChain(t, f)
			f.exec(query+" AND $1::text<>'' AND $2::text<>''", c[0].RootGrantId, c[1].RootGrantId)
			code(t, chainDecision(f, c[2]), codes.PermissionDenied)
		})
	}
}
func TestRPCGrantChainDigestIncludesParent(t *testing.T) {
	f := setup(t)
	c := grantChain(t, f)
	p, _ := f.proposal(c[2])
	f.exec("UPDATE grants SET policy_version='recipient-policy/2' WHERE grant_id=$1", c[1].RootGrantId)
	_, err := f.control.AdmitEffect(f.ctx, p)
	code(t, err, codes.PermissionDenied)
	if err = chainDecision(f, c[2]); err != nil {
		t.Fatal("valid changed chain could not obtain fresh decision", err)
	}
	if f.count("SELECT count(*) FROM effects WHERE task_id=$1", c[2].TaskId) != 0 {
		t.Fatal("old chain digest admitted")
	}
}
func TestRPCGrantChainReloadAfterLockWait(t *testing.T) {
	for _, change := range []string{"revoke", "reparent"} {
		t.Run(change, func(t *testing.T) {
			f := setup(t)
			c := grantChain(t, f)
			ids := []string{c[0].RootGrantId, c[1].RootGrantId, c[2].RootGrantId}
			sort.Strings(ids)
			tx, err := f.db.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(f.ctx)
			if _, err = tx.Exec(f.ctx, "SELECT grant_id FROM grants WHERE grant_id=$1 FOR UPDATE", ids[0]); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- chainDecision(f, c[2]) }()
			waitForLock(t, f, "SELECT grant_id FROM grants WHERE grant_id=$1 FOR SHARE")
			query := "UPDATE grants SET revoked_at=clock_timestamp() WHERE grant_id=$1"
			if change == "reparent" {
				query = "UPDATE grants SET parent_grant_id=grant_id WHERE grant_id=$1"
			}
			if _, err = tx.Exec(f.ctx, query, ids[0]); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(f.ctx); err != nil {
				t.Fatal(err)
			}
			code(t, <-done, codes.PermissionDenied)
		})
	}
}
func TestRPCGrantChainExpiryDuringAccountWait(t *testing.T) {
	f := setup(t)
	c := grantChain(t, f)
	var end time.Time
	if err := f.db.QueryRow(f.ctx, "SELECT clock_timestamp()+interval '2 seconds'").Scan(&end); err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE grants SET expires_at=$2 WHERE root_grant_id=$1", c[0].RootGrantId, end)
	p, _ := f.proposal(c[2])
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, "SELECT account_id FROM budget_accounts WHERE task_id=$1 FOR UPDATE", c[2].TaskId); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.control.AdmitEffect(f.ctx, p); done <- err }()
	waitForLock(t, f, "SELECT account_id FROM budget_accounts WHERE account_id=%")
	time.Sleep(time.Until(end) + 20*time.Millisecond)
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	code(t, <-done, codes.PermissionDenied)
	if f.count("SELECT count(*) FROM effects WHERE task_id=$1", c[2].TaskId) != 0 {
		t.Fatal("expired chain admitted after account wait")
	}
}
