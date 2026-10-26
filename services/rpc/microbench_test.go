//go:build microbench

package rpc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/agentnet/agentnet/internal/canonical"
	gwv1 "github.com/agentnet/agentnet/proto/gen/anet/gw/v1"
	icsv1 "github.com/agentnet/agentnet/proto/gen/anet/ics/v1"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	"github.com/agentnet/agentnet/services/ics"
	"github.com/agentnet/agentnet/services/tcs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// This opt-in measurement harness uses the existing real TCP/mTLS RPC fixture.
// It is deliberately excluded from correctness-test counts and default gates.
type microSample struct {
	Index       int              `json:"index"`
	Nanoseconds int64            `json:"nanoseconds"`
	Calls       int              `json:"rpc_calls"`
	ProtoBytes  int              `json:"uncompressed_protobuf_bytes"`
	Error       string           `json:"error,omitempty"`
	Stages      map[string]int64 `json:"stage_nanoseconds"`
}
type microRun struct {
	Scenario            string        `json:"scenario"`
	Concurrency         int           `json:"concurrency"`
	Repetition          int           `json:"repetition"`
	Warmup              int           `json:"warmup"`
	ElapsedNS           int64         `json:"elapsed_ns"`
	Successes           int           `json:"successes"`
	Errors              int           `json:"errors"`
	OperationsPerSecond float64       `json:"successful_operations_per_second"`
	P50US               float64       `json:"p50_us_all_attempts"`
	P95US               float64       `json:"p95_us_all_attempts"`
	P99US               float64       `json:"p99_us_all_attempts"`
	FactsVerified       bool          `json:"facts_verified"`
	TasksBefore         int           `json:"database_tasks_before"`
	Samples             []microSample `json:"samples"`
}

func microRPC[T proto.Message](s *microSample, stage string, req proto.Message, call func() (T, error)) (T, error) {
	s.Calls++
	s.ProtoBytes += proto.Size(req)
	start := time.Now()
	out, err := call()
	s.Stages[stage] += time.Since(start).Nanoseconds()
	if err == nil {
		s.ProtoBytes += proto.Size(out)
	}
	if err != nil {
		return out, fmt.Errorf("%s: %w", stage, err)
	}
	return out, nil
}

func microEnv(t *testing.T, name string, fallback, max int) int {
	t.Helper()
	if os.Getenv(name) == "" {
		return fallback
	}
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil || n < 1 || n > max {
		t.Fatalf("invalid %s", name)
	}
	return n
}

func TestRPCMicrobenchmark(t *testing.T) {
	if os.Getenv("ANET_TEST_DSN") == "" || os.Getenv("ANET_MICRO_REPORT") == "" {
		t.Fatal("isolated schema runner and ANET_MICRO_REPORT required")
	}
	n := microEnv(t, "ANET_MICRO_SAMPLES", 200, 1000)
	repeats := microEnv(t, "ANET_MICRO_REPETITIONS", 3, 10)
	const warm = 20
	report := struct {
		Format  string            `json:"format"`
		At      time.Time         `json:"at"`
		Go      string            `json:"go"`
		OS      string            `json:"os"`
		Arch    string            `json:"arch"`
		CPUs    int               `json:"logical_cpus"`
		Procs   int               `json:"gomaxprocs"`
		PoolMax int32             `json:"pool_max_connections"`
		PG      map[string]string `json:"postgres_settings"`
		Runs    []microRun        `json:"runs"`
	}{Format: "anet.microbench/1", At: time.Now().UTC(), Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), Procs: runtime.GOMAXPROCS(0)}
	// Always preserve completed runs, including errors; never report successes only.
	defer func() {
		raw, err := json.MarshalIndent(report, "", "  ")
		if err == nil {
			err = os.WriteFile(os.Getenv("ANET_MICRO_REPORT"), append(raw, '\n'), 0600)
		}
		if err != nil {
			t.Error(err)
		}
	}()
	cases := []struct {
		name        string
		concurrency int
	}{}
	for _, name := range []string{"gateway_new", "gateway_replay", "reserve_shared_account", "local_roundtrip"} {
		for _, c := range []int{1, 8} {
			cases = append(cases, struct {
				name        string
				concurrency int
			}{name, c})
		}
	}
	for rep := 0; rep < repeats; rep++ {
		// Rotate order between repetitions; all input sizes stay fixed.
		for offset := range cases {
			c := cases[(offset+rep)%len(cases)]
			t.Run(fmt.Sprintf("%s/c%d/r%d", c.name, c.concurrency, rep+1), func(t *testing.T) {
				f := setup(t)
				if report.PG == nil {
					report.PoolMax = f.db.Config().MaxConns
					report.PG = map[string]string{}
					for _, key := range []string{"server_version", "fsync", "synchronous_commit", "full_page_writes", "shared_buffers", "max_connections", "timezone"} {
						var value string
						if err := f.db.QueryRow(f.ctx, "SELECT current_setting($1)", key).Scan(&value); err != nil {
							t.Fatal(err)
						}
						report.PG[key] = value
					}
				}
				r := microRun{Scenario: c.name, Concurrency: c.concurrency, Repetition: rep + 1, Warmup: warm, TasksBefore: f.count("SELECT count(*) FROM tasks")}
				defer func() { report.Runs = append(report.Runs, r) }()
				microCase(t, f, &r, n, warm)
			})
		}
	}
}

