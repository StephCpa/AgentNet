package rpc_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentnet/agentnet/internal/signed"
	icsv1 "github.com/agentnet/agentnet/proto/gen/anet/ics/v1"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	"github.com/agentnet/agentnet/services/tcs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func subContract(t *testing.T, raw []byte, depth int, end time.Time) []byte {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	b["task_type"] = "compute"
	b["deadline"] = end
	b["delegation_policy"] = map[string]any{"allow": depth > 0, "max_depth": 2, "scope": []string{"compute"}, "max_concurrent_subtasks": 2}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func delegableRoot(t *testing.T, f *fixture) *tcsv1.AdmissionResult {
	t.Helper()
	f.command.Root.Rights.DepthLeft = 2
	f.command.Root.Rights.DelegationScope = []string{"compute"}
	raw, _ := json.Marshal(f.command.Root.Rights)
	f.exec("UPDATE root_consents SET rights=$2::jsonb,version=version+1 WHERE consent_id=$1", f.consent, string(raw))
	f.command.Contract = subContract(t, f.command.Contract, 2, time.Now().UTC().Add(time.Hour))
	return f.submit()
}
func subCommand(t *testing.T, f *fixture, parent *tcsv1.AdmissionResult, recipient tcs.Caller, depth int, value string) tcs.SubtaskCommand {
	t.Helper()
	lease, err := f.control.AcquireLease(f.ctx, &tcsv1.AcquireLeaseRequest{TaskId: parent.TaskId})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	end := now.Add(20 * time.Minute)
	r := f.command.Root.Rights
	r.DepthLeft = depth
	if depth == 0 {
		r.DelegationScope = nil
	}
	policy := fmt.Sprintf("recipient-%s-%s-%d", parent.TaskId, recipient.Instance, now.UnixNano())
	raw, _ := json.Marshal(r)
	f.exec(`INSERT INTO recipient_policies(policy_id,domain_id,agent_id,instance_id,version,rights,not_before,expires_at) VALUES($1,$2,$3,$4,'recipient/1',$5::jsonb,$6,$7)`, policy, f.domain, recipient.Agent, recipient.Instance, string(raw), now.Add(-time.Minute), end.Add(time.Minute))
	cmd := tcs.SubtaskCommand{ID: fmt.Sprintf("child-%s-%d", parent.TaskId, now.UnixNano()), Audience: f.domain, NotAfter: now.Add(time.Minute), Parent: parent.TaskId, Grant: parent.RootGrantId, Lease: lease.LeaseId, Epoch: lease.Epoch, ExpectedVersion: lease.TaskVersion, RecipientPolicy: policy, RecipientAgent: recipient.Agent, RecipientInstance: recipient.Instance, Contract: subContract(t, f.command.Contract, depth, end), Rights: r, NotBefore: now, ExpiresAt: end, Allocations: map[string]tcs.SubtaskAllocation{}}
	for _, dim := range []string{"calls", "messages", "money", "tokens"} {
		ref := ""
		if value != "0" {
			h, err := f.control.ReserveBudget(f.ctx, &tcsv1.ReserveBudgetRequest{TaskId: parent.TaskId, Dimension: dim, Purpose: "subtask", UpperBound: value})
			if err != nil {
				t.Fatal(err)
			}
			ref = h.ReservationId
		}
		cmd.Allocations[dim] = tcs.SubtaskAllocation{Amount: value, Reservation: ref}
	}
	return cmd
}
func subRequest(t *testing.T, f *fixture, c tcs.SubtaskCommand) *tcsv1.CreateSubtaskRequest {
	t.Helper()
	e, err := signed.Encode("gateway", f.gwKey, tcs.SubtaskSubmission{Kind: "anet.subtask/1", Caller: f.caller, Command: c})
	if err != nil {
		t.Fatal(err)
	}
	return &tcsv1.CreateSubtaskRequest{SignedCommand: pb(e)}
}
func recipientFixture(f *fixture) *fixture {
	b := *f
	b.caller = f.adapter
	b.conn = f.connect(f.adapterCert)
	b.control = tcsv1.NewTaskControlClient(b.conn)
	b.policy = icsv1.NewIdentityPolicyClient(b.conn)
	return &b
}
func createChild(t *testing.T, f *fixture) (*tcsv1.AdmissionResult, *fixture) {
	t.Helper()
	parent := delegableRoot(t, f)
	cmd := subCommand(t, f, parent, f.adapter, 0, "5")
	child, err := f.control.CreateSubtask(f.ctx, subRequest(t, f, cmd))
	if err != nil {
		t.Fatal(err)
	}
	return child, recipientFixture(f)
}
func TestRPCSubtaskCrossSubjectAndBudget(t *testing.T) {
	f := setup(t)
	f.command.Root.Rights.DataClasses = []string{"internal", "public"}
	parent := delegableRoot(t, f)
	cmd := subCommand(t, f, parent, f.adapter, 0, "5")
	// A new immutable recipient policy narrows a valid parent proposal.
	f.exec(`INSERT INTO recipient_policies(policy_id,domain_id,agent_id,instance_id,version,rights,not_before,expires_at) SELECT policy_id||'-narrow',domain_id,agent_id,instance_id,version,jsonb_set(rights,'{data_classes}','["internal"]'),not_before,expires_at FROM recipient_policies WHERE policy_id=$1`, cmd.RecipientPolicy)
	cmd.RecipientPolicy += "-narrow"
	if _, err := f.db.Exec(f.ctx, "UPDATE recipient_policies SET version='rewritten' WHERE policy_id=$1", cmd.RecipientPolicy); err == nil {
		t.Fatal("mutable recipient ceiling")
	}
	req := subRequest(t, f, cmd)
	child, err := f.control.CreateSubtask(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.control.CreateSubtask(f.ctx, req)
	if err != nil || again.Outcome != tcsv1.AdmissionResult_EXISTING || !proto.Equal(child.AcceptedReceipt, again.AcceptedReceipt) || again.RootGrantId != child.RootGrantId {
		t.Fatal(again, err)
	}
	if f.count("SELECT count(*) FROM tasks WHERE task_id=$1 AND owner_principal_id=$2 AND assignee_agent_id=$3 AND parent_task_id=$4", child.TaskId, f.caller.Principal, f.adapter.Agent, parent.TaskId) != 1 {
		t.Fatal("subject/actor/tree binding lost")
	}
	if f.count(`SELECT count(*) FROM grants WHERE grant_id=$1 AND rights->'data_classes'='["internal"]'::jsonb`, child.RootGrantId) != 1 {
		t.Fatal("recipient ceiling not intersected")
	}
	if f.count(`SELECT count(*) FROM budget_accounts a WHERE a.task_id=$1 AND delegated=5 AND settled=0 AND NOT EXISTS(SELECT 1 FROM budget_reservations r WHERE r.account_id=a.account_id AND state='HELD')`, parent.TaskId) != 4 {
		t.Fatal("H to D not atomic or replayed")
	}
	if f.count(`SELECT count(*) FROM delegation_allocations WHERE child_task_id=$1 AND amount=5`, child.TaskId) != 4 {
		t.Fatal("allocation linkage missing")
	}
	_, err = f.control.ReserveBudget(f.ctx, &tcsv1.ReserveBudgetRequest{TaskId: parent.TaskId, Dimension: "money", Purpose: "tool_call", UpperBound: "6"})
	code(t, err, codes.ResourceExhausted)
	b := recipientFixture(f)
	_, err = b.control.GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: parent.TaskId})
	code(t, err, codes.PermissionDenied)
	p, d := b.proposal(child)
	if d.SubjectPrincipalId != f.caller.Principal || d.ActorAgentId != f.adapter.Agent || d.ActorInstanceId != f.adapter.Instance {
		t.Fatal("subject replaced by recipient principal", d)
	}
	if _, err = b.control.AdmitEffect(f.ctx, p); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM budget_reservations WHERE holder_task_id=$1 AND effect_id=$2 AND state='HELD'", child.TaskId, p.Effect.EffectId) != 4 {
		t.Fatal("child holds missing")
	}
	// A parent owner cannot impersonate the delegated leaf actor.
	_, err = f.policy.Decide(f.ctx, &icsv1.DecideRequest{TaskId: child.TaskId, GrantId: child.RootGrantId, TargetRef: "bank", ParamsDigest: p.Effect.ParamsDigest, AudienceDomainId: f.domain, Sensitivity: "sensitive", DataClass: "internal"})
	code(t, err, codes.PermissionDenied)
	cmd.RecipientAgent = f.caller.Agent
	_, err = f.control.CreateSubtask(f.ctx, subRequest(t, f, cmd))
	code(t, err, codes.AlreadyExists)
}
func TestRPCSubtaskRejectsGuardViolations(t *testing.T) {
	for _, name := range []string{"epoch", "lease", "lease-holder", "version", "scope", "rights", "depth", "budget", "purpose", "recipient", "recipient-ceiling", "policy-revoked", "recipient-revoked", "parent-revoked", "expired", "deadline", "malformed-concurrency-limit"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			parent := delegableRoot(t, f)
			cmd := subCommand(t, f, parent, f.adapter, 0, "5")
			switch name {
			case "epoch":
				cmd.Epoch++
			case "lease":
				cmd.Lease = "someone-elses-lease"
			case "lease-holder":
				f.exec("UPDATE leases SET worker_instance_id=$2 WHERE lease_id=$1", cmd.Lease, f.adapter.Instance)
			case "version":
				cmd.ExpectedVersion++
			case "scope":
				var body map[string]any
				json.Unmarshal(cmd.Contract, &body)
				body["task_type"] = "admin"
				cmd.Contract, _ = json.Marshal(body)
			case "rights":
				cmd.Rights.Actions = []string{"admin"}
			case "depth":
				cmd.Rights.DepthLeft = 2
				cmd.Rights.DelegationScope = []string{"compute"}
			case "budget":
				a := cmd.Allocations["money"]
				a.Amount = "6"
				cmd.Allocations["money"] = a
			case "purpose":
				f.exec("UPDATE budget_reservations SET purpose='tool_call' WHERE reservation_id=$1", cmd.Allocations["money"].Reservation)
			case "recipient":
				cmd.RecipientInstance = f.caller.Instance
			case "recipient-ceiling":
				f.exec(`INSERT INTO recipient_policies(policy_id,domain_id,agent_id,instance_id,version,rights,not_before,expires_at) SELECT policy_id||'-other',domain_id,agent_id,instance_id,version,jsonb_set(rights,'{actions}','["read"]'),not_before,expires_at FROM recipient_policies WHERE policy_id=$1`, cmd.RecipientPolicy)
				cmd.RecipientPolicy += "-other"
			case "policy-revoked":
				f.exec("UPDATE recipient_policies SET revoked_at=clock_timestamp() WHERE policy_id=$1", cmd.RecipientPolicy)
			case "recipient-revoked":
				f.exec("UPDATE agent_instances SET revoked_at=clock_timestamp() WHERE instance_id=$1", f.adapter.Instance)
			case "parent-revoked":
				f.exec("UPDATE grants SET revoked_at=clock_timestamp() WHERE grant_id=$1", parent.RootGrantId)
			case "expired":
				cmd.NotAfter = time.Now().Add(-time.Second)
			case "deadline":
				cmd.Contract = subContract(t, cmd.Contract, 0, time.Now().Add(2*time.Hour))
			case "malformed-concurrency-limit":
				var body map[string]any
				json.Unmarshal(cmd.Contract, &body)
				body["delegation_policy"].(map[string]any)["max_concurrent_subtasks"] = -1
				cmd.Contract, _ = json.Marshal(body)
			}
			_, err := f.control.CreateSubtask(f.ctx, subRequest(t, f, cmd))
			if err == nil {
				t.Fatal("guard bypass", name)
			}
			if f.count("SELECT count(*) FROM tasks WHERE parent_task_id=$1", parent.TaskId) != 0 || f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", parent.TaskId) != 0 || f.count("SELECT count(*) FROM budget_reservations WHERE holder_task_id=$1 AND state='HELD'", parent.TaskId) != 4 {
				t.Fatal("partial delegation", name, err)
			}
		})
	}
}
func TestRPCSubtaskRollbackAndConcurrentReplay(t *testing.T) {
	f := setup(t)
	parent := delegableRoot(t, f)
	cmd := subCommand(t, f, parent, f.adapter, 0, "5")
	req := subRequest(t, f, cmd)
	trigger := "fail_sub_" + strings.ReplaceAll(f.domain, "-", "")
	f.exec(`CREATE FUNCTION ` + trigger + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.child_task_id IN (SELECT task_id FROM tasks WHERE parent_task_id='` + parent.TaskId + `') THEN RAISE EXCEPTION 'test late allocation failure'; END IF; RETURN NEW; END $$`)
	f.exec(`CREATE TRIGGER ` + trigger + ` BEFORE INSERT ON delegation_allocations FOR EACH ROW EXECUTE FUNCTION ` + trigger + `() `)
	_, err := f.control.CreateSubtask(f.ctx, req)
	if err == nil {
		t.Fatal("injected failure ignored")
	}
	f.exec(`DROP TRIGGER ` + trigger + ` ON delegation_allocations`)
	f.exec(`DROP FUNCTION ` + trigger + `() `)
	if f.count("SELECT count(*) FROM tasks WHERE parent_task_id=$1", parent.TaskId) != 0 || f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", parent.TaskId) != 0 {
		t.Fatal("failed create leaked state")
	}
	var wg sync.WaitGroup
	results := make(chan *tcsv1.AdmissionResult, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a, e := f.control.CreateSubtask(f.ctx, req); results <- a; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	fresh, existing := 0, 0
	for a := range results {
		if a.Outcome == tcsv1.AdmissionResult_NEW {
			fresh++
		} else {
			existing++
		}
	}
	if fresh != 1 || existing != 1 || f.count("SELECT count(*) FROM tasks WHERE parent_task_id=$1", parent.TaskId) != 1 {
		t.Fatal("duplicate child", fresh, existing)
	}
}
func TestRPCSubtaskPolicyAndExpiryAfterLockWait(t *testing.T) {
	for _, mode := range []string{"policy", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			parent := delegableRoot(t, f)
			cmd := subCommand(t, f, parent, f.adapter, 0, "5")
			if mode == "expiry" {
				cmd.NotAfter = time.Now().UTC().Add(1500 * time.Millisecond)
			}
			tx, err := f.db.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(f.ctx)
			if mode == "policy" {
				_, err = tx.Exec(f.ctx, "SELECT policy_id FROM recipient_policies WHERE policy_id=$1 FOR UPDATE", cmd.RecipientPolicy)
			} else {
				_, err = tx.Exec(f.ctx, "SELECT account_id FROM budget_accounts WHERE task_id=$1 FOR UPDATE", parent.TaskId)
			}
			if err != nil {
				t.Fatal(err)
			}
			req := subRequest(t, f, cmd)
			done := make(chan error, 1)
			go func() { _, err := f.control.CreateSubtask(f.ctx, req); done <- err }()
			if mode == "policy" {
				waitForLock(t, f, "SELECT p.rights,p.not_before,%")
				_, err = tx.Exec(f.ctx, "UPDATE recipient_policies SET revoked_at=clock_timestamp() WHERE policy_id=$1", cmd.RecipientPolicy)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				waitForLock(t, f, "SELECT account_id,dimension FROM budget_accounts%")
				time.Sleep(time.Until(cmd.NotAfter) + 20*time.Millisecond)
			}
			if err = tx.Commit(f.ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-done; err == nil {
				t.Fatal("stale creation passed")
			}
			if f.count("SELECT count(*) FROM tasks WHERE parent_task_id=$1", parent.TaskId) != 0 {
				t.Fatal("child after stale policy/expiry")
			}
		})
	}
}
func TestRPCSubtaskNestedAndZeroBudget(t *testing.T) {
	f := setup(t)
	root := delegableRoot(t, f)
	cmd := subCommand(t, f, root, f.adapter, 1, "5")
	child, err := f.control.CreateSubtask(f.ctx, subRequest(t, f, cmd))
	if err != nil {
		t.Fatal(err)
	}
	b := recipientFixture(f)
	nested := subCommand(t, b, child, f.caller, 0, "0")
	// A nested grant must fit the actual parent's shorter interval.
	nested.ExpiresAt = cmd.ExpiresAt.Add(-time.Second)
	nested.Contract = subContract(t, nested.Contract, 0, nested.ExpiresAt)
	leaf, err := b.control.CreateSubtask(b.ctx, subRequest(t, b, nested))
	if err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM tasks WHERE task_id=$1 AND root_task_id=$2 AND parent_task_id=$3 AND delegation_depth=2 AND owner_principal_id=$4", leaf.TaskId, root.TaskId, child.TaskId, f.caller.Principal) != 1 {
		t.Fatal("nested tree wrong")
	}
	if f.count("SELECT count(*) FROM budget_accounts WHERE task_id=$1 AND budget_limit=0 AND parent_account_id IS NOT NULL", leaf.TaskId) != 4 {
		t.Fatal("zero dimension missing")
	}
	_, err = f.control.ReserveBudget(f.ctx, &tcsv1.ReserveBudgetRequest{TaskId: leaf.TaskId, Dimension: "money", Purpose: "tool_call", UpperBound: "1"})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatal(err)
	}
}

func TestRPCSubtaskGrantRevocation(t *testing.T) {
	for _, actor := range []string{"owner", "recipient"} {
		t.Run(actor, func(t *testing.T) {
			f := setup(t)
			child, b := createChild(t, f)
			client := f.policy
			if actor == "recipient" {
				client = b.policy
			}
			_, err := client.Revoke(f.ctx, &icsv1.RevokeRequest{Target: &icsv1.RevokeRequest_GrantId{GrantId: child.RootGrantId}, Reason: "withdraw delegated execution"})
			if err != nil {
				t.Fatal(err)
			}
			code(t, chainDecision(b, child), codes.PermissionDenied)
		})
	}
}

func TestRPCSubtaskCapacityAndExpiredReplay(t *testing.T) {
	f := setup(t)
	root := delegableRoot(t, f)
	cmd := subCommand(t, f, root, f.adapter, 0, "0")
	cmd.NotAfter = time.Now().UTC().Add(2 * time.Second)
	req := subRequest(t, f, cmd)
	first, err := f.control.CreateSubtask(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second := subCommand(t, f, root, f.adapter, 0, "0")
	if _, err = f.control.CreateSubtask(f.ctx, subRequest(t, f, second)); err != nil {
		t.Fatal(err)
	}
	third := subCommand(t, f, root, f.adapter, 0, "0")
	_, err = f.control.CreateSubtask(f.ctx, subRequest(t, f, third))
	code(t, err, codes.ResourceExhausted)
	time.Sleep(time.Until(cmd.NotAfter) + 20*time.Millisecond)
	// Model an inbox already removed by retention; original signed expiry remains.
	f.exec("DELETE FROM command_inbox WHERE task_id=$1", first.TaskId)
	_, err = f.control.CreateSubtask(f.ctx, req)
	code(t, err, codes.FailedPrecondition)
	if f.count("SELECT count(*) FROM tasks WHERE parent_task_id=$1", root.TaskId) != 2 {
		t.Fatal("expired replay created another task")
	}
}

func TestRPCSubtaskExpiryAfterInsertWait(t *testing.T) {
	f := setup(t)
	root := delegableRoot(t, f)
	cmd := subCommand(t, f, root, f.adapter, 0, "5")
	cmd.NotAfter = time.Now().UTC().Add(time.Second)
	trigger := "slow_sub_" + strings.ReplaceAll(f.domain, "-", "")
	f.exec(`CREATE FUNCTION ` + trigger + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.parent_task_id='` + root.TaskId + `' THEN PERFORM pg_sleep(1.3); END IF; RETURN NEW; END $$`)
	f.exec(`CREATE TRIGGER ` + trigger + ` BEFORE INSERT ON task_delegations FOR EACH ROW EXECUTE FUNCTION ` + trigger + `() `)
	defer f.exec(`DROP FUNCTION ` + trigger + `() CASCADE`)
	_, err := f.control.CreateSubtask(f.ctx, subRequest(t, f, cmd))
	code(t, err, codes.FailedPrecondition)
	if f.count("SELECT count(*) FROM tasks WHERE parent_task_id=$1", root.TaskId) != 0 || f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", root.TaskId) != 0 || f.count("SELECT count(*) FROM budget_reservations WHERE holder_task_id=$1 AND state='HELD'", root.TaskId) != 4 {
		t.Fatal("expired write committed")
	}
}
