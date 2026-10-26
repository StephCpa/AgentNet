package tcs

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	t      *testing.T
	ctx    context.Context
	db     *pgxpool.Pool
	s      *Store
	caller Caller
	key    ed25519.PrivateKey
	cfg    Config
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("ANET_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL; run test/schema/run.py --kernel-tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ctx: ctx, db: db, key: key, caller: Caller{Principal: id("usr"), Agent: id("agt"), Instance: id("ins")}}
	f.cfg = Config{Domain: id("dom"), CommandKeys: map[string]ed25519.PublicKey{"gateway": pub}, DecisionKeys: map[string]ed25519.PublicKey{"ics": pub}, ResultKeys: map[string]ed25519.PublicKey{"adapter": pub},
		Operations: map[string]Operation{"bank": {Action: "pay", Resource: "account:test", Purpose: "tool_call", Capability: "queryable", Dimensions: []string{"money", "tokens", "calls", "messages"}}}}
	f.exec("INSERT INTO domains(domain_id,name,gateway_endpoint,trust_anchor) VALUES($1,'test','https://invalid.test','{}')", f.cfg.Domain)
	f.exec("INSERT INTO principals(principal_id,kind,domain_id) VALUES($1,'user',$2)", f.caller.Principal, f.cfg.Domain)
	f.exec("INSERT INTO agents(agent_id,domain_id,owner_principal_id,handle,card_digest) VALUES($1,$2,$3,$1,'sha256:test')", f.caller.Agent, f.cfg.Domain, f.caller.Principal)
	f.exec("INSERT INTO agent_instances(instance_id,agent_id,cred_hash) VALUES($1,$2,'test')", f.caller.Instance, f.caller.Agent)
	f.s, err = New(db, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) integer(sql string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.db.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}
func (f *fixture) command() Command {
	body, _ := json.Marshal(map[string]any{"deadline": time.Now().UTC().Add(time.Hour), "input_refs": []any{map[string]any{"version": 1, "digest": "sha256:" + strings.Repeat("a", 64)}}, "output_schema": map[string]string{"type": "object"}, "acceptance_policy": map[string]string{"verifier": "test"}})
	return Command{ID: id("cmd"), Audience: f.cfg.Domain, NotAfter: time.Now().UTC().Add(time.Hour), ContractID: id("con"), ContractVersion: 1, Contract: body, Limits: map[string]string{"money": "10", "tokens": "10", "calls": "10", "messages": "10"}}
}
func (f *fixture) sign(v any, key string) Envelope {
	f.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Fatal(err)
	}
	b, err = canonical.Bytes(b)
	if err != nil {
		f.t.Fatal(err)
	}
	return Envelope{KeyID: key, Payload: b, Signature: ed25519.Sign(f.key, b)}
}
func (f *fixture) prepare() (Proposal, Decision) {
	f.t.Helper()
	cmd := f.command()
	a, err := f.submit(cmd)
	if err != nil {
		f.t.Fatal(err)
	}
	lease, err := f.s.AcquireLease(f.ctx, f.caller, a.TaskID)
	if err != nil {
		f.t.Fatal(err)
	}
	grant := id("grt")
	f.exec(`INSERT INTO grants(grant_id,root_grant_id,task_id,subject_principal_id,actor_agent_id,rights,not_before,expires_at,policy_version)
 VALUES($1,$1,$2,$3,$4,'{"actions":["pay"],"resources":["account:test"]}',clock_timestamp()-interval '1 minute',clock_timestamp()+interval '1 hour','test')`, grant, a.TaskID, f.caller.Principal, f.caller.Agent)
	p := Proposal{EffectID: id("eff"), TaskID: a.TaskID, CommandID: cmd.ID, Target: "bank", ParamsDigest: "sha256:" + strings.Repeat("b", 64), AuthorityDigest: "sha256:" + strings.Repeat("c", 64), LeaseID: lease.ID, Epoch: lease.Epoch, ExpectedVersion: lease.Version, Sensitivity: "sensitive", Reservations: map[string]string{}, UpperBounds: map[string]string{}}
	for _, dim := range []string{"money", "tokens", "calls", "messages"} {
		r, err := f.s.Reserve(f.ctx, f.caller, a.TaskID, dim, "tool_call", "4")
		if err != nil {
			f.t.Fatal(err)
		}
		p.Reservations[dim] = r.ID
		p.UpperBounds[dim] = "3"
	}
	d := Decision{Kind: "anet.authorization/1", ID: id("dec"), Allow: true, At: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute), Subject: f.caller.Principal, Agent: f.caller.Agent, Instance: f.caller.Instance, Grant: grant, Task: a.TaskID, Target: p.Target, Action: "pay", Resource: "account:test", ParamsDigest: p.ParamsDigest, AuthorityDigest: p.AuthorityDigest, Audience: f.cfg.Domain, Sensitivity: p.Sensitivity}
	return p, d
}
func (f *fixture) admit(p Proposal, d Decision) {
	f.t.Helper()
	if _, err := f.s.Admit(f.ctx, f.caller, p, f.sign(d, "ics")); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) result(p Proposal, outcome, source, usage string) Result {
	return Result{Kind: "anet.result/1", Task: p.TaskID, Effect: p.EffectID, ParamsDigest: p.ParamsDigest, Audience: f.cfg.Domain, Outcome: outcome, UsageSource: source, Evidence: "provider:receipt", Usage: map[string]string{"money": usage, "tokens": usage, "calls": usage, "messages": usage}}
}
func wantCode(t *testing.T, err error, code Code) {
	t.Helper()
	if !errors.Is(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestKernelSubmitConcurrentAndRecovery(t *testing.T) {
	f := setup(t)
	cmd := f.command()
	var wg sync.WaitGroup
	answers := make(chan Admission, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a, e := f.submit(cmd); answers <- a; errs <- e }()
	}
	wg.Wait()
	close(answers)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	taskID, receiptID := "", ""
	created := 0
	for a := range answers {
		if !a.Existing {
			created++
		}
		if taskID != "" && (taskID != a.TaskID || receiptID != a.ReceiptID) {
			t.Fatal("duplicate identity")
		}
		taskID = a.TaskID
		receiptID = a.ReceiptID
	}
	if created != 1 {
		t.Fatalf("new=%d", created)
	}
	// Reopen a new pool, as after COMMIT with an ACK lost and process state gone.
	pool, err := pgxpool.New(f.ctx, os.Getenv("ANET_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	restarted, err := New(pool, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Submit(f.ctx, f.caller, f.sign(Submission{Kind: "anet.command/1", Caller: f.caller, Command: cmd}, "gateway"))
	if err != nil || !got.Existing || got.TaskID != taskID || got.ReceiptID != receiptID {
		t.Fatalf("retry=%+v %v", got, err)
	}
	if f.integer("SELECT count(*) FROM tasks WHERE domain_id=$1", f.cfg.Domain) != 1 || f.integer("SELECT count(*) FROM outbox WHERE task_id=$1", taskID) != 1 {
		t.Fatal("duplicate durable work")
	}
	cmd.Limits["money"] = "9"
	_, err = f.submit(cmd)
	wantCode(t, err, Conflict)
}

func TestKernelSubmitAtomicRollback(t *testing.T) {
	f := setup(t)
	cmd := f.command()
	constraint := "test_outbox_" + strings.TrimPrefix(id("x"), "x_")
	// Trigger a real DB failure after task, receipt and account writes.
	f.exec(fmt.Sprintf("ALTER TABLE outbox ADD CONSTRAINT %s CHECK (dedup_key <> '%s')", constraint, "accepted:"+f.cfg.Domain+":"+cmd.ID))
	t.Cleanup(func() { f.db.Exec(context.Background(), "ALTER TABLE outbox DROP CONSTRAINT "+constraint) })
	if _, err := f.submit(cmd); err == nil {
		t.Fatal("expected DB failure")
	}
	if f.integer("SELECT count(*) FROM tasks WHERE domain_id=$1", f.cfg.Domain) != 0 || f.integer("SELECT count(*) FROM command_inbox WHERE audience_domain_id=$1", f.cfg.Domain) != 0 {
		t.Fatal("partial commit")
	}
}

func TestKernelExpiryAfterLockWait(t *testing.T) {
	f := setup(t)
	cmd := f.command()
	cmd.NotAfter = time.Now().UTC().Add(time.Second)
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	_, err = tx.Exec(f.ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "anet.command:"+cmd.Audience+":"+cmd.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := f.submit(cmd); done <- e }()
	waitUntil(t, func() bool { return f.integer("SELECT count(*) FROM pg_stat_activity WHERE wait_event='advisory'") > 0 })
	time.Sleep(time.Until(cmd.NotAfter) + 20*time.Millisecond)
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	wantCode(t, <-done, Expired)
	if f.integer("SELECT count(*) FROM tasks WHERE domain_id=$1", f.cfg.Domain) != 0 {
		t.Fatal("expired command created a task")
	}
}

func TestKernelExpiryAfterInboxCleanup(t *testing.T) {
	f := setup(t)
	cmd := f.command()
	cmd.NotAfter = time.Now().UTC().Add(2 * time.Second)
	a, err := f.submit(cmd)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(cmd.NotAfter) + 20*time.Millisecond)
	f.exec("DELETE FROM command_inbox WHERE task_id=$1 AND not_after<clock_timestamp()", a.TaskID)
	_, err = f.submit(cmd)
	wantCode(t, err, Expired)
	if f.integer("SELECT count(*) FROM tasks WHERE domain_id=$1", f.cfg.Domain) != 1 {
		t.Fatal("replay created task")
	}
}

func TestKernelBudgetConcurrentReserve(t *testing.T) {
	f := setup(t)
	a, err := f.submit(f.command())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, e := f.s.Reserve(f.ctx, f.caller, a.TaskID, "money", "tool_call", "7"); done <- e }()
	}
	ok, denied := 0, 0
	for i := 0; i < 2; i++ {
		e := <-done
		if e == nil {
			ok++
		} else if errors.Is(e, BudgetExhausted) {
			denied++
		} else {
			t.Fatal(e)
		}
	}
	if ok != 1 || denied != 1 {
		t.Fatalf("ok=%d denied=%d", ok, denied)
	}
}

