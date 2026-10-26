package archive

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type childInput struct {
	S3               S3Config
	Config           Config
	Partition, Phase string
}

func TestArchiveProcessHelper(t *testing.T) {
	if os.Getenv("ANET_ARCHIVE_CHILD") == "" {
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
	objects, err := NewS3(input.S3, "archive-fixture", "fixture-secret", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(db, objects, input.Config)
	if err != nil {
		t.Fatal(err)
	}
	for {
		worked, err := s.Step(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			t.Fatal("no archive progress")
		}
		var state string
		if err = db.QueryRow(ctx, "SELECT state FROM archive_jobs WHERE partition_name=$1", input.Partition).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == input.Phase {
			fmt.Println("ARCHIVE_BOUNDARY")
			for {
				time.Sleep(time.Hour)
			}
		}
	}
}
func TestArchiveProcessKillAndCLIRecovery(t *testing.T) {
	for _, phase := range []string{"DETACHED", "UPLOADED", "EXPORTED", "COMPLETE"} {
		t.Run(phase, func(t *testing.T) {
			f := setup(t)
			part := f.partition("task_events", 3)
			if phase == "UPLOADED" {
				f.b.gate = make(chan struct{})
				f.b.hit = make(chan struct{}, 1)
				t.Cleanup(func() {
					f.b.mu.Lock()
					if f.b.gate != nil {
						close(f.b.gate)
						f.b.gate = nil
					}
					f.b.mu.Unlock()
				})
			}
			raw, _ := json.Marshal(childInput{S3: f.s3, Config: f.cfg, Partition: part, Phase: phase})
			child := exec.CommandContext(f.ctx, os.Args[0], "-test.run=^TestArchiveProcessHelper$", "-test.v")
			child.Env = append(os.Environ(), "ANET_ARCHIVE_CHILD=1")
			child.Stdin = strings.NewReader(string(raw))
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
					if scanner.Text() == "ARCHIVE_BOUNDARY" {
						ready <- true
						return
					}
				}
				ready <- false
			}()
			if phase == "UPLOADED" {
				select {
				case <-f.b.hit:
				case <-time.After(10 * time.Second):
					t.Fatal("upload boundary not reached")
				}
			} else {
				select {
				case ok := <-ready:
					if !ok {
						t.Fatal("child failed before boundary")
					}
				case <-time.After(10 * time.Second):
					t.Fatal("boundary timeout")
				}
			}
			if err = child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = child.Wait(); err == nil {
				t.Fatal("child not killed")
			}
			if phase == "UPLOADED" {
				f.b.mu.Lock()
				close(f.b.gate)
				f.b.gate = nil
				f.b.mu.Unlock()
			}
			// Observe the old session releasing its advisory lock, rather than racing reconnect.
			until := time.Now().Add(5 * time.Second)
			for {
				c, err := f.s.lock(f.ctx)
				if err == nil {
					unlock(c)
					break
				}
				if err != ErrBusy || time.Now().After(until) {
					t.Fatal(err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if phase != "COMPLETE" && f.count("SELECT count(*) FROM "+`public."`+part+`"`) != 3 {
				t.Fatal("source lost before complete")
			}
			f.read(3)
			if err = f.s.RunOnce(f.ctx); err != nil {
				t.Fatal(err)
			}
			f.read(3)
		})
	}
	// The shipped CLI uses the same S3 SDK over HTTP and the persisted catalog.
	t.Run("shipped_cli", func(t *testing.T) {
		if os.Getenv("ANET_TEST_DSN") == "" {
			t.Skip("requires isolated PostgreSQL schema runner")
		}
		dir := t.TempDir()
		binary := filepath.Join(dir, "archived.exe")
		buildCtx, cancelBuild := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancelBuild()
		if output, err := exec.CommandContext(buildCtx, "go", "build", "-o", binary, "../../cmd/archived").CombinedOutput(); err != nil {
			t.Fatalf("build: %v %s", err, output)
		}
		f := setup(t)
		f.partition("audit_log", 3)
		raw, _ := json.Marshal(map[string]any{"work_dir": f.cfg.WorkDir, "chunk_rows": 2, "s3": f.s3})
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		run := func(args ...string) []byte {
			t.Helper()
			cmd := exec.CommandContext(f.ctx, binary, append([]string{"-config", path}, args...)...)
			cmd.Env = append(os.Environ(), "ANET_DATABASE_URL="+os.Getenv("ANET_TEST_DSN"), "AWS_ACCESS_KEY_ID=archive-fixture", "AWS_SECRET_ACCESS_KEY=fixture-secret", "AWS_SESSION_TOKEN=")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("CLI: %v %s", err, output)
			}
			return output
		}
		run("-once")
		if f.count("SELECT count(*) FROM archive_jobs WHERE partition_name=ANY($1) AND state='COMPLETE'", f.parts) != 1 {
			t.Fatal("CLI did not archive source")
		}
		run("-once")
		var got []Record
		if err := json.Unmarshal(run("-task", f.task, "-domain", f.domain), &got); err != nil || len(got) != 3 {
			t.Fatal(len(got), err)
		}
	})
}
