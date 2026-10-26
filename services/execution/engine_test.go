package execution_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentnet/agentnet/adapters/mockbiz"
	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/services/delivery"
	"github.com/agentnet/agentnet/services/execution"
	"github.com/agentnet/agentnet/services/tcs"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	t                           *testing.T
	ctx                         context.Context
	db                          *pgxpool.Pool
	store                       *tcs.Store
	engine                      *execution.Engine
	domain, task, effect, grant string
	caller                      tcs.Caller
	key                         ed25519.PrivateKey
	cfg                         tcs.Config
	ecfg                        execution.Config
	params                      execution.Files
	client                      mockbiz.Client
	server                      *httptest.Server
	cap                         string
}

func setup(t *testing.T, cap string) *fixture {
	t.Helper()
	if os.Getenv("ANET_TEST_DSN") == "" {
		t.Skip("requires test/schema/run.py --kernel-tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	db, err := pgxpool.New(ctx, os.Getenv("ANET_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	var uid string
	if err = db.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ctx: ctx, db: db, domain: "domain-" + uid, effect: "effect-" + uid, grant: "grant-" + uid, caller: tcs.Caller{Principal: "principal-" + uid, Agent: "agent-" + uid, Instance: "instance-" + uid}, key: key, params: execution.Files{Root: t.TempDir()}, cap: cap}
	f.exec(`INSERT INTO domains(domain_id,name,gateway_endpoint,trust_anchor) VALUES($1,'test','https://invalid.test','{}')`, f.domain)
	f.exec(`INSERT INTO principals(principal_id,kind,domain_id) VALUES($1,'user',$2)`, f.caller.Principal, f.domain)
	f.exec(`INSERT INTO agents(agent_id,domain_id,owner_principal_id,handle,card_digest) VALUES($1,$2,$3,$1,'test')`, f.caller.Agent, f.domain, f.caller.Principal)
	f.exec(`INSERT INTO agent_instances(instance_id,agent_id,cred_hash) VALUES($1,$2,'test')`, f.caller.Instance, f.caller.Agent)
	f.cfg = tcs.Config{Domain: f.domain, CommandKeys: map[string]ed25519.PublicKey{"gateway": pub}, DecisionKeys: map[string]ed25519.PublicKey{"ics": pub}, ResultKeys: map[string]ed25519.PublicKey{"adapter": pub}, Operations: map[string]tcs.Operation{"bank": {Action: "pay", Resource: "account:test", Purpose: "tool_call", Capability: cap, Dimensions: []string{"money", "tokens", "calls", "messages"}, TargetTTL: time.Hour}}}
	f.store, err = tcs.New(db, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	contract, _ := json.Marshal(map[string]any{"deadline": time.Now().UTC().Add(time.Hour), "input_refs": []any{map[string]any{"version": 1, "digest": "sha256:" + strings.Repeat("a", 64)}}, "output_schema": map[string]any{"type": "object"}, "acceptance_policy": map[string]any{"verifier": "test", "checks": []string{"schema"}, "on_fail": "report"}})
	cmd := tcs.Command{ID: "command-" + uid, Audience: f.domain, NotAfter: time.Now().UTC().Add(time.Hour), ContractID: "contract-" + uid, ContractVersion: 1, Contract: contract, Limits: map[string]string{"money": "10", "tokens": "10", "calls": "10", "messages": "10"}}
	admitted, err := f.store.Submit(ctx, f.caller, f.sign(tcs.Submission{Kind: "anet.command/1", Caller: f.caller, Command: cmd}, "gateway"))
	if err != nil {
		t.Fatal(err)
	}
	f.task = admitted.TaskID
	lease, err := f.store.AcquireLease(ctx, f.caller, f.task)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := f.params.Put(ctx, []byte(`{"action":"pay","amount":"2","account_ref":"mock-account"}`))
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO grants(grant_id,root_grant_id,task_id,subject_principal_id,actor_agent_id,rights,not_before,expires_at,policy_version) VALUES($1,$1,$2,$3,$4,'{"actions":["pay"],"resources":["account:test"]}',clock_timestamp()-interval '1 minute',clock_timestamp()+interval '1 hour','test')`, f.grant, f.task, f.caller.Principal, f.caller.Agent)
	proposal := tcs.Proposal{EffectID: f.effect, TaskID: f.task, CommandID: cmd.ID, Target: "bank", ParamsDigest: digest, AuthorityDigest: "sha256:" + strings.Repeat("b", 64), LeaseID: lease.ID, Epoch: lease.Epoch, ExpectedVersion: lease.Version, Sensitivity: "sensitive", Reservations: map[string]string{}, UpperBounds: map[string]string{}}
	for _, dim := range []string{"money", "tokens", "calls", "messages"} {
		hold, err := f.store.Reserve(ctx, f.caller, f.task, dim, "tool_call", "4")
		if err != nil {
			t.Fatal(err)
		}
		proposal.Reservations[dim] = hold.ID
		proposal.UpperBounds[dim] = "3"
	}
	decision := tcs.Decision{Kind: "anet.authorization/1", ID: "decision-" + uid, Allow: true, At: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute), Subject: f.caller.Principal, Agent: f.caller.Agent, Instance: f.caller.Instance, Grant: f.grant, Task: f.task, Target: "bank", Action: "pay", Resource: "account:test", ParamsDigest: digest, AuthorityDigest: proposal.AuthorityDigest, Audience: f.domain, Sensitivity: "sensitive"}
	if _, err = f.store.Admit(ctx, f.caller, proposal, f.sign(decision, "ics")); err != nil {
		t.Fatal(err)
	}
	queue, err := delivery.New(db, delivery.Config{Domain: f.domain, Destination: "local-control"})
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := delivery.NewInbox(db, f.domain, "local-control")
	if err != nil {
		t.Fatal(err)
	}
	for {
		worked, err := queue.Once(ctx, inbox)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	if err = mockbiz.Install(ctx, db); err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewServer(&mockbiz.Server{DB: db, Namespace: f.domain, Capability: cap})
	t.Cleanup(f.server.Close)
	f.client = mockbiz.Client{URL: f.server.URL, HTTP: f.server.Client()}
	f.ecfg = execution.Config{Domain: f.domain, ResultKeyID: "adapter", ResultKey: key, Targets: map[string]execution.Binding{"bank": {Capability: cap, Target: f.client}}, Payloads: f.params, Resolver: f.store, Lease: time.Second, Backoff: []time.Duration{time.Millisecond}, RecoveryWindow: time.Hour}
	f.reset(f.client)
	return f
}
func (f *fixture) reset(target execution.Target) {
	f.t.Helper()
	f.ecfg.Targets = map[string]execution.Binding{"bank": {Capability: f.cap, Target: target}}
	var err error
	f.engine, err = execution.New(f.db, f.ecfg)
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}
func (f *fixture) sign(v any, key string) tcs.Envelope {
	f.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Fatal(err)
	}
	b, err = canonical.Bytes(b)
	if err != nil {
		f.t.Fatal(err)
	}
	return tcs.Envelope{KeyID: key, Payload: b, Signature: ed25519.Sign(f.key, b)}
}
func (f *fixture) due() {
	f.exec(`UPDATE effect_execution SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE effect_id=$1`, f.effect)
}
func (f *fixture) run() {
	f.t.Helper()
	if err := f.engine.Execute(f.ctx, f.effect); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) business() int {
	return f.count(`SELECT count(*) FROM mockbiz.calls WHERE namespace=$1`, f.domain)
}
func (f *fixture) held() int {
	return f.count(`SELECT count(*) FROM budget_reservations WHERE effect_id=$1 AND state='HELD'`, f.effect)
}

type faulty struct {
	execution.Target
	send  func(context.Context, execution.Request) (execution.Observation, error)
	query func(context.Context, execution.Request) (execution.Observation, error)
}

func (f faulty) Send(c context.Context, r execution.Request) (execution.Observation, error) {
	if f.send != nil {
		return f.send(c, r)
	}
	return f.Target.Send(c, r)
}
func (f faulty) Query(c context.Context, r execution.Request) (execution.Observation, error) {
	if f.query != nil {
		return f.query(c, r)
	}
	return f.Target.Query(c, r)
}
func unknown(r execution.Request) execution.Observation {
	return execution.Observation{Status: execution.Unknown, EffectID: r.EffectID, AttemptID: r.AttemptID, ParamsDigest: r.ParamsDigest, Evidence: "mock:unknown"}
}

func TestExecutionThreeCapabilities(t *testing.T) {
	for _, cap := range []string{"queryable", "opaque", "idempotent-key"} {
		t.Run(cap, func(t *testing.T) {
			f := setup(t, cap)
			f.run()
			f.run()
			if f.business() != 1 || f.held() != 0 || f.count(`SELECT sum(settled)::int FROM budget_accounts WHERE task_id=$1`, f.task) != 8 {
				t.Fatal("business or budget mismatch")
			}
		})
	}
}
func TestExecutionIdempotentLostAck(t *testing.T) {
	f := setup(t, "idempotent-key")
	calls := 0
	f.reset(faulty{Target: f.client, send: func(c context.Context, r execution.Request) (execution.Observation, error) {
		calls++
		o, err := f.client.Send(c, r)
		if calls == 1 && err == nil {
			return execution.Observation{}, io.ErrUnexpectedEOF
		}
		return o, err
	}, query: func(c context.Context, r execution.Request) (execution.Observation, error) { return unknown(r), nil }})
	f.run()
	if f.business() != 1 || f.held() != 4 {
		t.Fatal("lost ACK not uncertain")
	}
	f.due()
	f.run()
	if calls != 2 || f.business() != 1 || f.held() != 0 {
		t.Fatal("idempotent retry duplicated or failed")
	}
}
func TestExecutionKeyTTLStopsRetry(t *testing.T) {
	f := setup(t, "idempotent-key")
	sends := 0
	f.reset(faulty{Target: f.client, send: func(c context.Context, r execution.Request) (execution.Observation, error) {
		sends++
		return execution.Observation{}, io.ErrUnexpectedEOF
	}})
	f.run()
	f.exec(`UPDATE effect_execution SET key_expires_at=clock_timestamp()-interval '1 second' WHERE effect_id=$1`, f.effect)
	f.due()
	f.run()
	if sends != 1 || f.business() != 0 || f.held() != 4 || f.count(`SELECT count(*) FROM effect_execution WHERE effect_id=$1 AND escalated`, f.effect) != 1 {
		t.Fatal("expired key retried or hold released")
	}
}
func TestExecutionOpaqueLostAckEscalates(t *testing.T) {
	f := setup(t, "opaque")
	f.reset(faulty{Target: f.client, send: func(c context.Context, r execution.Request) (execution.Observation, error) {
		_, err := f.client.Send(c, r)
		if err != nil {
			return execution.Observation{}, err
		}
		return execution.Observation{}, io.ErrUnexpectedEOF
	}})
	f.run()
	if f.business() != 1 || f.held() != 4 {
		t.Fatal("opaque uncertainty lost")
	}
	if err := f.engine.Execute(f.ctx, f.effect); !errors.Is(err, execution.ErrBusy) {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM tasks WHERE task_id=$1 AND status='WAITING'`, f.task) != 1 || f.count(`SELECT count(*) FROM outbox WHERE task_id=$1 AND event_type='RECOVERY_ESCALATED'`, f.task) != 1 {
		t.Fatal("no escalation")
	}
}
func TestExecutionQueryableAbsentPendingAndLateCommit(t *testing.T) {
	f := setup(t, "queryable")
	release := make(chan struct{})
	committed := make(chan struct{})
	var once sync.Once
	delayed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/send" {
			b, _ := io.ReadAll(r.Body)
			<-release
			r = r.Clone(context.Background())
			r.Body = io.NopCloser(bytes.NewReader(b))
			(&mockbiz.Server{DB: f.db, Namespace: f.domain, Capability: f.cap}).ServeHTTP(w, r)
			once.Do(func() { close(committed) })
			return
		}
		(&mockbiz.Server{DB: f.db, Namespace: f.domain, Capability: f.cap}).ServeHTTP(w, r)
	}))
	defer delayed.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f.reset(mockbiz.Client{URL: delayed.URL, HTTP: delayed.Client()})
	f.run()
	for _, status := range []string{execution.Absent, execution.Pending, execution.Unknown} {
		f.exec(`INSERT INTO mockbiz.query_modes(namespace,effect_id,mode) VALUES($1,$2,$3) ON CONFLICT(namespace,effect_id) DO UPDATE SET mode=EXCLUDED.mode`, f.domain, f.effect, status)
		f.due()
		f.run()
		if f.business() != 0 || f.count(`SELECT send_count FROM effect_execution WHERE effect_id=$1`, f.effect) != 1 || f.held() != 4 {
			t.Fatal("query absence caused replay", status)
		}
	}
	close(release)
	select {
	case <-committed:
	case <-time.After(3 * time.Second):
		t.Fatal("late commit failed")
	}
	f.due()
	f.run()
	if f.business() != 1 || f.held() != 0 {
		t.Fatal("late fact not reconciled")
	}
}
func TestExecutionFinalProofTombstonesOldAttempt(t *testing.T) {
	f := setup(t, "queryable")
	var old execution.Request
	f.reset(faulty{Target: f.client, send: func(c context.Context, r execution.Request) (execution.Observation, error) {
		old = r
		return execution.Observation{}, io.ErrUnexpectedEOF
	}})
	f.run()
	f.exec(`INSERT INTO mockbiz.query_modes(namespace,effect_id,mode) VALUES($1,$2,'NOT_EXECUTED_FINAL')`, f.domain, f.effect)
	f.due()
	f.run()
	if f.held() != 0 || f.count(`SELECT count(*) FROM effects WHERE effect_id=$1 AND state='REJECTED'`, f.effect) != 1 {
		t.Fatal("final proof not applied")
	}
	result, err := f.client.Send(f.ctx, old)
	if err != nil || result.Status != execution.Final || f.business() != 0 {
		t.Fatal("final proof allowed delayed old attempt", result, err)
	}
}
func TestExecutionDispatchFences(t *testing.T) {
	edits := map[string]string{"epoch": `UPDATE tasks SET current_epoch=current_epoch+1,task_version=task_version+1 WHERE task_id=$1`, "cancel": `UPDATE tasks SET status='CANCELLED',cancel_version=1,cancelled_at=clock_timestamp(),task_version=task_version+1 WHERE task_id=$1`, "grant": `UPDATE grants SET revoked_at=clock_timestamp(),status='revoked' WHERE task_id=$1`, "rights": `UPDATE grants SET rights='{"actions":[],"resources":[]}' WHERE task_id=$1`, "instance": `UPDATE agent_instances SET revoked_at=clock_timestamp() WHERE instance_id IN (SELECT dispatch_actor_instance_id FROM effects WHERE task_id=$1)`}
	for name, sql := range edits {
		t.Run(name, func(t *testing.T) {
			f := setup(t, "queryable")
			f.exec(sql, f.task)
			f.run()
			if f.business() != 0 || f.held() != 4 {
				t.Fatal("fenced send executed")
			}
		})
	}
}
func TestExecutionPayloadDigestAndTargetBinding(t *testing.T) {
	t.Run("payload", func(t *testing.T) {
		f := setup(t, "queryable")
		files, err := filepath.Glob(filepath.Join(f.params.Root, "*.json"))
		if err != nil || len(files) != 1 {
			t.Fatal(files, err)
		}
		if err = os.WriteFile(files[0], []byte(`{"amount":"999"}`), 0600); err != nil {
			t.Fatal(err)
		}
		f.run()
		if f.business() != 0 || f.held() != 4 {
			t.Fatal("tampered parameters sent")
		}
	})
	t.Run("capability", func(t *testing.T) {
		f := setup(t, "queryable")
		f.ecfg.Targets["bank"] = execution.Binding{Capability: "opaque", Target: f.client}
		var err error
		f.engine, err = execution.New(f.db, f.ecfg)
		if err != nil {
			t.Fatal(err)
		}
		f.run()
		if f.business() != 0 {
			t.Fatal("capability mismatch sent")
		}
	})
}
func TestExecutionLateRecoveryAfterCancel(t *testing.T) {
	f := setup(t, "queryable")
	f.reset(faulty{Target: f.client, send: func(c context.Context, r execution.Request) (execution.Observation, error) {
		_, err := f.client.Send(c, r)
		if err != nil {
			return execution.Observation{}, err
		}
		return execution.Observation{}, io.ErrUnexpectedEOF
	}})
	f.run()
	if _, err := f.store.Cancel(f.ctx, f.caller, f.task); err != nil {
		t.Fatal(err)
	}
	f.due()
	f.run()
	if f.business() != 1 || f.held() != 0 || f.count(`SELECT count(*) FROM tasks WHERE task_id=$1 AND status='CANCELLED'`, f.task) != 1 {
		t.Fatal("cancel broke recovery")
	}
}