func TestKernelAdmitIdempotenceAndConflict(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	f.admit(p, d)
	// Old expected version, no fresh decision: querying the existing fact is still idempotent.
	got, err := f.s.Admit(f.ctx, f.caller, p, Envelope{})
	if err != nil || !got.Existing {
		t.Fatalf("retry %+v %v", got, err)
	}
	p.ParamsDigest = "sha256:" + strings.Repeat("d", 64)
	_, err = f.s.Admit(f.ctx, f.caller, p, f.sign(d, "ics"))
	wantCode(t, err, EffectConflict)
	if f.integer("SELECT count(*) FROM effect_conflict_attempts WHERE effect_id=$1", p.EffectID) != 1 {
		t.Fatal("conflict not durable")
	}
	if f.integer("SELECT count(*) FROM budget_reservations WHERE effect_id=$1 AND state='HELD'", p.EffectID) != 4 {
		t.Fatal("binding changed")
	}
}

func TestKernelReservationAtomicityAndExclusiveBinding(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	bad := p.Reservations["messages"]
	p.Reservations["messages"] = "missing"
	_, err := f.s.Admit(f.ctx, f.caller, p, f.sign(d, "ics"))
	wantCode(t, err, ReservationInvalid)
	if f.integer("SELECT count(*) FROM budget_reservations WHERE holder_task_id=$1 AND effect_id IS NOT NULL", p.TaskID) != 0 {
		t.Fatal("partially bound")
	}
	p.Reservations["messages"] = bad
	f.admit(p, d)
	p.EffectID = id("eff")
	p.ExpectedVersion++
	_, err = f.s.Admit(f.ctx, f.caller, p, f.sign(d, "ics"))
	wantCode(t, err, ReservationInvalid)
}

