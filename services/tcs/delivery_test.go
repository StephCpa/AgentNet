package tcs

import (
	"testing"

	"github.com/agentnet/agentnet/services/delivery"
)

func TestKernelDurableNotificationFlow(t *testing.T) {
	f := setup(t)
	p, d := f.prepare()
	f.admit(p, d)
	if _, err := f.s.Cancel(f.ctx, f.caller, p.TaskID); err != nil {
		t.Fatal(err)
	}
	q, err := delivery.New(f.db, delivery.Config{Domain: f.cfg.Domain, Destination: "local-control"})
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := delivery.NewInbox(f.db, f.cfg.Domain, "local-control")
	if err != nil {
		t.Fatal(err)
	}
	delivered := 0
	for {
		work, err := q.Once(f.ctx, inbox)
		if err != nil {
			t.Fatal(err)
		}
		if !work {
			break
		}
		delivered++
	}
	if delivered != 3 {
		t.Fatalf("expected T1/T2/T3 notifications, got %d", delivered)
	}
	if f.integer(`SELECT count(DISTINCT event_type) FROM delivery_inbox WHERE source_domain=$1 AND event_type IN ('TASK_ACCEPTED','EFFECT_ADMITTED','CANCELLED')`, f.cfg.Domain) != 3 {
		t.Fatal("missing notification")
	}
	if f.integer(`SELECT count(*) FROM outbox WHERE task_id=$1 AND status='DONE'`, p.TaskID) != 3 {
		t.Fatal("outbox not acknowledged")
	}
	// Transport acceptance must not be interpreted as business completion or
	// permission to release holds, including after task cancellation.
	if f.integer(`SELECT count(*) FROM effects WHERE effect_id=$1 AND state='ADMITTED'`, p.EffectID) != 1 || f.integer(`SELECT count(*) FROM budget_reservations WHERE effect_id=$1 AND state='HELD'`, p.EffectID) != 4 {
		t.Fatal("notification changed business facts")
	}
	if _, err := f.s.Resolve(f.ctx, f.sign(f.result(p, "COMMITTED", "provider", "2"), "adapter")); err != nil {
		t.Fatal(err)
	}
	if f.integer(`SELECT count(*) FROM effects WHERE effect_id=$1 AND state='COMMITTED'`, p.EffectID) != 1 {
		t.Fatal("late result missing")
	}
}
