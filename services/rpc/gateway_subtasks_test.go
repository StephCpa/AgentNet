package rpc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentnet/agentnet/internal/signed"
	gwv1 "github.com/agentnet/agentnet/proto/gen/anet/gw/v1"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	"github.com/agentnet/agentnet/services/gateway"
	service "github.com/agentnet/agentnet/services/rpc"
	"github.com/agentnet/agentnet/services/tcs"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

func gatewayDelegableRoot(t *testing.T, f *fixture, g gwv1.GatewayClient) *tcsv1.AdmissionResult {
	t.Helper()
	f.command.Root.Rights.DepthLeft = 2
	f.command.Root.Rights.DelegationScope = []string{"compute"}
	raw, _ := json.Marshal(f.command.Root.Rights)
	f.exec("UPDATE root_consents SET rights=$2::jsonb,version=version+1 WHERE consent_id=$1", f.consent, string(raw))
	f.command.Contract = subContract(t, f.command.Contract, 2, time.Now().UTC().Add(time.Hour))
	r, err := g.SubmitCommand(f.ctx, intent(f))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func subIntent(t *testing.T, c tcs.SubtaskCommand) *gwv1.CreateSubtaskRequest {
	t.Helper()
	raw, err := json.Marshal(gateway.SubtaskIntent{Kind: "anet.subtask-intent/1", ID: c.ID, Parent: c.Parent, Grant: c.Grant, Lease: c.Lease, Epoch: c.Epoch, ExpectedVersion: c.ExpectedVersion, RecipientPolicy: c.RecipientPolicy, RecipientAgent: c.RecipientAgent, RecipientInstance: c.RecipientInstance, Contract: c.Contract, Rights: c.Rights, NotBefore: c.NotBefore, ExpiresAt: c.ExpiresAt, Allocations: c.Allocations})
	if err != nil {
		t.Fatal(err)
	}
	return &gwv1.CreateSubtaskRequest{IntentJson: raw}
}
func storedGatewayCommand(t *testing.T, f *fixture, id string) tcs.Envelope {
	t.Helper()
	var raw []byte
	if err := f.db.QueryRow(f.ctx, "SELECT signed_command FROM gateway_commands WHERE domain_id=$1 AND command_id=$2", f.domain, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var e tcs.Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	return e
}
func TestRPCGatewaySubtaskLifecycle(t *testing.T) {
	f := setup(t)
	g := gatewayClient(t, f, 0)
	root := gatewayDelegableRoot(t, f, g)
	cmd := subCommand(t, f, root, f.adapter, 1, "5")
	req := subIntent(t, cmd)
	child, err := g.CreateSubtask(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	saved := storedGatewayCommand(t, f, cmd.ID)
	var bound tcs.SubtaskSubmission
	if err = signed.Decode(saved, f.cfg.CommandKeys, &bound); err != nil {
		t.Fatal(err)
	}
	if !bound.Caller.SameIdentity(f.caller) || bound.Command.Audience != f.domain || bound.Command.NotAfter.After(time.Now().Add(5*time.Minute)) || bound.Kind != "anet.subtask/1" {
		t.Fatal("missing server binding")
	}
	if f.count("SELECT count(*) FROM gateway_commands WHERE command_id=$1 AND root_permit IS NULL", cmd.ID) != 1 {
		t.Fatal("child acquired a root permit")
	}
	b := recipientFixture(f)
	p, _ := b.proposal(child)
	if _, err = b.control.AdmitEffect(b.ctx, p); err != nil {
		t.Fatal(err)
	}
	reportChild(t, f, p, "COMMITTED")
	nested := subCommand(t, b, child, f.caller, 0, "0")
	nested.ExpiresAt = cmd.ExpiresAt.Add(-time.Second)
	nested.Contract = subContract(t, nested.Contract, 0, nested.ExpiresAt)
	bg := gwv1.NewGatewayClient(f.connect(f.adapterCert))
	leaf, err := bg.CreateSubtask(b.ctx, subIntent(t, nested))
	if err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM grants WHERE grant_id=$1 AND subject_principal_id=$2 AND actor_agent_id=$3", child.RootGrantId, f.caller.Principal, f.adapter.Agent) != 1 {
		t.Fatal("subject/actor changed")
	}
	if _, err = f.control.CancelTask(f.ctx, &tcsv1.CancelTaskRequest{TaskId: root.TaskId}); err != nil {
		t.Fatal(err)
	}
	drainTree(t, f)
	if _, err = f.control.CloseSubtaskBudget(f.ctx, &tcsv1.CloseSubtaskBudgetRequest{ChildTaskId: leaf.TaskId}); err != nil {
		t.Fatal(err)
	}
	if _, err = b.control.CloseSubtaskBudget(b.ctx, &tcsv1.CloseSubtaskBudgetRequest{ChildTaskId: child.TaskId}); err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE root_consents SET revoked_at=clock_timestamp(),version=version+1 WHERE consent_id=$1", f.consent)
	g = gatewayClient(t, f, 0)
	req.IntentJson = append([]byte(" \n"), req.IntentJson...)
	again, err := g.CreateSubtask(f.ctx, req)
	if err != nil || again.GetOutcome() != tcsv1.AdmissionResult_EXISTING || again.GetTaskId() != child.TaskId || !proto.Equal(again.GetAcceptedReceipt(), child.AcceptedReceipt) {
		t.Fatal("accepted child did not recover", again, err)
	}
	after := storedGatewayCommand(t, f, cmd.ID)
	if string(saved.Payload) != string(after.Payload) || string(saved.Signature) != string(after.Signature) || f.count("SELECT sum(settled)::int FROM budget_accounts WHERE task_id=$1", root.TaskId) != 8 {
		t.Fatal("replay changed signature or accounting")
	}
}
func TestRPCGatewaySubtaskForgeryAndNamespace(t *testing.T) {
	f := setup(t)
	g := gatewayClient(t, f, 0)
	root := gatewayDelegableRoot(t, f, g)
	cmd := subCommand(t, f, root, f.adapter, 0, "5")
	for _, field := range []string{`"caller":{}`, `"not_after":"2099-01-01T00:00:00Z"`, `"audience":"other"`, `"signed_command":{}`, `"root_permit":{}`, `"CredentialHash":"forged"`, `"command_id":"duplicate"`} {
		r := subIntent(t, cmd)
		r.IntentJson = append(r.IntentJson[:len(r.IntentJson)-1], []byte(","+field+"}")...)
		_, err := g.CreateSubtask(f.ctx, r)
		code(t, err, codes.InvalidArgument)
	}
	if f.count("SELECT count(*) FROM gateway_commands WHERE command_id=$1", cmd.ID) != 0 {
		t.Fatal("forgery reached signer")
	}
	collision := cmd
	collision.ID = f.command.ID
	_, err := g.CreateSubtask(f.ctx, subIntent(t, collision))
	code(t, err, codes.AlreadyExists)
	if _, err = g.CreateSubtask(f.ctx, subIntent(t, cmd)); err != nil {
		t.Fatal(err)
	}
	f.command.ID = cmd.ID
	_, err = g.SubmitCommand(f.ctx, intent(f))
	code(t, err, codes.AlreadyExists)
	cmd.Epoch++
	_, err = g.CreateSubtask(f.ctx, subIntent(t, cmd))
	code(t, err, codes.AlreadyExists)
	cmd.Epoch--
	other := gwv1.NewGatewayClient(f.connect(f.adapterCert))
	_, err = other.CreateSubtask(f.ctx, subIntent(t, cmd))
	code(t, err, codes.AlreadyExists)
	if f.count("SELECT count(*) FROM tasks WHERE parent_task_id=$1", root.TaskId) != 1 {
		t.Fatal("conflict altered tree")
	}
}
func TestRPCGatewaySubtaskAuthorityGuards(t *testing.T) {
	for _, mode := range []string{"caller", "scope", "policy", "version", "reservation"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			g := gatewayClient(t, f, 0)
			root := gatewayDelegableRoot(t, f, g)
			cmd := subCommand(t, f, root, f.adapter, 0, "5")
			switch mode {
			case "caller":
				g = gwv1.NewGatewayClient(f.connect(f.adapterCert))
			case "scope":
				cmd.Rights.Actions = append(cmd.Rights.Actions, "admin")
			case "policy":
				f.exec("UPDATE recipient_policies SET revoked_at=clock_timestamp() WHERE policy_id=$1", cmd.RecipientPolicy)
			case "version":
				cmd.ExpectedVersion++
			case "reservation":
				a := cmd.Allocations["money"]
				a.Amount = "4"
				cmd.Allocations["money"] = a
			}
			if _, err := g.CreateSubtask(f.ctx, subIntent(t, cmd)); err == nil {
				t.Fatal("signature bypassed authorization")
			}
			if f.count("SELECT count(*) FROM gateway_commands WHERE command_id=$1", cmd.ID) != 1 || f.count("SELECT count(*) FROM tasks WHERE parent_task_id=$1", root.TaskId) != 0 || f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", root.TaskId) != 0 {
				t.Fatal("bound proposal became acceptance")
			}
		})
	}
}
func TestRPCGatewaySubtaskConcurrentAndAuditRollback(t *testing.T) {
	for _, stage := range []string{"gw.command_bound", "tcs.subtask_created"} {
		t.Run(stage, func(t *testing.T) {
			f := setup(t)
			g := gatewayClient(t, f, 0)
			root := gatewayDelegableRoot(t, f, g)
			cmd := subCommand(t, f, root, f.adapter, 0, "5")
			req := subIntent(t, cmd)
			var drop func()
			if stage == "gw.command_bound" {
				// The root already has a legitimate signing audit. Fault only the
				// pending child command, without invalidating that existing row.
				name := "gw_child_" + strings.ReplaceAll(f.domain, "-", "")
				f.exec(fmt.Sprintf(`ALTER TABLE audit_log ADD CONSTRAINT %s CHECK (domain_id <> '%s' OR action <> 'gw.command_bound' OR command_id IS DISTINCT FROM '%s')`, name, f.domain, cmd.ID))
				active := true
				drop = func() {
					if active {
						f.exec("ALTER TABLE audit_log DROP CONSTRAINT " + name)
						active = false
					}
				}
				t.Cleanup(drop)
			} else {
				drop = auditConstraint(t, f, stage)
			}
			_, err := g.CreateSubtask(f.ctx, req)
			code(t, err, codes.Internal)
			if f.count("SELECT count(*) FROM tasks WHERE parent_task_id=$1", root.TaskId) != 0 || f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", root.TaskId) != 0 {
				t.Fatal("partial acceptance")
			}
			expected := 0
			if stage == "tcs.subtask_created" {
				expected = 1
			}
			if f.count("SELECT count(*) FROM gateway_commands WHERE command_id=$1", cmd.ID) != expected {
				t.Fatal("incorrect signing boundary")
			}
			drop()
			var wg sync.WaitGroup
			results := make(chan *tcsv1.AdmissionResult, 8)
			errs := make(chan error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); a, e := g.CreateSubtask(f.ctx, req); results <- a; errs <- e }()
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
			id := ""
			for a := range results {
				if id != "" && id != a.TaskId {
					t.Fatal("multiple children")
				}
				id = a.TaskId
				if a.Outcome != tcsv1.AdmissionResult_EXISTING {
					fresh++
				}
			}
			if fresh != 1 || f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", root.TaskId) != 20 || f.count("SELECT count(*) FROM audit_log WHERE domain_id=$1 AND command_id=$2 AND action='gw.command_bound'", f.domain, cmd.ID) != 1 {
				t.Fatal("duplicate signature/transfer")
			}
		})
	}
}
func TestRPCGatewaySubtaskExpiryAndPendingRecovery(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "accepted"}[accepted], func(t *testing.T) {
			f := setup(t)
			g := gatewayClient(t, f, time.Second)
			root := gatewayDelegableRoot(t, f, g)
			cmd := subCommand(t, f, root, f.adapter, 0, "5")
			req := subIntent(t, cmd)
			if accepted {
				if _, err := g.CreateSubtask(f.ctx, req); err != nil {
					t.Fatal(err)
				}
			} else {
				lock, err := f.db.Begin(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Rollback(f.ctx)
				if _, err = lock.Exec(f.ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "anet.command:"+f.domain+":"+cmd.ID); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(f.ctx)
				defer cancel()
				done := make(chan error, 1)
				go func() { _, e := g.CreateSubtask(ctx, req); done <- e }()
				waitForLock(t, f, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))")
				cancel()
				code(t, <-done, codes.Canceled)
				if err = lock.Rollback(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			saved := storedGatewayCommand(t, f, cmd.ID)
			var bound tcs.SubtaskSubmission
			if err := signed.Decode(saved, f.cfg.CommandKeys, &bound); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Until(bound.Command.NotAfter) + 30*time.Millisecond)
			f.exec("UPDATE command_inbox SET expires_at=not_after WHERE command_id=$1", cmd.ID)
			f.exec("DELETE FROM command_inbox WHERE command_id=$1 AND expires_at<clock_timestamp()", cmd.ID)
			g = gatewayClient(t, f, 5*time.Minute)
			_, err := g.CreateSubtask(f.ctx, req)
			code(t, err, codes.FailedPrecondition)
			expected := 0
			if accepted {
				expected = 1
			}
			if f.count("SELECT count(*) FROM tasks WHERE parent_task_id=$1", root.TaskId) != expected || string(saved.Payload) != string(storedGatewayCommand(t, f, cmd.ID).Payload) {
				t.Fatal("expiry renewed or late child created")
			}
		})
	}
}
func TestRPCGatewayBindingRechecksAfterInsertWait(t *testing.T) {
	for _, kind := range []string{"root", "child"} {
		for _, mode := range []string{"revoked", "expired", "certificate"} {
			t.Run(kind+"-"+mode, func(t *testing.T) {
				f := setup(t)
				root := delegableRoot(t, f)
				cmd := subCommand(t, f, root, f.adapter, 0, "5")
				f.command.ID += "-other"
				f.command.ContractID += "-other"
				ttl := 5 * time.Minute
				if mode == "expired" {
					ttl = time.Second
				}
				if mode == "certificate" {
					short := cert(t, f.ca, f.caKey, "spiffe://test.agentnet/agent-instance/"+f.caller.Instance, false, 2*time.Second)
					f.exec("UPDATE agent_instances SET cred_hash=$2,credential_version=credential_version+1 WHERE instance_id=$1", f.caller.Instance, service.Fingerprint(short.Leaf))
					f.workerCert = short
				}
				g := gatewayClient(t, f, ttl)
				lock, err := f.db.Begin(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Rollback(f.ctx)
				if _, err = lock.Exec(f.ctx, "LOCK TABLE gateway_commands IN SHARE MODE"); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				rootReq, childReq := intent(f), subIntent(t, cmd)
				go func() {
					var e error
					if kind == "root" {
						_, e = g.SubmitCommand(f.ctx, rootReq)
					} else {
						_, e = g.CreateSubtask(f.ctx, childReq)
					}
					done <- e
				}()
				waitForLock(t, f, "INSERT INTO gateway_commands%")
				want := codes.PermissionDenied
				if mode == "revoked" {
					f.exec("UPDATE agent_instances SET revoked_at=clock_timestamp() WHERE instance_id=$1", f.caller.Instance)
				} else if mode == "certificate" {
					time.Sleep(time.Until(f.workerCert.Leaf.NotAfter) + 30*time.Millisecond)
				} else {
					time.Sleep(1100 * time.Millisecond)
					want = codes.FailedPrecondition
				}
				if err = lock.Rollback(f.ctx); err != nil {
					t.Fatal(err)
				}
				code(t, <-done, want)
				if f.count("SELECT count(*) FROM gateway_commands WHERE domain_id=$1", f.domain) != 0 || f.count("SELECT count(*) FROM tasks WHERE domain_id=$1", f.domain) != 1 {
					t.Fatal("stale binding committed")
				}
			})
		}
	}
}