func TestKernelRequiresAllServerDimensions(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	delete(p.Reservations, "money")
	delete(p.UpperBounds, "money")
	_, err := f.s.Admit(f.ctx, f.caller, p, f.sign(d, "ics"))
	wantCode(t, err, ReservationInvalid)
}

func TestKernelLeaseTakeoverAndStaleProposal(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	other := f.caller
	other.Instance = id("ins")
	f.exec("INSERT INTO agent_instances(instance_id,agent_id,cred_hash) VALUES($1,$2,'test')", other.Instance, other.Agent)
	f.exec("UPDATE leases SET expires_at=clock_timestamp()-interval '20 seconds' WHERE lease_id=$1", p.LeaseID)
	lease, err := f.s.AcquireLease(f.ctx, other, p.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	p.ExpectedVersion = lease.Version
	p.LeaseID = lease.ID
	_, err = f.s.Admit(f.ctx, f.caller, p, f.sign(d, "ics"))
	wantCode(t, err, Fenced)
	p.Epoch = lease.Epoch // stale worker queries the current epoch and copies the new lease ID
	_, err = f.s.Admit(f.ctx, f.caller, p, f.sign(d, "ics"))
	wantCode(t, err, LeaseInvalid)
	d.Instance = other.Instance
	p.Epoch = lease.Epoch - 1
	_, err = f.s.Admit(f.ctx, other, p, f.sign(d, "ics"))
	wantCode(t, err, Fenced)
	p.Epoch = lease.Epoch
	_, err = f.s.Admit(f.ctx, other, p, f.sign(d, "ics"))
	if err != nil {
		t.Fatal(err)
	}
}

func TestKernelSignedDecisionBinding(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	edits := map[string]func(*Decision){"subject": func(x *Decision) { x.Subject = "other" }, "actor": func(x *Decision) { x.Instance = "other" }, "task": func(x *Decision) { x.Task = "other" }, "params": func(x *Decision) { x.ParamsDigest = "other" }, "action": func(x *Decision) { x.Action = "read" }, "resource": func(x *Decision) { x.Resource = "other" }, "sensitivity": func(x *Decision) { x.Sensitivity = "standard" }, "future": func(x *Decision) { x.At = time.Now().UTC().Add(time.Hour) }, "stale": func(x *Decision) { x.At = time.Now().UTC().Add(-time.Hour) }, "expired": func(x *Decision) { x.ExpiresAt = time.Now().UTC().Add(-time.Second) }}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			bad := d
			edit(&bad)
			_, err := f.s.Admit(f.ctx, f.caller, p, f.sign(bad, "ics"))
			wantCode(t, err, DecisionMismatch)
		})
	}
	signed := f.sign(d, "ics")
	signed.Signature[0] ^= 1
	_, err := f.s.Admit(f.ctx, f.caller, p, signed)
	wantCode(t, err, Denied)
	f.exec("UPDATE grants SET status='revoked',revoked_at=clock_timestamp() WHERE grant_id=$1", d.Grant)
	_, err = f.s.Admit(f.ctx, f.caller, p, f.sign(d, "ics"))
	wantCode(t, err, Denied)
}

