package rpc_test

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentnet/agentnet/internal/signed"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	"github.com/agentnet/agentnet/services/delivery"
	"github.com/agentnet/agentnet/services/tasktree"
	"github.com/agentnet/agentnet/services/tcs"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

func childParent(t *testing.T, f *fixture, child string) string {
	t.Helper()
	var parent string
	if err := f.db.QueryRow(f.ctx, "SELECT parent_task_id FROM tasks WHERE task_id=$1", child).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	return parent
}
func treeTools(t *testing.T, f *fixture) (*delivery.Queue, *delivery.Inbox, *tasktree.Worker) {
	t.Helper()
	q, err := delivery.New(f.db, delivery.Config{Domain: f.domain, Destination: "local-control"})
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := delivery.NewInbox(f.db, f.domain, "local-control")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := tasktree.New(f.db, f.domain)
	if err != nil {
		t.Fatal(err)
	}
	return q, inbox, tree
}
func drainTree(t *testing.T, f *fixture) {
	t.Helper()
	q, inbox, tree := treeTools(t, f)
	for i := 0; i < 100; i++ {
		a, err := q.Once(f.ctx, inbox)
		if err != nil {
			t.Fatal(err)
		}
		b, err := tree.Once(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !a && !b {
			return
		}
	}
	t.Fatal("tree did not converge")
}
func reportChild(t *testing.T, f *fixture, p *tcsv1.AdmitEffectRequest, outcome string) *tcsv1.ReportEffectResultRequest {
	t.Helper()
	r := tcs.Result{Kind: "anet.result/1", Task: p.Effect.TaskId, Effect: p.Effect.EffectId, ParamsDigest: p.Effect.ParamsDigest, Audience: f.domain, Outcome: outcome, Evidence: "fixture:child-business-fact", UsageSource: "provider", Usage: map[string]string{"money": "2", "tokens": "2", "calls": "2", "messages": "2"}}
	e, err := signed.Encode("adapter", f.adapterKey, r)
	if err != nil {
		t.Fatal(err)
	}
	req := &tcsv1.ReportEffectResultRequest{SignedResult: pb(e)}
	if _, err = tcsv1.NewTaskControlClient(f.connect(f.adapterCert)).ReportEffectResult(f.ctx, req); err != nil {
		t.Fatal(err)
	}
	return req
}
func TestRPCSubtaskCloseoutUnknownAndLateSettlement(t *testing.T) {
	f := setup(t)
	child, b := createChild(t, f)
	parent := childParent(t, f, child.TaskId)
	p, _ := b.proposal(child)
	if _, err := b.control.AdmitEffect(b.ctx, p); err != nil {
		t.Fatal(err)
	}
	reportChild(t, f, p, "UNKNOWN")
	if _, err := f.control.CancelTask(f.ctx, &tcsv1.CancelTaskRequest{TaskId: parent}); err != nil {
		t.Fatal(err)
	}
	drainTree(t, f)
	req := &tcsv1.CloseSubtaskBudgetRequest{ChildTaskId: child.TaskId}
	_, err := b.control.CloseSubtaskBudget(b.ctx, req)
	code(t, err, codes.FailedPrecondition)
	if f.count("SELECT count(*) FROM tasks WHERE task_id=$1 AND status='CANCELLED' AND cancel_version=1", child.TaskId) != 1 || f.count("SELECT count(*) FROM budget_reservations WHERE effect_id=$1 AND state='HELD'", p.Effect.EffectId) != 4 || f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", parent) != 20 {
		t.Fatal("cancel released unknown liability")
	}
	fact := reportChild(t, f, p, "COMMITTED")
	closed, err := b.control.CloseSubtaskBudget(b.ctx, req)
	if err != nil || closed.Existing || len(closed.GetDimensions()) != 4 {
		t.Fatal(closed, err)
	}
	for _, d := range closed.Dimensions {
		if d.Allocated != "5.000000" || d.Settled != "2.000000" || d.Returned != "3.000000" {
			t.Fatal(d)
		}
	}
	again, err := f.control.CloseSubtaskBudget(f.ctx, req)
	if err != nil || !again.Existing {
		t.Fatal(again, err)
	}
	again.Existing = false
	if !proto.Equal(closed, again) {
		t.Fatal("closeout receipt changed")
	}
	if f.count("SELECT count(*) FROM budget_accounts WHERE task_id=$1 AND settled=2 AND delegated=0", parent) != 4 || f.count("SELECT count(*) FROM budget_accounts WHERE task_id=$1 AND closed_at IS NOT NULL AND budget_limit=settled AND settled=2", child.TaskId) != 4 {
		t.Fatal("incorrect rollup")
	}
	if _, err = b.control.ReportEffectResult(b.ctx, fact); err != nil {
		t.Fatal("late fact replay lost", err)
	}
	// Parent is cancelled too: returned quota does not restart cancelled work.
	_, err = f.control.ReserveBudget(f.ctx, &tcsv1.ReserveBudgetRequest{TaskId: parent, Dimension: "money", Purpose: "tool_call", UpperBound: "8"})
	code(t, err, codes.FailedPrecondition)
	if _, err = f.db.Exec(f.ctx, "UPDATE budget_accounts SET budget_limit=budget_limit+1 WHERE task_id=$1", child.TaskId); err == nil {
		t.Fatal("closed account reopened")
	}
}
func TestRPCSubtaskCloseoutUnboundAndFrozen(t *testing.T) {
	for _, mode := range []string{"unbound", "frozen", "active"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			child, b := createChild(t, f)
			parent := childParent(t, f, child.TaskId)
			var hold *tcsv1.Reservation
			if mode == "unbound" {
				var err error
				hold, err = b.control.ReserveBudget(b.ctx, &tcsv1.ReserveBudgetRequest{TaskId: child.TaskId, Dimension: "money", Purpose: "tool_call", UpperBound: "1"})
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode != "active" {
				if _, err := b.control.CancelTask(b.ctx, &tcsv1.CancelTaskRequest{TaskId: child.TaskId}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "frozen" {
				f.exec("UPDATE tasks SET budget_frozen=true,task_version=task_version+1 WHERE task_id=$1", child.TaskId)
			}
			req := &tcsv1.CloseSubtaskBudgetRequest{ChildTaskId: child.TaskId}
			_, err := b.control.CloseSubtaskBudget(b.ctx, req)
			code(t, err, codes.FailedPrecondition)
			if f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", parent) != 20 {
				t.Fatal("pending liability returned")
			}
			if mode == "unbound" {
				if _, err = b.control.ReleaseReservation(b.ctx, &tcsv1.ReleaseReservationRequest{TaskId: child.TaskId, ReservationId: hold.ReservationId}); err != nil {
					t.Fatal(err)
				}
				if _, err = b.control.CloseSubtaskBudget(b.ctx, req); err != nil {
					t.Fatal(err)
				}
				// Live parent can reuse exactly the restored capacity.
				if _, err = f.control.ReserveBudget(f.ctx, &tcsv1.ReserveBudgetRequest{TaskId: parent, Dimension: "money", Purpose: "tool_call", UpperBound: "10"}); err != nil {
					t.Fatal(err)
				}
				_, err = f.control.ReserveBudget(f.ctx, &tcsv1.ReserveBudgetRequest{TaskId: parent, Dimension: "money", Purpose: "tool_call", UpperBound: "1"})
				code(t, err, codes.ResourceExhausted)
			}
		})
	}
}
func TestRPCSubtaskCloseoutNested(t *testing.T) {
	for _, amount := range []string{"0", "4"} {
		t.Run(amount, func(t *testing.T) {
			f := setup(t)
			root := delegableRoot(t, f)
			cmd := subCommand(t, f, root, f.adapter, 1, "5")
			child, err := f.control.CreateSubtask(f.ctx, subRequest(t, f, cmd))
			if err != nil {
				t.Fatal(err)
			}
			b := recipientFixture(f)
			nested := subCommand(t, b, child, f.caller, 0, amount)
			nested.ExpiresAt = cmd.ExpiresAt.Add(-time.Second)
			nested.Contract = subContract(t, nested.Contract, 0, nested.ExpiresAt)
			leaf, err := b.control.CreateSubtask(b.ctx, subRequest(t, b, nested))
			if err != nil {
				t.Fatal(err)
			}
			var proposal *tcsv1.AdmitEffectRequest
			if amount == "4" {
				proposal, _ = f.proposal(leaf)
				if _, err = f.control.AdmitEffect(f.ctx, proposal); err != nil {
					t.Fatal(err)
				}
			}
			// Terminal intermediaries keep their fact but still forward tree cancellation.
			if amount == "0" {
				f.exec("UPDATE tasks SET status='FAILED',task_version=task_version+1 WHERE task_id=$1", child.TaskId)
			}
			if _, err = f.control.CancelTask(f.ctx, &tcsv1.CancelTaskRequest{TaskId: root.TaskId}); err != nil {
				t.Fatal(err)
			}
			drainTree(t, f)
			if f.count("SELECT count(*) FROM tasks WHERE task_id=$1 AND status='CANCELLED'", leaf.TaskId) != 1 {
				t.Fatal("grandchild not cancelled")
			}
			if amount == "0" && f.count("SELECT count(*) FROM tasks WHERE task_id=$1 AND status='FAILED'", child.TaskId) != 1 {
				t.Fatal("terminal fact overwritten")
			}
			_, err = b.control.CloseSubtaskBudget(b.ctx, &tcsv1.CloseSubtaskBudgetRequest{ChildTaskId: child.TaskId})
			code(t, err, codes.FailedPrecondition)
			if proposal != nil {
				reportChild(t, f, proposal, "COMMITTED")
			}
			if _, err = f.control.CloseSubtaskBudget(f.ctx, &tcsv1.CloseSubtaskBudgetRequest{ChildTaskId: leaf.TaskId}); err != nil {
				t.Fatal(err)
			}
			if _, err = b.control.CloseSubtaskBudget(b.ctx, &tcsv1.CloseSubtaskBudgetRequest{ChildTaskId: child.TaskId}); err != nil {
				t.Fatal(err)
			}
			expected := 0
			if amount == "4" {
				expected = 8
			}
			if f.count("SELECT sum(settled)::int FROM budget_accounts WHERE task_id=$1", root.TaskId) != expected || f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", root.TaskId) != 0 {
				t.Fatal("nested costs not conserved")
			}
		})
	}
}
func TestRPCSubtaskCloseoutRollbackAndConcurrency(t *testing.T) {
	f := setup(t)
	child, b := createChild(t, f)
	parent := childParent(t, f, child.TaskId)
	if _, err := b.control.CancelTask(b.ctx, &tcsv1.CancelTaskRequest{TaskId: child.TaskId}); err != nil {
		t.Fatal(err)
	}
	trigger := "fail_close_" + strings.ReplaceAll(f.domain, "-", "")
	f.exec(`CREATE FUNCTION ` + trigger + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.task_id='` + parent + `' AND NEW.event_type='CHILD_BUDGET_SETTLED' THEN RAISE EXCEPTION 'late close failure'; END IF; RETURN NEW; END $$`)
	f.exec(`CREATE TRIGGER ` + trigger + ` BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION ` + trigger + `() `)
	req := &tcsv1.CloseSubtaskBudgetRequest{ChildTaskId: child.TaskId}
	_, err := b.control.CloseSubtaskBudget(b.ctx, req)
	if err == nil {
		t.Fatal("injection ignored")
	}
	f.exec(`DROP FUNCTION ` + trigger + `() CASCADE`)
	if f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", parent) != 20 || f.count("SELECT count(*) FROM budget_accounts WHERE task_id=$1 AND closed_at IS NOT NULL", child.TaskId) != 0 {
		t.Fatal("partial closeout")
	}
	results := make(chan *tcsv1.SubtaskBudgetCloseout, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r, e := b.control.CloseSubtaskBudget(b.ctx, req); results <- r; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	fresh := 0
	for r := range results {
		if !r.Existing {
			fresh++
		}
	}
	if fresh != 1 || f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", parent) != 0 {
		t.Fatal("duplicate closeout")
	}
}
func TestRPCSubtaskCloseoutIdentityAfterWait(t *testing.T) {
	f := setup(t)
	child, b := createChild(t, f)
	parent := childParent(t, f, child.TaskId)
	if _, err := b.control.CancelTask(b.ctx, &tcsv1.CancelTaskRequest{TaskId: child.TaskId}); err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, "SELECT account_id FROM budget_accounts WHERE task_id=$1 FOR UPDATE", parent); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := b.control.CloseSubtaskBudget(b.ctx, &tcsv1.CloseSubtaskBudgetRequest{ChildTaskId: child.TaskId})
		done <- err
	}()
	waitForLock(t, f, "SELECT account_id,task_id,COALESCE(parent_account_id%")
	f.exec("UPDATE agent_instances SET revoked_at=clock_timestamp() WHERE instance_id=$1", b.caller.Instance)
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	code(t, <-done, codes.PermissionDenied)
	if f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", parent) != 20 {
		t.Fatal("revoked caller changed ledger")
	}
	if _, err = f.control.CloseSubtaskBudget(f.ctx, &tcsv1.CloseSubtaskBudgetRequest{ChildTaskId: child.TaskId}); err != nil {
		t.Fatal(err)
	}
}
func TestRPCTreeCancellationAuthorityAndReplay(t *testing.T) {
	f := setup(t)
	child, b := createChild(t, f)
	parent := childParent(t, f, child.TaskId)
	q, inbox, tree := treeTools(t, f)
	forged := delivery.Message{SourceDomain: f.domain, Destination: "local-control", Key: "forged", TaskID: parent, Type: "CANCELLED", Payload: json.RawMessage(`{"cancel_version":1}`)}
	if err := inbox.Publish(f.ctx, forged); !errors.Is(err, delivery.ErrInvalid) {
		t.Fatal("notification cancelled live task", err)
	}
	f.exec("INSERT INTO task_cancellations(task_id,parent_task_id,origin_task_id,domain_id) VALUES($1,$2,$2,$3)", child.TaskId, parent, f.domain)
	if _, err := tree.Once(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM task_cancellations WHERE task_id=$1 AND state='INVALID'", child.TaskId) != 1 || f.count("SELECT count(*) FROM tasks WHERE task_id=$1 AND status='ACCEPTED'", child.TaskId) != 1 {
		t.Fatal("queue row became authority")
	}
	// A real cancelled task in the same domain still cannot cancel a sibling.
	root := f.submit()
	siblingCommand := subCommand(t, f, root, f.caller, 0, "0")
	sibling, err := f.control.CreateSubtask(f.ctx, subRequest(t, f, siblingCommand))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.control.CancelTask(f.ctx, &tcsv1.CancelTaskRequest{TaskId: sibling.TaskId}); err != nil {
		t.Fatal(err)
	}
	_, err = b.control.CloseSubtaskBudget(b.ctx, &tcsv1.CloseSubtaskBudgetRequest{ChildTaskId: sibling.TaskId})
	code(t, err, codes.PermissionDenied)
	f.exec("UPDATE task_cancellations SET state='PENDING',completed_at=NULL,origin_task_id=$2 WHERE task_id=$1", child.TaskId, sibling.TaskId)
	if _, err = tree.Once(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM task_cancellations WHERE task_id=$1 AND state='INVALID'", child.TaskId) != 1 || f.count("SELECT count(*) FROM tasks WHERE task_id=$1 AND status='ACCEPTED'", child.TaskId) != 1 {
		t.Fatal("sibling origin became authority")
	}
	if _, err := f.control.CancelTask(f.ctx, &tcsv1.CancelTaskRequest{TaskId: parent}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		work, err := q.Once(f.ctx, inbox)
		if err != nil {
			t.Fatal(err)
		}
		if !work {
			break
		}
	}
	// The legitimate event replaces a quarantined invalid intent.
	if f.count("SELECT count(*) FROM task_cancellations WHERE task_id=$1 AND state='PENDING'", child.TaskId) != 1 {
		t.Fatal("invalid intent poisoned later cancellation")
	}
	drainTree(t, f)
	if err := inbox.Publish(f.ctx, forged); err != nil {
		t.Fatal(err)
	}
	drainTree(t, f)
	if f.count("SELECT cancel_version::int FROM tasks WHERE task_id=$1", child.TaskId) != 1 || f.count("SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='CANCELLED'", child.TaskId) != 1 || f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", parent) != 20 {
		t.Fatal("cancellation replay changed version/budget")
	}
}
