//go:build minio_integration

package archive

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Opt-in gate: the schema runner provides a fixed, independently built MinIO
// binary. This test fails (never silently skips) if that prerequisite is missing.
func TestArchiveMinIO(t *testing.T) {
	binary := os.Getenv("ANET_MINIO_BINARY")
	if binary == "" {
		t.Fatal("ANET_MINIO_BINARY required")
	}
	f := setup(t)
	port := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := l.Addr().String()
		l.Close()
		return address
	}
	address, console := port(), port()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	start := func() func() {
		log, err := os.OpenFile(filepath.Join(dir, "minio.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(f.ctx, binary, "server", data, "--address", address, "--console-address", console)
		cmd.Env = append(os.Environ(), "MINIO_ROOT_USER=archive-integration", "MINIO_ROOT_PASSWORD=archive-integration-secret", "MINIO_BROWSER=off", "MINIO_UPDATE=off")
		cmd.Stdout = log
		cmd.Stderr = log
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		stopped := false
		stop := func() {
			if !stopped {
				stopped = true
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				_ = log.Close()
			}
		}
		t.Cleanup(stop)
		client := &http.Client{Timeout: time.Second}
		deadline := time.Now().Add(25 * time.Second)
		for {
			r, e := client.Get("http://" + address + "/minio/health/ready")
			if e == nil {
				r.Body.Close()
				if r.StatusCode == 200 {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("MinIO readiness timeout")
			}
			time.Sleep(50 * time.Millisecond)
		}
		return stop
	}
	stop := start()
	cfg := S3Config{Endpoint: "http://" + address, Region: "us-east-1", Bucket: "archive-" + f.domain, Prefix: "isolated-database"}
	objects, err := NewS3(cfg, "archive-integration", "archive-integration-secret", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = objects.client.CreateBucket(f.ctx, &s3.CreateBucketInput{Bucket: aws.String(cfg.Bucket)}); err != nil {
		t.Fatal(err)
	}
	if _, err = objects.client.PutBucketVersioning(f.ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(cfg.Bucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		t.Fatal(err)
	}
	f.s, err = New(f.db, objects, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.partition("task_events", 5)
	f.partition("audit_log", 3)
	original := f.read(8)
	if err = f.s.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	ch, err := scanChunk(f.db.QueryRow(f.ctx, "SELECT "+chunkColumns+" FROM archive_chunks WHERE job_id IN (SELECT job_id FROM archive_jobs WHERE partition_name=ANY($1)) ORDER BY ordinal LIMIT 1", f.parts))
	if err != nil {
		t.Fatal(err)
	}
	if ch.Version == "" {
		t.Fatal("MinIO object version was not persisted")
	}
	if _, err = objects.Put(f.ctx, ch.Key, bytes.NewReader([]byte("replacement")), 11); err != nil {
		t.Fatal(err)
	}
	o, err := objects.Open(f.ctx, ch.Key, "")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(o.Body)
	o.Body.Close()
	if err != nil || o.Version != ch.Version || int64(len(body)) != ch.Size {
		t.Fatal("conditional PUT overwrote archive", err)
	}
	// An independently privileged fixture writer changes the current version;
	// archival reads must stay pinned to the verified original version.
	if _, err = objects.client.PutObject(f.ctx, &s3.PutObjectInput{Bucket: aws.String(cfg.Bucket), Key: aws.String(cfg.Prefix + "/" + ch.Key), Body: bytes.NewReader([]byte("later-version"))}); err != nil {
		t.Fatal(err)
	}
	stop()
	_ = start()
	got := f.read(8)
	for i := range original {
		if original[i] != got[i] {
			t.Fatal("MinIO restart/version changed original record")
		}
	}
	wrong, err := NewS3(cfg, "archive-integration", "wrong-secret", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if object, err := wrong.Open(f.ctx, ch.Key, ch.Version); err == nil {
		object.Body.Close()
		t.Fatal("MinIO accepted wrong signing key")
	}
}