func TestKernelRevokedInstanceAndTaskOwnership(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	bad := f.caller
	bad.Principal = "other"
	_, err := f.s.Admit(f.ctx, bad, p, f.sign(d, "ics"))
	wantCode(t, err, Denied)
	f.exec("UPDATE agent_instances SET revoked_at=clock_timestamp() WHERE instance_id=$1", f.caller.Instance)
	_, err = f.s.Reserve(f.ctx, f.caller, p.TaskID, "money", "tool_call", "1")
	wantCode(t, err, Denied)
}

func TestKernelCancelAdmissionRace(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	done := make(chan error, 2)
	go func() { _, e := f.s.Admit(f.ctx, f.caller, p, f.sign(d, "ics")); done <- e }()
	go func() { _, e := f.s.Cancel(f.ctx, f.caller, p.TaskID); done <- e }()
	for i := 0; i < 2; i++ {
		err := <-done
		if err != nil && !errors.Is(err, Terminal) {
			t.Fatal(err)
		}
	}
	version, err := f.s.Cancel(f.ctx, f.caller, p.TaskID)
	if err != nil || version != 1 {
		t.Fatalf("cancel %d %v", version, err)
	}
	p.EffectID = id("eff")
	_, err = f.s.Admit(f.ctx, f.caller, p, f.sign(d, "ics"))
	wantCode(t, err, Terminal)
}