func microCase(t *testing.T, f *fixture, r *microRun, n, warm int) {
	t.Helper()
	total := n + warm
	if r.Scenario == "local_roundtrip" {
		addRecords(f)
		var err error
		f.store, err = tcs.New(f.db, f.cfg)
		if err != nil {
			t.Fatal(err)
		}
		f.ics, err = ics.New(f.db, ics.Config{Domain: f.domain, KeyID: "ics", Key: f.icsKey, CommandKeys: f.cfg.CommandKeys, Operations: f.cfg.Operations})
		if err != nil {
			t.Fatal(err)
		}
	}
	if r.Scenario == "reserve_shared_account" {
		f.command.Limits["calls"] = strconv.Itoa(total)
		limits, _ := json.Marshal(f.command.Limits)
		f.exec("UPDATE root_consents SET task_limits=$2::jsonb,version=version+1 WHERE consent_id=$1", f.consent, string(limits))
	}
	g := gatewayClient(t, f, 0)
	// gatewayClient starts the complete updated service on the same listener.
	conn := f.connect(f.workerCert)
	control := tcsv1.NewTaskControlClient(conn)
	policy := icsv1.NewIdentityPolicyClient(conn)
	requests := make([]*gwv1.SubmitCommandRequest, total)
	params := make([][]byte, total)
	digests := make([]string, total)
	for i := range requests {
		f.command.ID = fmt.Sprintf("micro-%s-%d", f.domain, i)
		f.command.ContractID = fmt.Sprintf("contract-%s-%d", f.domain, i)
		requests[i] = intent(f)
		params[i] = []byte(fmt.Sprintf(`{"key":"micro-%d","expected_version":0,"value":{"payload":"%s"}}`, i, string(makePayload())))
		var err error
		digests[i], err = canonical.Digest(params[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	seed := make([]*tcsv1.AdmissionResult, total)
	if r.Scenario == "gateway_replay" || r.Scenario == "reserve_shared_account" {
		count := total
		if r.Scenario == "reserve_shared_account" {
			count = 1
		}
		for i := 0; i < count; i++ {
			var err error
			seed[i], err = g.SubmitCommand(f.ctx, requests[i])
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	invoke := func(i int) microSample {
		s := microSample{Index: i - warm, Stages: map[string]int64{}}
		ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
		defer cancel()
		start := time.Now()
		err := func() error {
			if r.Scenario == "reserve_shared_account" {
				req := &tcsv1.ReserveBudgetRequest{TaskId: seed[0].TaskId, Dimension: "calls", Purpose: "tool_call", UpperBound: "1"}
				_, err := microRPC(&s, "reserve", req, func() (*tcsv1.Reservation, error) { return control.ReserveBudget(ctx, req) })
				return err
			}
			a, err := microRPC(&s, "gateway", requests[i], func() (*tcsv1.AdmissionResult, error) { return g.SubmitCommand(ctx, requests[i]) })
			if err != nil {
				return err
			}
			if a.TaskId == "" || a.AcceptedReceipt == nil {
				return fmt.Errorf("missing admission facts")
			}
			if r.Scenario == "gateway_replay" {
				if a.Outcome != tcsv1.AdmissionResult_EXISTING || a.TaskId != seed[i].TaskId || !proto.Equal(a.AcceptedReceipt, seed[i].AcceptedReceipt) {
					return fmt.Errorf("replay changed receipt/task")
				}
				return nil
			}
			if a.Outcome != tcsv1.AdmissionResult_NEW {
				return fmt.Errorf("new command not accepted: %v", a.Outcome)
			}
			if r.Scenario != "local_roundtrip" {
				return nil
			}
			lr := &tcsv1.AcquireLeaseRequest{TaskId: a.TaskId}
			lease, err := microRPC(&s, "lease", lr, func() (*tcsv1.Lease, error) { return control.AcquireLease(ctx, lr) })
			if err != nil {
				return err
			}
			dr := &icsv1.DecideRequest{GrantId: a.RootGrantId, TaskId: a.TaskId, TargetRef: "records", ParamsDigest: digests[i], AudienceDomainId: f.domain, Sensitivity: "sensitive", DataClass: "internal"}
			d, err := microRPC(&s, "decide", dr, func() (*icsv1.Decision, error) { return policy.Decide(ctx, dr) })
			if err != nil {
				return err
			}
			p := &tcsv1.EffectProposal{EffectId: "local-" + a.TaskId, TaskId: a.TaskId, CommandId: fmt.Sprintf("micro-%s-%d", f.domain, i), TargetRef: "records", Capability: "local-tx", ParamsDigest: digests[i], AuthorityDigest: d.AuthorityDigest, Epoch: lease.Epoch, Sensitivity: "sensitive", DataClass: "internal"}
			for _, dim := range []string{"money", "tokens", "calls", "messages"} {
				rr := &tcsv1.ReserveBudgetRequest{TaskId: a.TaskId, Dimension: dim, Purpose: "tool_call", UpperBound: "2"}
				h, err := microRPC(&s, "reserve", rr, func() (*tcsv1.Reservation, error) { return control.ReserveBudget(ctx, rr) })
				if err != nil {
					return err
				}
				p.Reservations = append(p.Reservations, &tcsv1.DimensionalReservation{Dimension: dim, ReservationRef: h.ReservationId, UpperBound: "1"})
			}
			er := &tcsv1.ExecuteLocalEffectRequest{Admission: &tcsv1.AdmitEffectRequest{Effect: p, LeaseId: lease.LeaseId, ExpectedTaskVersion: lease.TaskVersion, SignedDecision: d.SignedDecision}, ParametersJson: params[i]}
			out, err := microRPC(&s, "execute", er, func() (*tcsv1.LocalEffectResult, error) { return control.ExecuteLocalEffect(ctx, er) })
			if err != nil {
				return err
			}
			if out.GetAdmission().GetState() != "COMMITTED" || out.GetUsage()["calls"] != "1" {
				return fmt.Errorf("local result not committed/metered")
			}
			return nil
		}()
		s.Nanoseconds = time.Since(start).Nanoseconds()
		if err != nil {
			s.Error = err.Error()
		}
		return s
	}
	runBatch := func(first, count int) ([]microSample, int64) {
		samples := make([]microSample, count)
		jobs := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < r.Concurrency; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					samples[i-first] = invoke(i)
				}
			}()
		}
		start := time.Now()
		for i := first; i < first+count; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		return samples, time.Since(start).Nanoseconds()
	}
	// Warm the same concurrency (including pool connections) as measurement.
	warmSamples, _ := runBatch(0, warm)
	for _, s := range warmSamples {
		if s.Error != "" {
			t.Fatalf("warmup: %s", s.Error)
		}
	}
	r.Samples, r.ElapsedNS = runBatch(warm, n)
	latencies := make([]int64, n)
	for i, s := range r.Samples {
		latencies[i] = s.Nanoseconds
		if s.Error == "" {
			r.Successes++
		} else {
			r.Errors++
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	percentile := func(q float64) float64 { return float64(latencies[int(math.Ceil(q*float64(n)))-1]) / 1000 }
	r.P50US, r.P95US, r.P99US = percentile(.5), percentile(.95), percentile(.99)
	r.OperationsPerSecond = float64(r.Successes) / (float64(r.ElapsedNS) / 1e9)
	if r.Errors > 0 {
		t.Errorf("%d/%d measured operations failed; retained in report", r.Errors, n)
		return
	}
	expectedTasks := total
	if r.Scenario == "reserve_shared_account" {
		expectedTasks = 1
	}
	if f.count("SELECT count(*) FROM tasks WHERE domain_id=$1", f.domain) != expectedTasks {
		t.Error("task facts differ from workload")
		return
	}
	if r.Scenario == "reserve_shared_account" {
		if f.count("SELECT count(*) FROM budget_reservations WHERE holder_task_id=$1 AND state='HELD'", seed[0].TaskId) != total {
			t.Error("held reservations differ")
			return
		}
		_, err := control.ReserveBudget(f.ctx, &tcsv1.ReserveBudgetRequest{TaskId: seed[0].TaskId, Dimension: "calls", Purpose: "tool_call", UpperBound: "1"})
		if status.Code(err) != codes.ResourceExhausted {
			t.Errorf("overflow probe: %v", err)
			return
		}
	}
	if r.Scenario == "local_roundtrip" {
		if f.count("SELECT count(*) FROM local_records WHERE domain_id=$1", f.domain) != total || f.count("SELECT count(*) FROM local_effect_results r JOIN effects e USING(effect_id) JOIN tasks t USING(task_id) WHERE t.domain_id=$1 AND e.state='COMMITTED'", f.domain) != total {
			t.Error("business/result facts differ")
			return
		}
		if f.count("SELECT count(*) FROM budget_accounts a JOIN tasks t ON t.task_id=a.task_id WHERE t.domain_id=$1 AND (a.settled<>CASE WHEN a.dimension='calls' THEN 1 ELSE 0 END)", f.domain) != 0 {
			t.Error("settled ledger differs")
			return
		}
	}
	r.FactsVerified = true
	t.Logf("%s c=%d n=%d %.1f op/s p50=%.1fus p95=%.1fus p99=%.1fus", r.Scenario, r.Concurrency, n, r.OperationsPerSecond, r.P50US, r.P95US, r.P99US)
}

func makePayload() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = 'x'
	}
	return b
}
