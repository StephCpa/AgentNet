package archive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type bucket struct {
	mu                  sync.Mutex
	objects             map[string][]byte
	failPut, corruptGet bool
	gate                chan struct{}
	hit                 chan struct{}
	server              *httptest.Server
}

func newBucket(t *testing.T) *bucket {
	b := &bucket{objects: map[string][]byte{}}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=archive-fixture/") || !strings.Contains(r.Header.Get("Authorization"), "/s3/aws4_request") {
			http.Error(w, "unsigned request", 403)
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		switch r.Method {
		case "PUT":
			if r.Header.Get("If-None-Match") != "*" {
				http.Error(w, "create-only required", 400)
				return
			}
			if b.failPut {
				w.WriteHeader(403)
				fmt.Fprint(w, `<Error><Code>AccessDenied</Code></Error>`)
				return
			}
			if _, ok := b.objects[r.URL.Path]; ok {
				w.WriteHeader(412)
				fmt.Fprint(w, `<Error><Code>PreconditionFailed</Code></Error>`)
				return
			}
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			b.objects[r.URL.Path] = raw
			w.Header().Set("x-amz-version-id", "fixture-v1")
			w.WriteHeader(200)
		case "GET":
			if v := r.URL.Query().Get("versionId"); v != "" && v != "fixture-v1" {
				http.NotFound(w, r)
				return
			}
			raw, ok := b.objects[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if b.gate != nil && strings.HasSuffix(r.URL.Path, ".parquet") {
				gate, hit := b.gate, b.hit
				b.mu.Unlock()
				select {
				case hit <- struct{}{}:
				default:
				}
				select {
				case <-gate:
				case <-r.Context().Done():
				}
				b.mu.Lock()
			}
			raw = bytes.Clone(raw)
			if b.corruptGet && len(raw) > 0 {
				raw[0] ^= 0xff
			}
			w.Header().Set("x-amz-version-id", "fixture-v1")
			w.Write(raw)
		default:
			http.Error(w, "unsupported", 405)
		}
	}))
	t.Cleanup(b.server.Close)
	return b
}

type fixture struct {
	t            *testing.T
	ctx          context.Context
	db           *pgxpool.Pool
	s            *Service
	b            *bucket
	cfg          Config
	s3           S3Config
	domain, task string
	month        time.Time
	parts        []string
}