func TestKernelUnknownConflictAndLateSettlement(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	f.admit(p, d)
	unknown := f.result(p, "UNKNOWN", "provider", "0")
	_, err := f.s.Resolve(f.ctx, f.sign(unknown, "adapter"))
	if err != nil {
		t.Fatal(err)
	}
	changed := p
	changed.ParamsDigest = "sha256:" + strings.Repeat("d", 64)
	_, err = f.s.Admit(f.ctx, f.caller, changed, Envelope{})
	wantCode(t, err, EffectConflict)
	if f.integer("SELECT count(*) FROM budget_reservations WHERE effect_id=$1 AND state='HELD'", p.EffectID) != 4 {
		t.Fatal("unknown released")
	}
	_, err = f.s.Cancel(f.ctx, f.caller, p.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	result := f.result(p, "COMMITTED", "provider", "2")
	signed := f.sign(result, "adapter")
	_, err = f.s.Resolve(f.ctx, signed)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.s.Resolve(f.ctx, signed)
	if err != nil || !got.Existing {
		t.Fatalf("repeat %+v %v", got, err)
	}
	if f.integer("SELECT sum(settled)::bigint FROM budget_accounts WHERE task_id=$1", p.TaskID) != 8 || f.integer("SELECT count(*) FROM budget_reservations WHERE effect_id=$1 AND state='HELD'", p.EffectID) != 0 {
		t.Fatal("settlement wrong")
	}
	if f.integer("SELECT count(*) FROM tasks WHERE task_id=$1 AND status='CANCELLED'", p.TaskID) != 1 {
		t.Fatal("late result restarted task")
	}
	result.Outcome = "REJECTED"
	_, err = f.s.Resolve(f.ctx, f.sign(result, "adapter"))
	wantCode(t, err, ResultConflict)
}

func TestKernelConservativeSelfReport(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	f.admit(p, d)
	_, err := f.s.Resolve(f.ctx, f.sign(f.result(p, "COMMITTED", "self_reported", "0"), "adapter"))
	if err != nil {
		t.Fatal(err)
	}
	if f.integer("SELECT sum(settled)::bigint FROM budget_accounts WHERE task_id=$1", p.TaskID) != 16 {
		t.Fatal("self report released budget")
	}
}

func TestKernelOverrunPreservesEvidenceAndFreezes(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	f.admit(p, d)
	_, err := f.s.Resolve(f.ctx, f.sign(f.result(p, "COMMITTED", "provider", "5"), "adapter"))
	wantCode(t, err, BudgetViolation)
	if f.integer("SELECT count(*) FROM budget_reservations WHERE effect_id=$1 AND state='HELD'", p.EffectID) != 4 {
		t.Fatal("overrun released holds")
	}
	if f.integer("SELECT count(*) FROM audit_log WHERE effect_id=$1 AND action='budget.violation' AND cost->'usage'->>'money'='5'", p.EffectID) != 1 {
		t.Fatal("actual usage lost")
	}
	_, err = f.s.Reserve(f.ctx, f.caller, p.TaskID, "money", "tool_call", "1")
	wantCode(t, err, BudgetViolation)
}

func TestKernelForgedResultCannotReleaseUnknown(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	f.admit(p, d)
	if _, err := f.s.Resolve(f.ctx, f.sign(f.result(p, "UNKNOWN", "provider", "0"), "adapter")); err != nil {
		t.Fatal(err)
	}
	signed := f.sign(f.result(p, "REJECTED", "provider", "0"), "adapter")
	signed.Signature[0] ^= 1
	_, err := f.s.Resolve(f.ctx, signed)
	wantCode(t, err, Denied)
	if f.integer("SELECT count(*) FROM budget_reservations WHERE effect_id=$1 AND state='HELD'", p.EffectID) != 4 {
		t.Fatal("forged release")
	}
}

func TestKernelLeaseRenewalAndGrace(t *testing.T) {
	f := setup(t)
	p, _ := f.prepare()
	lease, err := f.s.RenewLease(f.ctx, f.caller, p.TaskID, p.LeaseID)
	if err != nil || lease.Epoch != p.Epoch {
		t.Fatalf("renew %+v %v", lease, err)
	}
	snap, err := f.s.GetTask(f.ctx, f.caller, p.TaskID)
	if err != nil || snap.Version != p.ExpectedVersion {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	f.exec("UPDATE leases SET expires_at=clock_timestamp()-interval '1 second' WHERE lease_id=$1", p.LeaseID)
	_, err = f.s.RenewLease(f.ctx, f.caller, p.TaskID, p.LeaseID)
	wantCode(t, err, LeaseInvalid)
	_, err = f.s.AcquireLease(f.ctx, f.caller, p.TaskID)
	wantCode(t, err, LeaseInvalid)
	f.exec("UPDATE leases SET expires_at=clock_timestamp()-interval '20 seconds' WHERE lease_id=$1", p.LeaseID)
	next, err := f.s.AcquireLease(f.ctx, f.caller, p.TaskID)
	if err != nil || next.Epoch != p.Epoch+1 {
		t.Fatalf("takeover %+v %v", next, err)
	}
}

func TestKernelReleaseUnboundOnly(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	r, err := f.s.Reserve(f.ctx, f.caller, p.TaskID, "money", "tool_call", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.ReleaseUnbound(f.ctx, f.caller, p.TaskID, r.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.s.ReleaseUnbound(f.ctx, f.caller, p.TaskID, r.ID); err != nil {
		t.Fatal(err)
	}
	f.admit(p, d)
	_, err = f.s.Cancel(f.ctx, f.caller, p.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	err = f.s.ReleaseUnbound(f.ctx, f.caller, p.TaskID, p.Reservations["money"])
	wantCode(t, err, ReservationInvalid)
}

func TestKernelGrantScopeAndResultBinding(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	f.exec(`UPDATE grants SET rights='{"actions":["read"],"resources":["account:test"]}' WHERE grant_id=$1`, d.Grant)
	_, err := f.s.Admit(f.ctx, f.caller, p, f.sign(d, "ics"))
	wantCode(t, err, Denied)
	f.exec(`UPDATE grants SET rights='{"actions":["pay"],"resources":["account:test"]}' WHERE grant_id=$1`, d.Grant)
	f.admit(p, d)
	r := f.result(p, "REJECTED", "provider", "0")
	r.ParamsDigest = "sha256:" + strings.Repeat("a", 64)
	_, err = f.s.Resolve(f.ctx, f.sign(r, "adapter"))
	wantCode(t, err, ResultConflict)
	r.ParamsDigest = p.ParamsDigest
	_, err = f.s.Resolve(f.ctx, f.sign(r, "adapter"))
	if err != nil {
		t.Fatal(err)
	}
	if f.integer("SELECT sum(settled)::bigint FROM budget_accounts WHERE task_id=$1", p.TaskID) != 0 || f.integer("SELECT count(*) FROM budget_reservations WHERE effect_id=$1 AND state='HELD'", p.EffectID) != 0 {
		t.Fatal("definitive nonexecution did not release holds")
	}
}

func (f *fixture) submit(cmd Command) (Admission, error) {
	return f.s.Submit(f.ctx, f.caller, f.sign(Submission{Kind: "anet.command/1", Caller: f.caller, Command: cmd}, "gateway"))
}

func TestKernelCommandSignatureProtectsExpiry(t *testing.T) {
	f := setup(t)
	cmd := f.command()
	signed := f.sign(Submission{Kind: "anet.command/1", Caller: f.caller, Command: cmd}, "gateway")
	var changed Submission
	if err := json.Unmarshal(signed.Payload, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Command.NotAfter = cmd.NotAfter.Add(time.Hour)
	signed.Payload, _ = json.Marshal(changed)
	_, err := f.s.Submit(f.ctx, f.caller, signed)
	wantCode(t, err, Denied)
	if f.integer("SELECT count(*) FROM tasks WHERE domain_id=$1", f.cfg.Domain) != 0 {
		t.Fatal("forged command accepted")
	}
}

func waitUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected database wait was not observed")
}

func TestKernelLeaseExpiryAfterAccountLockWait(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	signed := f.sign(d, "ics")
	expires := time.Now().UTC().Add(time.Second)
	f.exec("UPDATE leases SET expires_at=$2 WHERE lease_id=$1", p.LeaseID, expires)
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, "SELECT account_id FROM budget_accounts WHERE task_id=$1 FOR UPDATE", p.TaskID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := f.s.Admit(f.ctx, f.caller, p, signed); done <- e }()
	waitUntil(t, func() bool {
		return f.integer("SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'SELECT account_id FROM budget_accounts WHERE account_id=%'") > 0
	})
	time.Sleep(time.Until(expires) + 20*time.Millisecond)
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	wantCode(t, <-done, LeaseInvalid)
	if f.integer("SELECT count(*) FROM effects WHERE task_id=$1", p.TaskID) != 0 {
		t.Fatal("expired lease admitted an effect")
	}
}

func TestKernelInstanceRevocationAfterAccountWait(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	signed := f.sign(d, "ics")
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, "SELECT account_id FROM budget_accounts WHERE task_id=$1 FOR UPDATE", p.TaskID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := f.s.Admit(f.ctx, f.caller, p, signed); done <- e }()
	waitUntil(t, func() bool {
		return f.integer("SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'SELECT account_id FROM budget_accounts WHERE account_id=%'") > 0
	})
	f.exec("UPDATE agent_instances SET revoked_at=clock_timestamp() WHERE instance_id=$1", f.caller.Instance)
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	wantCode(t, <-done, Denied)
	if f.integer("SELECT count(*) FROM effects WHERE task_id=$1", p.TaskID) != 0 {
		t.Fatal("revoked worker admitted after waiting")
	}
}

func TestKernelUnsupportedExecutionCapabilities(t *testing.T) {
	f := setup(t)
	for _, cap := range []string{"local-tx", "idempotent-key"} {
		t.Run(cap, func(t *testing.T) {
			cfg := f.cfg
			cfg.Operations = map[string]Operation{"bank": {Action: "pay", Resource: "account:test", Purpose: "tool_call", Capability: cap, Dimensions: []string{"money"}}}
			if _, err := New(f.db, cfg); !errors.Is(err, Invalid) {
				t.Fatalf("unsupported async capability or missing target TTL accepted: %v", err)
			}
		})
	}
}
