package rpc_test

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

	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	"github.com/agentnet/agentnet/services/tasktree"
	"github.com/agentnet/agentnet/services/tcs"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type treeProcessInput struct {
	Config      tcs.Config
	Caller      tcs.Caller
	Child, Mode string
}

func TestRPCTreeProcessHelper(t *testing.T) {
	app := os.Getenv("ANET_TREE_CHILD")
	if app == "" {
		return
	}
	var input treeProcessInput
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
	if input.Mode == "cancel" {
		w, e := tasktree.New(db, input.Config.Domain)
		if e != nil {
			t.Fatal(e)
		}
		work, e := w.Once(context.Background())
		if e != nil || !work {
			t.Fatal(work, e)
		}
	} else {
		store, e := tcs.New(db, input.Config)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = store.CloseSubtaskBudget(context.Background(), input.Caller, input.Child); e != nil {
			t.Fatal(e)
		}
	}
	fmt.Println("FAULT_COMMITTED")
	for {
		time.Sleep(time.Hour)
	}
}
func TestRPCTreeProcessRecovery(t *testing.T) {
	for _, mode := range []string{"close", "cancel"} {
		for _, phase := range []string{"before", "after"} {
			t.Run(mode+"-"+phase, func(t *testing.T) {
				f := setup(t)
				child, b := createChild(t, f)
				parent := childParent(t, f, child.TaskId)
				q, inbox, w := treeTools(t, f)
				target := child.TaskId
				if mode == "cancel" {
					target = parent
				}
				if _, err := f.control.CancelTask(f.ctx, &tcsv1.CancelTaskRequest{TaskId: target}); err != nil {
					t.Fatal(err)
				}
				if mode == "cancel" {
					for i := 0; i < 20; i++ {
						work, err := q.Once(f.ctx, inbox)
						if err != nil {
							t.Fatal(err)
						}
						if !work {
							break
						}
					}
				}
				lock, err := f.db.Begin(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Rollback(f.ctx)
				if phase == "before" {
					if _, err = lock.Exec(f.ctx, "LOCK TABLE outbox IN SHARE MODE"); err != nil {
						t.Fatal(err)
					}
				} else {
					lock.Rollback(f.ctx)
				}
				app := "tree-" + strings.ReplaceAll(f.domain, "-", "") + "-" + phase
				raw, err := json.Marshal(treeProcessInput{f.cfg, b.caller, child.TaskId, mode})
				if err != nil {
					t.Fatal(err)
				}
				process := exec.CommandContext(f.ctx, os.Args[0], "-test.run=^TestRPCTreeProcessHelper$", "-test.v")
				process.Env = append(os.Environ(), "ANET_TREE_CHILD="+app)
				process.Stdin = strings.NewReader(string(raw))
				pipe, err := process.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				process.Stderr = process.Stdout
				if err = process.Start(); err != nil {
					t.Fatal(err)
				}
				defer process.Process.Kill()
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
				if phase == "before" {
					until := time.Now().Add(10 * time.Second)
					for f.count("SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE 'INSERT INTO outbox%'", app) != 1 {
						if time.Now().After(until) {
							t.Fatal("process did not reach final insert")
						}
						time.Sleep(10 * time.Millisecond)
					}
				} else {
					select {
					case ok := <-ready:
						if !ok {
							t.Fatal("process exited before commit")
						}
					case <-time.After(10 * time.Second):
						t.Fatal("process did not commit")
					}
				}
				if err = process.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				_ = process.Wait()
				if phase == "before" {
					if err = lock.Rollback(f.ctx); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "close" {
					a, err := b.control.CloseSubtaskBudget(b.ctx, &tcsv1.CloseSubtaskBudgetRequest{ChildTaskId: child.TaskId})
					if err != nil || a.Existing != (phase == "after") {
						t.Fatal(a, err)
					}
					if f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", parent) != 0 || f.count("SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='SUBTASK_BUDGET_CLOSED'", child.TaskId) != 1 {
						t.Fatal("closeout did not recover once")
					}
				} else {
					if _, err = w.Once(f.ctx); err != nil {
						t.Fatal(err)
					}
					if f.count("SELECT cancel_version::int FROM tasks WHERE task_id=$1", child.TaskId) != 1 || f.count("SELECT count(*) FROM task_cancellations WHERE task_id=$1 AND state='DONE'", child.TaskId) != 1 || f.count("SELECT sum(delegated)::int FROM budget_accounts WHERE task_id=$1", parent) != 20 {
						t.Fatal("cancel did not recover without releasing budget")
					}
				}
			})
		}
	}
}
func TestRPCTreeDaemonPropagation(t *testing.T) {
	f := setup(t)
	child, _ := createChild(t, f)
	parent := childParent(t, f, child.TaskId)
	binary := filepath.Join(t.TempDir(), "outboxd.exe")
	if output, err := exec.CommandContext(f.ctx, "go", "build", "-o", binary, "../../cmd/outboxd").CombinedOutput(); err != nil {
		t.Fatalf("build outboxd: %v %s", err, output)
	}
	if _, err := f.control.CancelTask(f.ctx, &tcsv1.CancelTaskRequest{TaskId: parent}); err != nil {
		t.Fatal(err)
	}
	process := exec.CommandContext(f.ctx, binary)
	process.Env = append(os.Environ(), "ANET_DATABASE_URL="+os.Getenv("ANET_TEST_DSN"), "ANET_DOMAIN="+f.domain)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = process.Process.Kill(); _ = process.Wait() }()
	until := time.Now().Add(10 * time.Second)
	for f.count("SELECT count(*) FROM tasks WHERE task_id=$1 AND status='CANCELLED'", child.TaskId) != 1 {
		if time.Now().After(until) {
			t.Fatal("outboxd did not propagate cancellation")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if f.count("SELECT count(*) FROM task_cancellations WHERE task_id=$1 AND state='DONE'", child.TaskId) != 1 {
		t.Fatal("worker did not persist completion")
	}
}
func TestRPCTreeCreateCancelRace(t *testing.T) {
	f := setup(t)
	root := delegableRoot(t, f)
	cmd := subCommand(t, f, root, f.adapter, 1, "5")
	child, err := f.control.CreateSubtask(f.ctx, subRequest(t, f, cmd))
	if err != nil {
		t.Fatal(err)
	}
	b := recipientFixture(f)
	nested := subCommand(t, b, child, f.caller, 0, "0")
	nested.ExpiresAt = cmd.ExpiresAt.Add(-time.Second)
	nested.Contract = subContract(t, nested.Contract, 0, nested.ExpiresAt)
	if _, err = f.control.CancelTask(f.ctx, &tcsv1.CancelTaskRequest{TaskId: root.TaskId}); err != nil {
		t.Fatal(err)
	}
	q, inbox, w := treeTools(t, f)
	for i := 0; i < 20; i++ {
		work, err := q.Once(f.ctx, inbox)
		if err != nil {
			t.Fatal(err)
		}
		if !work {
			break
		}
	}
	lock, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(f.ctx)
	if _, err = lock.Exec(f.ctx, "SELECT task_id FROM tasks WHERE task_id=$1 FOR UPDATE", child.TaskId); err != nil {
		t.Fatal(err)
	}
	created := make(chan *tcsv1.AdmissionResult, 1)
	createErr := make(chan error, 1)
	cancelErr := make(chan error, 1)
	req := subRequest(t, b, nested)
	go func() { a, e := b.control.CreateSubtask(b.ctx, req); created <- a; createErr <- e }()
	go func() { _, e := w.Once(f.ctx); cancelErr <- e }()
	waitForLock(t, f, "SELECT status,task_version,current_epoch%")
	waitForLock(t, f, "SELECT status,COALESCE(parent_task_id%")
	if err = lock.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-cancelErr; err != nil {
		t.Fatal(err)
	}
	a, e := <-created, <-createErr
	if e != nil && status.Code(e) != codes.FailedPrecondition && status.Code(e) != codes.Aborted {
		t.Fatal(e)
	}
	drainTree(t, f)
	if e == nil && f.count("SELECT count(*) FROM tasks WHERE task_id=$1 AND status='CANCELLED'", a.TaskId) != 1 {
		t.Fatal("racing child escaped cancellation")
	}
	if f.count("SELECT count(*) FROM tasks WHERE root_task_id=$1 AND status NOT IN ('CANCELLED','SUCCEEDED','FAILED')", root.TaskId) != 0 {
		t.Fatal("tree did not converge")
	}
}