func TestExecutionPreSendLookupFailureRecovers(t *testing.T) {
	f := setup(t, "queryable")
	first := true
	f.reset(faulty{Target: f.client, query: func(c context.Context, r execution.Request) (execution.Observation, error) {
		if first {
			first = false
			return execution.Observation{}, io.ErrUnexpectedEOF
		}
		return f.client.Query(c, r)
	}})
	f.run()
	if f.business() != 0 || f.count(`SELECT count(*) FROM effects WHERE effect_id=$1 AND state='ADMITTED'`, f.effect) != 1 {
		t.Fatal("lookup failure invented uncertain write")
	}
	f.due()
	f.run()
	if f.business() != 1 || f.held() != 0 {
		t.Fatal("pre-send lookup did not recover")
	}
	if f.count(`SELECT count(*) FROM reconciliations WHERE effect_id=$1`, f.effect) != 2 {
		t.Fatal("query audit missing")
	}
}
func TestExecutionIdempotentWithoutQueryAPI(t *testing.T) {
	f := setup(t, "idempotent-key")
	first := true
	f.reset(faulty{Target: f.client, send: func(c context.Context, r execution.Request) (execution.Observation, error) {
		o, err := f.client.Send(c, r)
		if first && err == nil {
			first = false
			return execution.Observation{}, io.ErrUnexpectedEOF
		}
		return o, err
	}, query: func(context.Context, execution.Request) (execution.Observation, error) {
		return execution.Observation{}, errors.New("query unsupported")
	}})
	f.run()
	f.due()
	f.run()
	if f.business() != 1 || f.held() != 0 {
		t.Fatal("key-only target did not recover")
	}
}