var monthCounter atomic.Int64

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("ANET_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL schema runner")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	f := &fixture{t: t, ctx: ctx, db: db, b: newBucket(t), cfg: Config{WorkDir: t.TempDir(), ChunkRows: 2}, month: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, int(monthCounter.Add(1)), 0)}
	if err = db.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&f.domain); err != nil {
		t.Fatal(err)
	}
	f.task = "archive-task-" + f.domain
	f.exec("INSERT INTO domains(domain_id,name,gateway_endpoint,trust_anchor) VALUES($1,'archive','https://invalid.test','{}')", f.domain)
	f.exec("INSERT INTO tasks(task_id,root_task_id,domain_id,contract_id,contract_version) VALUES($1,$1,$2,'archive-fixture',1)", f.task, f.domain)
	f.s3 = S3Config{Endpoint: f.b.server.URL, Region: "us-east-1", Bucket: "archive", Prefix: f.domain}
	objects, err := NewS3(f.s3, "archive-fixture", "fixture-secret", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.s, err = New(db, objects, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range f.parts {
			_, _ = db.Exec(context.Background(), "DROP TABLE IF EXISTS "+pgx.Identifier{"public", p}.Sanitize())
			_, _ = db.Exec(context.Background(), "DELETE FROM archive_task_index WHERE job_id IN (SELECT job_id FROM archive_jobs WHERE partition_name=$1)", p)
			_, _ = db.Exec(context.Background(), "DELETE FROM archive_chunks WHERE job_id IN (SELECT job_id FROM archive_jobs WHERE partition_name=$1)", p)
			_, _ = db.Exec(context.Background(), "DELETE FROM archive_jobs WHERE partition_name=$1", p)
		}
	})
	return f
}
func (f *fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, q, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) count(q string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.db.QueryRow(f.ctx, q, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}
func (f *fixture) partition(parent string, n int) string {
	f.t.Helper()
	name := parent + "_" + f.month.Format("2006_01")
	f.parts = append(f.parts, name)
	f.exec(fmt.Sprintf("CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')", pgx.Identifier{"public", name}.Sanitize(), pgx.Identifier{"public", parent}.Sanitize(), f.month.Format(time.RFC3339), f.month.AddDate(0, 1, 0).Format(time.RFC3339)))
	for i := 0; i < n; i++ {
		if parent == "task_events" {
			f.exec("INSERT INTO task_events(recorded_at,task_id,root_task_id,event_type,payload) VALUES($1,$2,$2,'ARCHIVE_TEST',jsonb_build_object('n',$3::integer,'nested',jsonb_build_object('text','中文'),'nullable',NULL))", f.month.Add(time.Duration(i)*time.Second), f.task, i)
		} else {
			f.exec("INSERT INTO audit_log(at,domain_id,task_id,action,decision,cost) VALUES($1,$2,$3,'archive.test','record',NULL)", f.month.Add(time.Duration(i)*time.Second), f.domain, f.task)
		}
	}
	return name
}
func (f *fixture) step() {
	f.t.Helper()
	worked, err := f.s.Step(f.ctx)
	if err != nil || !worked {
		f.t.Fatal(worked, err)
	}
}
func (f *fixture) read(n int) []Record {
	f.t.Helper()
	r, err := f.s.ReadTask(f.ctx, f.domain, f.task, 100)
	if err != nil || len(r) != n {
		f.t.Fatal(len(r), err)
	}
	return r
}

func TestArchiveRoundTripHotDetachedCold(t *testing.T) {
	f := setup(t)
	f.partition("task_events", 5)
	f.partition("audit_log", 3)
	f.exec("INSERT INTO task_events(task_id,root_task_id,event_type,payload) VALUES($1,$1,'HOT','{}')", f.task)
	original := f.read(9)
	cfg := f.db.Config()
	cfg.ConnConfig.RuntimeParams["timezone"] = "America/New_York"
	shifted, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(shifted.Close)
	f.s, err = New(shifted, f.s.objects, f.cfg)
	if err != nil {
		t.Fatal(err)
	}

	f.step()
	f.read(9)
	var events string
	if err := f.db.QueryRow(f.ctx, "SELECT partition_name FROM archive_jobs WHERE state='DETACHED'").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(f.ctx, "DELETE FROM "+pgx.Identifier{"public", events}.Sanitize()); err == nil {
		t.Fatal("detached source was writable")
	}
	if _, err := f.db.Exec(f.ctx, "TRUNCATE "+pgx.Identifier{"public", events}.Sanitize()); err == nil {
		t.Fatal("detached source was truncatable")
	}
	if err := f.s.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	got := f.read(9)
	for i := range original {
		if original[i] != got[i] {
			t.Fatalf("archive changed record %d", i)
		}
	}
	if f.count("SELECT count(*) FROM archive_jobs WHERE state='COMPLETE' AND partition_name=ANY($1)", f.parts) != 2 || f.count("SELECT count(*) FROM archive_chunks WHERE job_id IN (SELECT job_id FROM archive_jobs WHERE partition_name=ANY($1))", f.parts) != 5 {
		t.Fatal("wrong checkpoint/partition count")
	}
	if err := f.s.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.read(9)
	if _, err := f.s.ReadTask(f.ctx, "other-domain", f.task, 100); err == nil {
		t.Fatal("cross-domain lookup allowed")
	}
	if _, err := f.s.ReadTask(f.ctx, f.domain, f.task, 2); err == nil {
		t.Fatal("silent record truncation")
	}
}

func TestArchiveFailuresRetainSourceAndResume(t *testing.T) {
	for _, mode := range []string{"upload", "corrupt_download", "missing_object", "catalog_failure", "store_change"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			name := f.partition("task_events", 3)
			f.step()
			f.b.mu.Lock()
			if mode == "upload" {
				f.b.failPut = true
			}
			if mode == "corrupt_download" {
				f.b.corruptGet = true
			}
			f.b.mu.Unlock()
			if mode == "upload" || mode == "corrupt_download" {
				if _, err := f.s.Step(f.ctx); err == nil {
					t.Fatal("storage failure accepted")
				}
			}
			if mode == "store_change" {
				other, _ := NewS3(S3Config{Endpoint: f.b.server.URL, Region: "us-east-1", Bucket: "archive", Prefix: "changed"}, "archive-fixture", "fixture-secret", "", nil)
				worker, _ := New(f.db, other, f.cfg)
				if _, err := worker.Step(f.ctx); err == nil {
					t.Fatal("store binding ignored")
				}
			}
			f.b.mu.Lock()
			f.b.failPut = false
			f.b.corruptGet = false
			f.b.mu.Unlock()
			if mode == "missing_object" || mode == "catalog_failure" {
				for f.count("SELECT count(*) FROM archive_jobs WHERE partition_name=$1 AND state='EXPORTED'", name) == 0 {
					f.step()
				}
				if mode == "missing_object" {
					f.b.mu.Lock()
					var key string
					var saved []byte
					for k, v := range f.b.objects {
						if strings.HasSuffix(k, ".parquet") {
							key = k
							saved = v
							delete(f.b.objects, k)
							break
						}
					}
					f.b.mu.Unlock()
					if _, err := f.s.Step(f.ctx); err == nil {
						t.Fatal("missing archive allowed source deletion")
					}
					f.b.mu.Lock()
					f.b.objects[key] = saved
					f.b.mu.Unlock()
				} else {
					f.exec("ALTER TABLE archive_jobs ADD CONSTRAINT archive_test_fail CHECK(state<>'COMPLETE')")
					_, err := f.s.Step(f.ctx)
					f.exec("ALTER TABLE archive_jobs DROP CONSTRAINT archive_test_fail")
					if err == nil {
						t.Fatal("expected post-DROP catalog failure")
					}
				}
			}
			if f.count("SELECT count(*) FROM "+pgx.Identifier{"public", name}.Sanitize()) != 3 {
				t.Fatal("source lost")
			}
			f.read(3)
			if err := f.s.RunOnce(f.ctx); err != nil {
				t.Fatal(err)
			}
			f.read(3)
		})
	}
}

