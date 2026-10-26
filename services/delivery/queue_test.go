package delivery

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	t            *testing.T
	ctx          context.Context
	db           *pgxpool.Pool
	q            *Queue
	inbox        *Inbox
	domain, task string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	if os.Getenv("ANET_TEST_DSN") == "" {
		t.Skip("requires test/schema/run.py --kernel-tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	db, err := pgxpool.New(ctx, os.Getenv("ANET_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	var unique string
	if err = db.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&unique); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ctx: ctx, db: db, domain: "delivery-" + unique, task: "task-" + unique}
	f.exec(`INSERT INTO domains(domain_id,name,gateway_endpoint,trust_anchor) VALUES($1,'test','https://invalid.test','{}')`, f.domain)
	f.exec(`INSERT INTO tasks(task_id,root_task_id,domain_id,contract_id,contract_version) VALUES($1,$1,$2,'test',1)`, f.task, f.domain)
	f.q, err = New(db, Config{Domain: f.domain, Destination: "local-control", Lease: time.Second, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	f.inbox, err = NewInbox(db, f.domain, "local-control")
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, query, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}
func (f *fixture) add(key string) int64 {
	f.t.Helper()
	var seq int64
	err := f.db.QueryRow(f.ctx, `INSERT INTO outbox(task_id,event_type,payload,dedup_key) VALUES($1,'EFFECT_ADMITTED','{"effect_id":"example"}',$2) RETURNING outbox_seq`, f.task, key).Scan(&seq)
	if err != nil {
		f.t.Fatal(err)
	}
	return seq
}
func (f *fixture) claim() *Claim {
	f.t.Helper()
	c, dead, err := f.q.Claim(f.ctx)
	if err != nil || dead || c == nil {
		f.t.Fatalf("claim=%v dead=%v err=%v", c, dead, err)
	}
	return c
}
func TestDeliveryConcurrentClaims(t *testing.T) {
	f := setup(t)
	for i := 0; i < 12; i++ {
		f.add(fmt.Sprint(i))
	}
	var wg sync.WaitGroup
	got := make(chan int64, 12)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, dead, err := f.q.Claim(f.ctx)
			if err != nil || dead || c == nil {
				errs <- fmt.Errorf("claim: %v", err)
				return
			}
			got <- c.Sequence
		}()
	}
	wg.Wait()
	close(got)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for seq := range got {
		if seen[seq] {
			t.Fatal("double claim")
		}
		seen[seq] = true
	}
	if len(seen) != 12 {
		t.Fatal(len(seen))
	}
	c, _, err := f.q.Claim(f.ctx)
	if err != nil || c != nil {
		t.Fatalf("live lease reclaimed: %v %v", c, err)
	}
}
func TestDeliveryStaleAcknowledgement(t *testing.T) {
	f := setup(t)
	seq := f.add("k")
	old := f.claim()
	f.exec(`UPDATE outbox SET lease_until=clock_timestamp()-interval '1 second' WHERE outbox_seq=$1`, seq)
	if err := f.q.Finish(f.ctx, *old, nil); !errors.Is(err, ErrLeaseLost) {
		t.Fatal(err)
	}
	fresh := f.claim()
	if fresh.Token == old.Token || fresh.Attempt != 2 {
		t.Fatal(fresh)
	}
	if err := f.q.Finish(f.ctx, *old, nil); !errors.Is(err, ErrLeaseLost) {
		t.Fatal(err)
	}
	if err := f.inbox.Publish(f.ctx, fresh.Message); err != nil {
		t.Fatal(err)
	}
	if err := f.q.Finish(f.ctx, *fresh, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.q.Finish(f.ctx, *old, errors.New("late failure")); !errors.Is(err, ErrLeaseLost) {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM outbox WHERE outbox_seq=$1 AND status='DONE'`, seq) != 1 {
		t.Fatal("stale finish changed DONE")
	}
}
func TestDeliveryInboxDuplicateAndConflict(t *testing.T) {
	f := setup(t)
	f.add("key")
	c := f.claim()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- f.inbox.Publish(f.ctx, c.Message) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.count(`SELECT count(*) FROM delivery_inbox WHERE source_domain=$1`, f.domain) != 1 {
		t.Fatal("duplicate acceptance")
	}
	altered := c.Message
	altered.Payload = []byte(`{"effect_id":"changed"}`)
	if err := f.inbox.Publish(f.ctx, altered); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := f.q.Finish(f.ctx, *c, ErrConflict); err != nil {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM delivery_dead_letters WHERE outbox_seq=$1 AND reason='identity_conflict'`, c.Sequence) != 1 {
		t.Fatal("missing alert")
	}
	if f.count(`SELECT count(*) FROM delivery_inbox WHERE source_domain=$1 AND payload->>'effect_id'='example'`, f.domain) != 1 {
		t.Fatal("original overwritten")
	}
}
func TestDeliveryBackoffAndDeadLetter(t *testing.T) {
	f := setup(t)
	seq := f.add("fail")
	for attempt := 1; attempt <= 3; attempt++ {
		c := f.claim()
		if c.Attempt != attempt {
			t.Fatal(c.Attempt)
		}
		if err := f.q.Finish(f.ctx, *c, errors.New("failure with secret")); err != nil {
			t.Fatal(err)
		}
		if attempt < 3 {
			empty, _, err := f.q.Claim(f.ctx)
			if err != nil || empty != nil {
				t.Fatal("backoff ignored", err)
			}
			f.exec(`UPDATE outbox SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE outbox_seq=$1`, seq)
		}
	}
	if f.count(`SELECT count(*) FROM outbox WHERE outbox_seq=$1 AND status='DEAD' AND attempts=3 AND last_failure='attempts_exhausted'`, seq) != 1 {
		t.Fatal("not DEAD")
	}
	if f.count(`SELECT count(*) FROM delivery_dead_letters WHERE outbox_seq=$1`, seq) != 1 {
		t.Fatal("no durable alert")
	}
	if c, dead, err := f.q.Claim(f.ctx); err != nil || dead || c != nil {
		t.Fatal("DEAD retried", err)
	}
	if f.q.delay(1) != 100*time.Millisecond || f.q.delay(3) != 400*time.Millisecond || f.q.delay(100) != time.Second {
		t.Fatal("wrong exponential backoff")
	}
}
func TestDeliveryExpiredFinalAttempt(t *testing.T) {
	f := setup(t)
	seq := f.add("exhausted")
	f.claim()
	f.exec(`UPDATE outbox SET attempts=3,lease_until=clock_timestamp()-interval '1 second' WHERE outbox_seq=$1`, seq)
	c, dead, err := f.q.Claim(f.ctx)
	if err != nil || !dead || c != nil {
		t.Fatal(c, dead, err)
	}
	if f.count(`SELECT count(*) FROM delivery_dead_letters WHERE outbox_seq=$1`, seq) != 1 {
		t.Fatal("lost exhausted crash")
	}
}
func TestDeliveryDomainAndDestinationIsolation(t *testing.T) {
	f := setup(t)
	f.add("k")
	other, err := New(f.db, Config{Domain: "different", Destination: "local-control"})
	if err != nil {
		t.Fatal(err)
	}
	if c, _, err := other.Claim(f.ctx); err != nil || c != nil {
		t.Fatal("cross-domain claim", err)
	}
	route, err := New(f.db, Config{Domain: f.domain, Destination: "other-service"})
	if err != nil {
		t.Fatal(err)
	}
	if c, _, err := route.Claim(f.ctx); err != nil || c != nil {
		t.Fatal("cross-route claim", err)
	}
	c := f.claim()
	if err := other.Finish(f.ctx, *c, nil); !errors.Is(err, ErrLeaseLost) {
		t.Fatal(err)
	}
	c.Message.SourceDomain = "different"
	if err := f.inbox.Publish(f.ctx, c.Message); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
func TestDeliveryRunAndCancellation(t *testing.T) {
	f := setup(t)
	f.add("run")
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.q.Run(ctx, f.inbox) }()
	deadline := time.Now().Add(3 * time.Second)
	for f.count(`SELECT count(*) FROM outbox WHERE task_id=$1 AND status='DONE'`, f.task) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("worker failed to drain")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker ignored cancellation")
	}
}

