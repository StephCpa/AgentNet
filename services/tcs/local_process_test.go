package tcs

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLocalProcessKillBeforeAndAfterCommit(t *testing.T) {
	for _, phase := range []string{"before_commit", "after_commit"} {
		t.Run(phase, func(t *testing.T) {
			f, p, d, raw := localFixture(t)
			signed := f.sign(d, "ics")
			app := id("faultlab")
			// Hold the final local_effect_results insert. Earlier business and ledger writes are still uncommitted.
			lock, err := f.db.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback(f.ctx)
			if phase == "before_commit" {
				if _, err = lock.Exec(f.ctx, "LOCK TABLE local_effect_results IN SHARE MODE"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err = lock.Rollback(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			input, err := json.Marshal(processSubmission{Config: f.cfg, Caller: f.caller, Envelope: signed, Local: &processLocal{Proposal: p, Parameters: raw}})
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
				for f.integer(`SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE 'INSERT INTO local_effect_results%'`, app) != 1 {
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
				localEmpty(t, f, p)
			}
			recovered, err := f.s.ExecuteLocal(f.ctx, f.caller, p, signed, raw)
			if err != nil || recovered.Effect.State != "COMMITTED" || recovered.Effect.Existing != (phase == "after_commit") {
				t.Fatal(recovered, err)
			}
			if f.integer("SELECT version FROM local_records WHERE domain_id=$1", f.cfg.Domain) != 1 || f.integer("SELECT sum(settled)::bigint FROM budget_accounts WHERE task_id=$1", p.TaskID) != 1 || f.integer("SELECT count(*) FROM local_effect_results WHERE effect_id=$1", p.EffectID) != 1 {
				t.Fatal("duplicate or missing local fact")
			}
		})
	}
}