func TestArchiveLockExclusionAndEmptyPartition(t *testing.T) {
	f := setup(t)
	f.partition("task_events", 0)
	c, err := f.s.lock(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Step(f.ctx); err != ErrBusy {
		unlock(c)
		t.Fatal(err)
	}
	unlock(c)
	if err = f.s.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.read(0)
}

func TestArchiveOversizeRetainsSource(t *testing.T) {
	f := setup(t)
	name := f.partition("task_events", 1)
	f.exec("UPDATE "+pgx.Identifier{"public", name}.Sanitize()+" SET payload=jsonb_build_object('text',$1::text)", strings.Repeat("a", 2000))
	f.cfg.MaxChunkBytes = 1024
	f.s, _ = New(f.db, f.s.objects, f.cfg)
	f.step()
	if _, err := f.s.Step(f.ctx); err == nil {
		t.Fatal("oversize row ignored")
	}
	if f.count("SELECT count(*) FROM "+pgx.Identifier{"public", name}.Sanitize()) != 1 {
		t.Fatal("oversize source lost")
	}
	f.cfg.MaxChunkBytes = 4096
	f.s, _ = New(f.db, f.s.objects, f.cfg)
	if err := f.s.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.read(1)
}

func TestArchiveByteBoundedChunksCommitCursor(t *testing.T) {
	f := setup(t)
	name := f.partition("task_events", 5)
	f.exec("UPDATE "+pgx.Identifier{"public", name}.Sanitize()+" SET payload=jsonb_build_object('text',$1::text)", strings.Repeat("x", 400))
	original := f.read(5)
	f.cfg.MaxChunkBytes = 1024
	f.cfg.ChunkRows = 100
	f.s, _ = New(f.db, f.s.objects, f.cfg)
	if err := f.s.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM archive_chunks WHERE job_id IN (SELECT job_id FROM archive_jobs WHERE partition_name=$1)", name) != 5 {
		t.Fatal("byte-limited chunks did not persist independently")
	}
	got := f.read(5)
	for i := range original {
		if original[i] != got[i] {
			t.Fatalf("byte boundary lost or repeated row %d", i)
		}
	}
}

func TestArchiveCleanupGuards(t *testing.T) {
	f := setup(t)
	for _, item := range []struct{ id, age, expiry, notAfter string }{{"old", "8 days", "1 day", "2 days"}, {"retained", "1 day", "1 hour", "2 hours"}, {"live", "8 days", "-1 day", "-1 hour"}} {
		f.exec(`INSERT INTO command_inbox(command_id,audience_domain_id,task_id,profile,payload_digest,received_at,expires_at,not_after) VALUES($1,$2,$3,'test','test',clock_timestamp()-$4::interval,clock_timestamp()-$5::interval,clock_timestamp()-$6::interval)`, item.id+f.domain, f.domain, f.task, item.age, item.expiry, item.notAfter)
	}
	for _, item := range []struct{ status, age string }{{"DONE", "8 days"}, {"DONE", "1 day"}, {"PENDING", "8 days"}, {"DEAD", "8 days"}} {
		f.exec("INSERT INTO outbox(task_id,event_type,payload,dedup_key,status,dispatched_at) VALUES($1,'TEST','{}',$2,$3,clock_timestamp()-$4::interval)", f.task, item.status+item.age+f.domain, item.status, item.age)
	}
	n, err := f.s.Cleanup(f.ctx)
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if f.count("SELECT count(*) FROM command_inbox WHERE task_id=$1", f.task) != 2 || f.count("SELECT count(*) FROM outbox WHERE task_id=$1", f.task) != 3 {
		t.Fatal("cleanup removed protected data")
	}
}