type unavailableResolver struct{}

func (unavailableResolver) Resolve(context.Context, tcs.Envelope) (tcs.Effect, error) {
	return tcs.Effect{}, io.ErrUnexpectedEOF
}
func TestExecutionSavedReceiptSurvivesReporterFailure(t *testing.T) {
	f := setup(t, "opaque")
	f.ecfg.Resolver = unavailableResolver{}
	f.reset(f.client)
	if err := f.engine.Execute(f.ctx, f.effect); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	if f.business() != 1 || f.held() != 4 || f.count(`SELECT count(*) FROM effect_execution WHERE effect_id=$1 AND result_payload IS NOT NULL`, f.effect) != 1 {
		t.Fatal("terminal receipt not durable")
	}
	f.exec(`UPDATE effect_execution SET lease_until=clock_timestamp()-interval '1 second' WHERE effect_id=$1`, f.effect)
	f.ecfg.Resolver = f.store
	f.reset(faulty{Target: f.client, send: func(context.Context, execution.Request) (execution.Observation, error) {
		t.Fatal("receipt replay re-sent business operation")
		return execution.Observation{}, nil
	}, query: func(context.Context, execution.Request) (execution.Observation, error) {
		t.Fatal("opaque receipt replay queried target")
		return execution.Observation{}, nil
	}})
	f.run()
	if f.business() != 1 || f.held() != 0 {
		t.Fatal("saved receipt not settled")
	}
}
func TestExecutionRecoveryWindowAndBadUsage(t *testing.T) {
	t.Run("window", func(t *testing.T) {
		f := setup(t, "queryable")
		f.reset(faulty{Target: f.client, send: func(context.Context, execution.Request) (execution.Observation, error) {
			return execution.Observation{}, io.ErrUnexpectedEOF
		}})
		f.run()
		f.exec(`UPDATE effect_execution SET created_at=clock_timestamp()-interval '2 hours' WHERE effect_id=$1`, f.effect)
		f.due()
		f.run()
		if f.count(`SELECT count(*) FROM effect_execution WHERE effect_id=$1 AND escalated`, f.effect) != 1 || f.held() != 4 {
			t.Fatal("recovery window did not escalate")
		}
	})
	t.Run("usage", func(t *testing.T) {
		f := setup(t, "queryable")
		f.reset(faulty{Target: f.client, send: func(c context.Context, r execution.Request) (execution.Observation, error) {
			o, err := f.client.Send(c, r)
			o.Usage["money"] = "-100"
			return o, err
		}})
		f.run()
		if f.business() != 1 || f.held() != 4 || f.count(`SELECT count(*) FROM effect_execution WHERE effect_id=$1 AND result_payload IS NULL AND escalated`, f.effect) != 1 {
			t.Fatal("invalid usage accepted or poisoned saved receipt")
		}
	})
}
func TestExecutionSchedulerAndClaimExclusion(t *testing.T) {
	f := setup(t, "queryable")
	entered := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f.reset(faulty{Target: f.client, send: func(c context.Context, r execution.Request) (execution.Observation, error) {
		close(entered)
		<-release
		return f.client.Send(c, r)
	}})
	done := make(chan error, 1)
	go func() { _, err := f.engine.Step(f.ctx); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler never sent")
	}
	if err := f.engine.Execute(f.ctx, f.effect); !errors.Is(err, execution.ErrBusy) {
		t.Fatal("live claim duplicated", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if work, err := f.engine.Step(f.ctx); err != nil || work {
		t.Fatal("finished job rescheduled", err)
	}
	if f.business() != 1 {
		t.Fatal("duplicate effect")
	}
}

type childInput struct {
	Config                        tcs.Config
	Key                           ed25519.PrivateKey
	Root, URL, Effect, Capability string
}

func TestExecutionUnknownWithoutSendProvenance(t *testing.T) {
	f := setup(t, "queryable")
	var digest string
	if err := f.db.QueryRow(f.ctx, `SELECT params_digest FROM effects WHERE effect_id=$1`, f.effect).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	r := tcs.Result{Kind: "anet.result/1", Task: f.task, Effect: f.effect, ParamsDigest: digest, Audience: f.domain, Outcome: "UNKNOWN", Evidence: "trusted:prior-attempt-outside-current-dispatcher"}
	if _, err := f.store.Resolve(f.ctx, f.sign(r, "adapter")); err != nil {
		t.Fatal(err)
	}
	f.run()
	if f.business() != 0 || f.held() != 4 || f.count(`SELECT send_count FROM effect_execution WHERE effect_id=$1`, f.effect) != 0 {
		t.Fatal("UNKNOWN without provenance was executed")
	}
}
func TestExecutionParameterStoreConcurrentPublish(t *testing.T) {
	store := execution.Files{Root: t.TempDir()}
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	expected, _ := canonical.Digest([]byte(`{"b":2,"a":1}`))
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			digest, err := store.Put(context.Background(), []byte(`{"a":1,"b":2}`))
			if err == nil && digest != expected {
				err = errors.New("digest mismatch")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	b, err := store.Load(context.Background(), expected)
	if err != nil || string(b) != `{"a":1,"b":2}` {
		t.Fatal(string(b), err)
	}
	if _, err = store.Load(context.Background(), "../../untrusted"); err == nil {
		t.Fatal("non-digest path accepted")
	}
}

func TestExecutionFinalProofScopeAcrossAttempts(t *testing.T) {
	for _, scope := range []string{"attempt", "effect"} {
		t.Run(scope, func(t *testing.T) {
			f := setup(t, "idempotent-key")
			var requests []execution.Request
			f.reset(faulty{Target: f.client, send: func(c context.Context, r execution.Request) (execution.Observation, error) {
				requests = append(requests, r)
				return execution.Observation{}, io.ErrUnexpectedEOF
			}})
			f.run()
			f.due()
			f.run()
			if len(requests) != 2 {
				t.Fatal("expected two in-flight attempts")
			}
			mode := execution.Final
			if scope == "effect" {
				mode = "NOT_EXECUTED_FINAL_KEY"
			}
			f.exec(`INSERT INTO mockbiz.query_modes(namespace,effect_id,mode) VALUES($1,$2,$3)`, f.domain, f.effect, mode)
			f.due()
			f.run()
			if scope == "attempt" {
				if f.held() != 4 || f.count(`SELECT count(*) FROM effects WHERE effect_id=$1 AND state='UNKNOWN'`, f.effect) != 1 {
					t.Fatal("last-attempt proof released older uncertainty")
				}
				late, err := f.client.Send(f.ctx, requests[0])
				if err != nil || late.Status != execution.Committed || f.business() != 1 {
					t.Fatal("counterexample did not preserve old in-flight request", late, err)
				}
			} else {
				if f.held() != 0 || f.count(`SELECT count(*) FROM effects WHERE effect_id=$1 AND state='REJECTED'`, f.effect) != 1 {
					t.Fatal("key-wide final proof not accepted")
				}
				for _, req := range requests {
					late, err := f.client.Send(f.ctx, req)
					if err != nil || late.Status != execution.Final {
						t.Fatal("key proof allowed delayed request", late, err)
					}
				}
				if f.business() != 0 {
					t.Fatal("key tombstone failed")
				}
			}
		})
	}
}

func TestExecutionProcessHelper(t *testing.T) {
	if os.Getenv("ANET_EXECUTION_CHILD") == "" {
		return
	}
	var input childInput
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, os.Getenv("ANET_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := tcs.New(db, input.Config)
	if err != nil {
		t.Fatal(err)
	}
	client := mockbiz.Client{URL: input.URL}
	target := faulty{Target: client, send: func(c context.Context, r execution.Request) (execution.Observation, error) {
		o, err := client.Send(c, r)
		if err != nil {
			return o, err
		}
		fmt.Println("EXTERNAL_COMMITTED")
		for {
			time.Sleep(time.Hour)
		}
	}}
	engine, err := execution.New(db, execution.Config{Domain: input.Config.Domain, ResultKeyID: "adapter", ResultKey: input.Key, Targets: map[string]execution.Binding{"bank": {Capability: input.Capability, Target: target}}, Payloads: execution.Files{Root: input.Root}, Resolver: store, Lease: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Execute(ctx, input.Effect); err != nil {
		t.Fatal(err)
	}
	t.Fatal("fault point not reached")
}
func TestExecutionExternalCommitProcessKill(t *testing.T) {
	for _, cap := range []string{"queryable", "idempotent-key", "opaque"} {
		t.Run(cap, func(t *testing.T) {
			f := setup(t, cap)
			input, _ := json.Marshal(childInput{f.cfg, f.key, f.params.Root, f.server.URL, f.effect, cap})
			child := exec.CommandContext(f.ctx, os.Args[0], "-test.run=^TestExecutionProcessHelper$", "-test.v")
			child.Env = append(os.Environ(), "ANET_EXECUTION_CHILD=1")
			child.Stdin = bytes.NewReader(input)
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
					if strings.Contains(scanner.Text(), "EXTERNAL_COMMITTED") {
						ready <- true
						return
					}
				}
				ready <- false
			}()
			select {
			case ok := <-ready:
				if !ok {
					t.Fatal("child failed before external commit")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("child timed out")
			}
			if f.business() != 1 || f.count(`SELECT count(*) FROM effects WHERE effect_id=$1 AND state='ADMITTED'`, f.effect) != 1 || f.count(`SELECT count(*) FROM effect_execution WHERE effect_id=$1 AND result_payload IS NULL AND send_count=1`, f.effect) != 1 {
				t.Fatal("wrong crash window")
			}
			if err = child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = child.Wait(); err == nil {
				t.Fatal("child not killed")
			}
			deadline := time.Now().Add(4 * time.Second)
			for {
				err = f.engine.Execute(f.ctx, f.effect)
				if !errors.Is(err, execution.ErrBusy) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("claim never recovered")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err != nil {
				t.Fatal(err)
			}
			if f.business() != 1 {
				t.Fatal("business effect repeated")
			}
			if cap == "opaque" {
				if f.held() != 4 || f.count(`SELECT count(*) FROM effects WHERE effect_id=$1 AND state='UNKNOWN'`, f.effect) != 1 {
					t.Fatal("opaque fact fabricated")
				}
			} else {
				if f.held() != 0 || f.count(`SELECT count(*) FROM effects WHERE effect_id=$1 AND state='COMMITTED'`, f.effect) != 1 {
					t.Fatal("recoverable fact not reconciled")
				}
			}
		})
	}
}