// This helper is launched as a real OS process. Marker output is an out-of-band
// faultlab signal, not an application ACK. The parent kills the process.
func TestDeliveryProcessHelper(t *testing.T) {
	phase := os.Getenv("ANET_DELIVERY_CHILD")
	if phase == "" {
		return
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, os.Getenv("ANET_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	domain := os.Getenv("ANET_CHILD_DOMAIN")
	q, err := New(db, Config{Domain: domain, Destination: "local-control", Lease: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := q.Claim(ctx)
	if err != nil || c == nil {
		t.Fatalf("child claim: %v", err)
	}
	if phase == "accepted" {
		in, err := NewInbox(db, domain, "local-control")
		if err != nil {
			t.Fatal(err)
		}
		if err = in.Publish(ctx, c.Message); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("FAULT_READY")
	for {
		time.Sleep(time.Hour)
	}
}
func TestDeliveryProcessCrashRecovery(t *testing.T) {
	for _, phase := range []string{"claimed", "accepted"} {
		t.Run(phase, func(t *testing.T) {
			f := setup(t)
			seq := f.add("crash")
			child := exec.CommandContext(f.ctx, os.Args[0], "-test.run=^TestDeliveryProcessHelper$", "-test.v")
			child.Env = append(os.Environ(), "ANET_DELIVERY_CHILD="+phase, "ANET_CHILD_DOMAIN="+f.domain)
			pipe, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			child.Stderr = child.Stdout
			if err = child.Start(); err != nil {
				t.Fatal(err)
			}
			defer child.Process.Kill()
			ready := make(chan bool, 1)
			go func() {
				scanner := bufio.NewScanner(pipe)
				for scanner.Scan() {
					if strings.Contains(scanner.Text(), "FAULT_READY") {
						ready <- true
						return
					}
				}
				ready <- false
			}()
			select {
			case ok := <-ready:
				if !ok {
					t.Fatal("child exited before fault")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("child did not reach fault")
			}
			expected := 0
			if phase == "accepted" {
				expected = 1
			}
			if f.count(`SELECT count(*) FROM delivery_inbox WHERE source_domain=$1`, f.domain) != expected {
				t.Fatal("wrong crash phase")
			}
			if err = child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = child.Wait(); err == nil {
				t.Fatal("child was not killed")
			}
			deadline := time.Now().Add(4 * time.Second)
			for {
				work, err := f.q.Once(f.ctx, f.inbox)
				if err != nil {
					t.Fatal(err)
				}
				if work {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("lease not recovered")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if f.count(`SELECT count(*) FROM delivery_inbox WHERE source_domain=$1`, f.domain) != 1 {
				t.Fatal("lost or duplicate acceptance")
			}
			if f.count(`SELECT count(*) FROM outbox WHERE outbox_seq=$1 AND status='DONE' AND attempts=2`, seq) != 1 {
				t.Fatal("not recovered")
			}
		})
	}
}
