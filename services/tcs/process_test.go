package tcs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type processSubmission struct {
	Config   Config
	Caller   Caller
	Envelope Envelope
}

// This entry point only executes in a child process. The parent kills it at an
// observed database boundary; no production-only fault hook or panic is needed.
func TestKernelProcessHelper(t *testing.T) {
	app := os.Getenv("ANET_TCS_CHILD")
	if app == "" {
		return
	}
	var input processSubmission
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("ANET_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = app
	db, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := New(db, input.Config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Submit(context.Background(), input.Caller, input.Envelope); err != nil {
		t.Fatal(err)
	}
	fmt.Println("FAULT_COMMITTED")
	for {
		time.Sleep(time.Hour)
	}
}

func TestKernelProcessKillBeforeAndAfterCommit(t *testing.T) {
	for _, phase := range []string{"before_commit", "after_commit"} {
		t.Run(phase, func(t *testing.T) {
			f := setup(t)
			cmd := f.command()
			signed := f.sign(Submission{Kind: "anet.command/1", Caller: f.caller, Command: cmd}, "gateway")
			app := id("faultlab")
			// Hold the final outbox insert. Earlier T1 writes are still uncommitted.
			lock, err := f.db.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback(f.ctx)
			if phase == "before_commit" {
				if _, err = lock.Exec(f.ctx, "LOCK TABLE outbox IN SHARE MODE"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err = lock.Rollback(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			input, err := json.Marshal(processSubmission{f.cfg, f.caller, signed})
			if err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(f.ctx, os.Args[0], "-test.run=^TestKernelProcessHelper$", "-test.v")
			child.Env = append(os.Environ(), "ANET_TCS_CHILD="+app)
			child.Stdin = strings.NewReader(string(input))
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
					if strings.Contains(scanner.Text(), "FAULT_COMMITTED") {
						ready <- true
						return
					}
				}
				ready <- false
			}()
			if phase == "before_commit" {
				deadline := time.Now().Add(5 * time.Second)
				for f.integer(`SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE 'INSERT INTO outbox%'`, app) != 1 {
					if time.Now().After(deadline) {
						t.Fatal("child never reached final insert lock")
					}
					time.Sleep(5 * time.Millisecond)
				}
			} else {
				select {
				case ok := <-ready:
					if !ok {
						t.Fatal("child exited before commit")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("child never committed")
				}
			}
			var original Admission
			if phase == "after_commit" {
				err = f.db.QueryRow(f.ctx, `SELECT i.task_id,r.receipt_id FROM command_inbox i JOIN receipts r ON r.task_id=i.task_id WHERE i.command_id=$1`, cmd.ID).Scan(&original.TaskID, &original.ReceiptID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err = child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = child.Wait(); err == nil {
				t.Fatal("child not killed")
			}
			if phase == "before_commit" {
				if err = lock.Rollback(f.ctx); err != nil {
					t.Fatal(err)
				}
				if f.integer("SELECT count(*) FROM tasks WHERE contract_id=$1", cmd.ContractID) != 0 || f.integer("SELECT count(*) FROM command_inbox WHERE command_id=$1", cmd.ID) != 0 || f.integer("SELECT count(*) FROM receipts WHERE command_id=$1", cmd.ID) != 0 {
					t.Fatal("partial T1 survived kill")
				}
			}
			recovered, err := f.s.Submit(f.ctx, f.caller, signed)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "after_commit" && (!recovered.Existing || recovered.TaskID != original.TaskID || recovered.ReceiptID != original.ReceiptID) {
				t.Fatal("retry did not recover original fact")
			}
			if phase == "before_commit" && recovered.Existing {
				t.Fatal("uncommitted task survived")
			}
			if f.integer("SELECT count(*) FROM tasks WHERE contract_id=$1", cmd.ContractID) != 1 || f.integer("SELECT count(*) FROM receipts WHERE command_id=$1", cmd.ID) != 1 || f.integer("SELECT count(*) FROM outbox WHERE task_id=$1", recovered.TaskID) != 1 {
				t.Fatal("duplicate or missing durable fact")
			}
		})
	}
}