// Configuration validation is independent of PostgreSQL.
func TestS3RejectsUnsafeConfiguration(t *testing.T) {
	for _, endpoint := range []string{"http://example.com", "https://user:password@example.com", "https://example.com/path"} {
		if _, err := NewS3(S3Config{Endpoint: endpoint, Region: "r", Bucket: "b", Prefix: "p"}, "key", "secret", "", nil); err == nil {
			t.Fatal(endpoint)
		}
	}
}

func TestArchiveCleanupDrainsBacklogBeforeStorageFailure(t *testing.T) {
	f := setup(t)
	name := f.partition("task_events", 1)
	f.exec(`INSERT INTO command_inbox(command_id,audience_domain_id,task_id,profile,payload_digest,received_at,expires_at,not_after)
     SELECT $1||i::text,$1,$2,'test','test',clock_timestamp()-interval '8 days',clock_timestamp()-interval '1 day',clock_timestamp()-interval '2 days' FROM generate_series(1,2001) i`, f.domain, f.task)
	f.exec(`INSERT INTO outbox(task_id,event_type,payload,dedup_key,status,dispatched_at)
     SELECT $1,'TEST','{}',$1||i::text,'DONE',clock_timestamp()-interval '8 days' FROM generate_series(1,2001) i`, f.task)
	f.b.mu.Lock()
	f.b.failPut = true
	f.b.mu.Unlock()
	if err := f.s.RunOnce(f.ctx); err == nil {
		t.Fatal("expected storage failure")
	}
	if f.count("SELECT count(*) FROM command_inbox WHERE task_id=$1", f.task) != 0 || f.count("SELECT count(*) FROM outbox WHERE task_id=$1", f.task) != 0 {
		t.Fatal("cleanup backlog remained behind failed archive")
	}
	if f.count("SELECT count(*) FROM archive_jobs WHERE partition_name=$1 AND state='DETACHED'", name) != 1 {
		t.Fatal("failed archive not retained")
	}
	f.read(1)
	f.b.mu.Lock()
	f.b.failPut = false
	f.b.mu.Unlock()
	if err := f.s.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.read(1)
}

func TestArchiveLookupDuringFinalDrop(t *testing.T) {
	f := setup(t)
	name := f.partition("task_events", 3)
	for f.count("SELECT count(*) FROM archive_jobs WHERE partition_name=$1 AND state='EXPORTED'", name) == 0 {
		f.step()
	}
	locker, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(f.ctx)
	if _, err = locker.Exec(f.ctx, "LOCK TABLE "+pgx.Identifier{"public", name}.Sanitize()+" IN ACCESS SHARE MODE"); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { _, e := f.s.Step(f.ctx); finished <- e }()
	until := time.Now().Add(5 * time.Second)
	wait := func(pattern string) {
		t.Helper()
		for f.count("SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE $1", pattern) == 0 {
			if time.Now().After(until) {
				t.Fatal("expected lock wait: " + pattern)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait("LOCK TABLE " + pgx.Identifier{"public", name}.Sanitize() + " IN ACCESS EXCLUSIVE%")
	type result struct {
		rows []Record
		err  error
	}
	read := make(chan result, 1)
	go func() { r, e := f.s.ReadTask(f.ctx, f.domain, f.task, 10); read <- result{r, e} }()
	wait("SELECT job_id::text,parent_table%FOR SHARE")
	if err = locker.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	got := <-read
	if got.err != nil || len(got.rows) != 3 {
		t.Fatal("lookup gap/duplicate during finalization", len(got.rows), got.err)
	}
}

func TestArchiveRetentionUsesPartitionUpperBound(t *testing.T) {
	f := setup(t)
	for _, item := range []struct {
		table string
		days  int
	}{{"task_events", 180}, {"audit_log", 365}} {
		cutoff := time.Now().UTC().AddDate(0, 0, -item.days)
		f.month = time.Date(cutoff.Year(), cutoff.Month(), 1, 0, 0, 0, 0, time.UTC)
		f.partition(item.table, 1)
	}
	if err := f.s.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM archive_jobs WHERE partition_name=ANY($1)", f.parts) != 0 {
		t.Fatal("partition tail still inside hot retention was detached")
	}
	f.read(2)
}
