package tcs

import (
	"testing"
	"time"

	"github.com/agentnet/agentnet/services/archive"
)

func TestKernelExpiredInboxCleanupCannotRecreateTask(t *testing.T) {
	f := setup(t)
	cmd := f.command()
	cmd.NotAfter = time.Now().Add(2 * time.Second)
	signed := f.sign(Submission{Kind: "anet.command/1", Caller: f.caller, Command: cmd}, "gateway")
	a, err := f.s.Submit(f.ctx, f.caller, signed)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate passage of the retention window without changing the signed expiry.
	f.exec("UPDATE command_inbox SET received_at=clock_timestamp()-interval '8 days',expires_at=not_after WHERE task_id=$1", a.TaskID)
	objects, err := archive.NewS3(archive.S3Config{Endpoint: "http://127.0.0.1:1", Region: "test", Bucket: "unused", Prefix: "unused"}, "unused", "unused", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	maint, err := archive.New(f.db, objects, archive.Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = maint.Cleanup(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.integer("SELECT count(*) FROM command_inbox WHERE task_id=$1", a.TaskID) != 1 {
		t.Fatal("live command lost dedup record")
	}
	time.Sleep(time.Until(cmd.NotAfter) + 50*time.Millisecond)
	if _, err = maint.Cleanup(f.ctx); err != nil {
		t.Fatal(err)
	}
	if f.integer("SELECT count(*) FROM command_inbox WHERE task_id=$1", a.TaskID) != 0 {
		t.Fatal("expired inbox not cleaned")
	}
	_, err = f.s.Submit(f.ctx, f.caller, signed)
	wantCode(t, err, Expired)
	if f.integer("SELECT count(*) FROM tasks WHERE domain_id=$1", f.cfg.Domain) != 1 {
		t.Fatal("cleaned command recreated task")
	}
}
