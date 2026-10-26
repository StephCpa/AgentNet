package tcs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	localadapter "github.com/agentnet/agentnet/adapters/local"
	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/internal/localtx"
)

func localFixture(t *testing.T) (*fixture, Proposal, Decision, []byte) {
	f := setup(t)
	op := f.cfg.Operations["bank"]
	op.Action, op.Resource, op.Capability = "record.put", "records:test", "local-tx"
	f.cfg.Operations["bank"] = op
	f.cfg.LocalHandlers = map[string]localtx.Handler{"bank": localadapter.Records{}}
	var err error
	f.s, err = New(f.db, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, d := f.prepare()
	f.exec(`UPDATE grants SET rights='{"actions":["record.put"],"resources":["records:test"]}' WHERE grant_id=$1`, d.Grant)
	d.Action, d.Resource = op.Action, op.Resource
	raw := []byte(`{"key":"one","expected_version":0,"value":{"text":"atomic"}}`)
	p.ParamsDigest, _ = canonical.Digest(raw)
	d.ParamsDigest = p.ParamsDigest
	return f, p, d, raw
}

func localEmpty(t *testing.T, f *fixture, p Proposal) {
	t.Helper()
	if f.integer("SELECT count(*) FROM effects WHERE effect_id=$1", p.EffectID) != 0 || f.integer("SELECT count(*) FROM local_records WHERE domain_id=$1", f.cfg.Domain) != 0 || f.integer("SELECT count(*) FROM local_effect_results WHERE effect_id=$1", p.EffectID) != 0 || f.integer("SELECT sum(settled)::bigint FROM budget_accounts WHERE task_id=$1", p.TaskID) != 0 || f.integer("SELECT count(*) FROM budget_reservations r JOIN budget_accounts a USING(account_id) WHERE a.task_id=$1 AND r.state='HELD' AND r.effect_id IS NULL", p.TaskID) != 4 || f.integer("SELECT task_version FROM tasks WHERE task_id=$1", p.TaskID) != p.ExpectedVersion {
		t.Fatal("local transaction partially persisted")
	}
}

func TestLocalAtomicReplayAndBoundParameters(t *testing.T) {
	f, p, d, raw := localFixture(t)
	signed := f.sign(d, "ics")
	first, err := f.s.ExecuteLocal(f.ctx, f.caller, p, signed, raw)
	if err != nil || first.Effect.State != "COMMITTED" || first.Effect.Existing {
		t.Fatal(first, err)
	}
	// Even an expired decision / lease may retrieve an already committed fact.
	f.exec("UPDATE leases SET expires_at=clock_timestamp()-interval '1 second' WHERE lease_id=$1", p.LeaseID)
	d.ExpiresAt = time.Now().Add(-time.Hour)
	again, err := f.s.ExecuteLocal(f.ctx, f.caller, p, f.sign(d, "ics"), raw)
	if err != nil || !again.Effect.Existing || string(again.Result.Payload) == "" || again.ResultRef != first.ResultRef {
		t.Fatal(again, err)
	}
	a, _ := canonical.Digest(first.Result.Payload)
	b, _ := canonical.Digest(again.Result.Payload)
	if a != b {
		t.Fatal("replay changed result")
	}
	_, err = f.s.ExecuteLocal(f.ctx, f.caller, p, signed, []byte(`{"key":"two"}`))
	wantCode(t, err, DecisionMismatch)
	p.ParamsDigest, _ = canonical.Digest([]byte(`{"key":"two"}`))
	_, err = f.s.ExecuteLocal(f.ctx, f.caller, p, signed, []byte(`{"key":"two"}`))
	wantCode(t, err, EffectConflict)
	if f.integer("SELECT version FROM local_records WHERE domain_id=$1", f.cfg.Domain) != 1 || f.integer("SELECT sum(settled)::bigint FROM budget_accounts WHERE task_id=$1", p.TaskID) != 1 || f.integer("SELECT count(*) FROM budget_reservations WHERE effect_id=$1 AND state='SETTLED'", p.EffectID) != 4 || f.integer("SELECT count(*) FROM effect_execution WHERE effect_id=$1", p.EffectID) != 0 || f.integer("SELECT count(*) FROM outbox WHERE dedup_key=$1", "effect:"+p.EffectID) != 0 {
		t.Fatal("wrong atomic facts or external dispatch")
	}
}

type meteredRecords struct {
	localadapter.Records
	calls string
}

func (h meteredRecords) Execute(ctx context.Context, db localtx.DB, r localtx.Request) (localtx.Result, error) {
	result, err := h.Records.Execute(ctx, db, r)
	result.Usage["calls"] = h.calls
	return result, err
}

func TestLocalRollbackOnInvalidExecution(t *testing.T) {
	for _, mode := range []string{"async", "over_bound", "over_hold", "bad_meter", "bad_body", "audit_failure"} {
		t.Run(mode, func(t *testing.T) {
			f, p, d, raw := localFixture(t)
			want := Invalid
			switch mode {
			case "over_bound", "over_hold", "bad_meter":
				usage := "2"
				p.UpperBounds["calls"] = "1"
				if mode == "over_hold" {
					usage = "5"
				}
				if mode == "bad_meter" {
					usage = "-1"
				} else {
					want = BudgetExhausted
				}
				f.cfg.LocalHandlers["bank"] = meteredRecords{calls: usage}
				var err error
				f.s, err = New(f.db, f.cfg)
				if err != nil {
					t.Fatal(err)
				}
			case "bad_body":
				raw = []byte(`{"key":"one","expected_version":0,"value":1,"resource":"records:other"}`)
				p.ParamsDigest, _ = canonical.Digest(raw)
				d.ParamsDigest = p.ParamsDigest
			case "audit_failure":
				name := "local_audit_" + strings.TrimPrefix(id("x"), "x_")
				f.exec(fmt.Sprintf("ALTER TABLE audit_log ADD CONSTRAINT %s CHECK(effect_id IS DISTINCT FROM '%s' OR action<>'budget.settle')", name, p.EffectID))
				t.Cleanup(func() { f.db.Exec(context.Background(), "ALTER TABLE audit_log DROP CONSTRAINT "+name) })
			}
			var err error
			if mode == "async" {
				_, err = f.s.Admit(f.ctx, f.caller, p, f.sign(d, "ics"))
			} else {
				_, err = f.s.ExecuteLocal(f.ctx, f.caller, p, f.sign(d, "ics"), raw)
			}
			if mode == "audit_failure" {
				if err == nil {
					t.Fatal("audit failure accepted")
				}
			} else {
				wantCode(t, err, want)
			}
			localEmpty(t, f, p)
		})
	}
}

func TestLocalConcurrentRecordCAS(t *testing.T) {
	f, p, d, raw := localFixture(t)
	// Separate task locks, shared business key: exactly one create succeeds.
	q, e := f.prepare()
	f.exec(`UPDATE grants SET rights='{"actions":["record.put"],"resources":["records:test"]}' WHERE grant_id=$1`, e.Grant)
	e.Action, e.Resource = d.Action, d.Resource
	q.ParamsDigest, e.ParamsDigest = p.ParamsDigest, p.ParamsDigest
	type answer struct {
		r   LocalEffect
		err error
	}
	ch := make(chan answer, 2)
	s1, s2 := f.sign(d, "ics"), f.sign(e, "ics")
	go func() { r, err := f.s.ExecuteLocal(f.ctx, f.caller, p, s1, raw); ch <- answer{r, err} }()
	go func() { r, err := f.s.ExecuteLocal(f.ctx, f.caller, q, s2, raw); ch <- answer{r, err} }()
	states := map[string]int{}
	for range 2 {
		a := <-ch
		if a.err != nil {
			t.Fatal(a.err)
		}
		states[a.r.Effect.State]++
	}
	if states["COMMITTED"] != 1 || states["REJECTED"] != 1 || f.integer("SELECT version FROM local_records WHERE domain_id=$1", f.cfg.Domain) != 1 || f.integer("SELECT sum(settled)::bigint FROM budget_accounts WHERE task_id IN ($1,$2)", p.TaskID, q.TaskID) != 2 {
		t.Fatal(states)
	}
}

func TestLocalReplaySurvivesBusinessUpdate(t *testing.T) {
	f, p, d, raw := localFixture(t)
	first, err := f.s.ExecuteLocal(f.ctx, f.caller, p, f.sign(d, "ics"), raw)
	if err != nil {
		t.Fatal(err)
	}
	q, e := f.prepare()
	f.exec(`UPDATE grants SET rights='{"actions":["record.put"],"resources":["records:test"]}' WHERE grant_id=$1`, e.Grant)
	e.Action, e.Resource = d.Action, d.Resource
	next := []byte(`{"key":"one","expected_version":1,"value":{"text":"updated"}}`)
	q.ParamsDigest, _ = canonical.Digest(next)
	e.ParamsDigest = q.ParamsDigest
	updated, err := f.s.ExecuteLocal(f.ctx, f.caller, q, f.sign(e, "ics"), next)
	if err != nil || updated.Effect.State != "COMMITTED" {
		t.Fatal(updated, err)
	}
	again, err := f.s.ExecuteLocal(f.ctx, f.caller, p, f.sign(d, "ics"), raw)
	a, _ := canonical.Digest(first.Result.Payload)
	b, _ := canonical.Digest(again.Result.Payload)
	if err != nil || !again.Effect.Existing || a != b || f.integer("SELECT version FROM local_records WHERE domain_id=$1", f.cfg.Domain) != 2 {
		t.Fatal(again, err)
	}
}

func TestLocalRechecksAfterBusinessLock(t *testing.T) {
	for _, mode := range []string{"lease", "decision", "grant", "credential", "deadline", "revocation"} {
		t.Run(mode, func(t *testing.T) {
			f, p, d, raw := localFixture(t)
			locker, err := f.db.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer locker.Rollback(f.ctx)
			k, _ := json.Marshal([]string{"anet.record.put/v1", f.cfg.Domain, "records:test", "one"})
			if _, err = locker.Exec(f.ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", string(k)); err != nil {
				t.Fatal(err)
			}
			expires := time.Now().Add(800 * time.Millisecond)
			want := Denied
			switch mode {
			case "lease":
				f.exec("UPDATE leases SET expires_at=$2 WHERE lease_id=$1", p.LeaseID, expires)
				want = LeaseInvalid
			case "decision":
				d.ExpiresAt = expires
				want = DecisionMismatch
			case "grant":
				f.exec("UPDATE grants SET expires_at=$2 WHERE grant_id=$1", d.Grant, expires)
			case "credential":
				f.caller.CredentialExpiresAt = &expires
			case "deadline":
				f.exec("UPDATE tasks SET deadline=$2,task_version=task_version+1 WHERE task_id=$1", p.TaskID, expires)
				p.ExpectedVersion++
				want = Deadline
			}
			ch := make(chan error, 1)
			signed := f.sign(d, "ics")
			go func() { _, err := f.s.ExecuteLocal(f.ctx, f.caller, p, signed, raw); ch <- err }()
			until := time.Now().Add(5 * time.Second)
			for f.integer("SELECT count(*) FROM pg_stat_activity WHERE wait_event='advisory' AND query LIKE 'SELECT pg_advisory_xact_lock%'") == 0 {
				if time.Now().After(until) {
					t.Fatal("handler never waited")
				}
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(time.Until(expires) + 30*time.Millisecond)
			if mode == "revocation" {
				f.exec("UPDATE agent_instances SET revoked_at=clock_timestamp() WHERE instance_id=$1", f.caller.Instance)
			}
			if err = locker.Rollback(f.ctx); err != nil {
				t.Fatal(err)
			}
			wantCode(t, <-ch, want)
			localEmpty(t, f, p)
		})
	}
}

func TestLocalCancelSerializesWithBusinessCommit(t *testing.T) {
	f, p, d, raw := localFixture(t)
	lock, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(f.ctx)
	k, _ := json.Marshal([]string{"anet.record.put/v1", f.cfg.Domain, "records:test", "one"})
	if _, err = lock.Exec(f.ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", string(k)); err != nil {
		t.Fatal(err)
	}
	executed := make(chan error, 1)
	signed := f.sign(d, "ics")
	go func() { _, err := f.s.ExecuteLocal(f.ctx, f.caller, p, signed, raw); executed <- err }()
	until := time.Now().Add(5 * time.Second)
	for f.integer("SELECT count(*) FROM pg_stat_activity WHERE wait_event='advisory' AND query LIKE 'SELECT pg_advisory_xact_lock%'") == 0 {
		if time.Now().After(until) {
			t.Fatal("business lock not observed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancelled := make(chan error, 1)
	go func() { _, err := f.s.Cancel(f.ctx, f.caller, p.TaskID); cancelled <- err }()
	for f.integer("SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'SELECT status,task_version,current_epoch%'") == 0 {
		if time.Now().After(until) {
			t.Fatal("cancellation did not wait")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err = lock.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-executed; err != nil {
		t.Fatal(err)
	}
	if err = <-cancelled; err != nil {
		t.Fatal(err)
	}
	if f.integer("SELECT count(*) FROM tasks WHERE task_id=$1 AND status='CANCELLED'", p.TaskID) != 1 || f.integer("SELECT count(*) FROM effects WHERE effect_id=$1 AND state='COMMITTED'", p.EffectID) != 1 || f.integer("SELECT version FROM local_records WHERE domain_id=$1", f.cfg.Domain) != 1 {
		t.Fatal("cancellation erased a committed fact")
	}
	// Cancellation also prevents a later new local effect.
	p.EffectID = id("eff")
	_, err = f.s.ExecuteLocal(f.ctx, f.caller, p, signed, raw)
	wantCode(t, err, Terminal)
}
