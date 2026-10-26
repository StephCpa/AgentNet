package tcs

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/agentnet/agentnet/internal/canonical"
)

func TestKernelContractValidation(t *testing.T) {
	f := setup(t)
	for _, schema := range []string{`null`, `{"type":"not-a-type"}`, `{"$ref":"https://invalid.test/schema"}`} {
		cmd := f.command()
		var body map[string]json.RawMessage
		json.Unmarshal(cmd.Contract, &body)
		body["output_schema"] = json.RawMessage(schema)
		cmd.Contract, _ = json.Marshal(body)
		_, err := f.submit(cmd)
		wantCode(t, err, ContractInvalid)
		if f.integer("SELECT count(*) FROM contracts WHERE contract_id=$1", cmd.ContractID) != 0 || f.integer("SELECT count(*) FROM tasks WHERE domain_id=$1", f.cfg.Domain) != 0 || f.integer("SELECT count(*) FROM command_inbox WHERE command_id=$1", cmd.ID) != 0 {
			t.Fatal("invalid contract left admission facts")
		}
	}
}

func TestKernelLegacyContractReplay(t *testing.T) {
	f := setup(t)
	cmd := f.command()
	var body map[string]any
	json.Unmarshal(cmd.Contract, &body)
	body["acceptance_policy"] = map[string]string{"verifier": "test"}
	cmd.Contract, _ = json.Marshal(body)
	// Seed an authentic pre-015 admission shape without mutating a contract or
	// disabling triggers. A stricter parser must not erase an accepted fact.
	raw, _ := json.Marshal(struct {
		Command Command `json:"command"`
		Caller  Caller  `json:"caller"`
	}{cmd, f.caller})
	digest, _ := canonical.Digest(raw)
	contractDigest, _ := canonical.Digest(cmd.Contract)
	task, receipt := id("task"), id("rct")
	f.exec("INSERT INTO contracts(contract_id,version,body,digest) VALUES($1,1,$2::jsonb,$3)", cmd.ContractID, string(cmd.Contract), contractDigest)
	f.exec("INSERT INTO tasks(task_id,root_task_id,domain_id,contract_id,contract_version,owner_principal_id,assignee_agent_id) VALUES($1,$1,$2,$3,1,$4,$5)", task, f.cfg.Domain, cmd.ContractID, f.caller.Principal, f.caller.Agent)
	f.exec("INSERT INTO command_inbox(command_id,audience_domain_id,task_id,profile,payload_digest,not_after,expires_at) VALUES($1,$2,$3,'agent-network/0.1',$4,$5,$5)", cmd.ID, f.cfg.Domain, task, digest, cmd.NotAfter)
	f.exec("INSERT INTO receipts(receipt_id,kind,issuer_domain_id,task_id,command_id) VALUES($1,'ACCEPTED',$2,$3,$4)", receipt, f.cfg.Domain, task, cmd.ID)
	a, err := f.submit(cmd)
	if err != nil || !a.Existing || a.TaskID != task || a.ReceiptID != receipt {
		t.Fatal(a, err)
	}
	cmd.ID = id("cmd")
	_, err = f.submit(cmd)
	wantCode(t, err, ContractInvalid)
}

func TestKernelContractVersionsImmutable(t *testing.T) {
	f := setup(t)
	cmd := f.command()
	a, err := f.submit(cmd)
	if err != nil {
		t.Fatal(err)
	}
	for _, update := range []string{
		"UPDATE contracts SET body='{}' WHERE contract_id=$1",
		"UPDATE contracts SET digest='sha256:rewrite' WHERE contract_id=$1",
		"UPDATE contracts SET contract_id=contract_id||'-rewrite' WHERE contract_id=$1",
		"UPDATE contracts SET version=version+1 WHERE contract_id=$1",
		"UPDATE contracts SET created_at=created_at+interval '1 second' WHERE contract_id=$1",
		"DELETE FROM contracts WHERE contract_id=$1",
	} {
		_, err = f.db.Exec(f.ctx, update, cmd.ContractID)
		if err == nil || !strings.Contains(err.Error(), "ANET-CMD-CONTRACT-IMMUTABLE") {
			t.Fatal(update, err)
		}
	}
	f.exec("UPDATE contracts SET body=body WHERE contract_id=$1", cmd.ContractID)
	second := cmd
	second.ID = id("cmd")
	second.ContractVersion++
	var body map[string]any
	json.Unmarshal(second.Contract, &body)
	body["goal"] = "new version"
	second.Contract, _ = json.Marshal(body)
	b, err := f.submit(second)
	if err != nil || b.TaskID == a.TaskID {
		t.Fatal(b, err)
	}
	_, err = f.db.Exec(f.ctx, "UPDATE tasks SET contract_version=2,task_version=task_version+1 WHERE task_id=$1", a.TaskID)
	if err == nil || !strings.Contains(err.Error(), "ANET-TSK-CONTRACT-IMMUTABLE") {
		t.Fatal("rebound old task", err)
	}
	third := f.command()
	c, err := f.submit(third)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.db.Exec(f.ctx, "UPDATE tasks SET contract_id=$2,task_version=task_version+1 WHERE task_id=$1", c.TaskID, cmd.ContractID)
	if err == nil || !strings.Contains(err.Error(), "ANET-TSK-CONTRACT-IMMUTABLE") {
		t.Fatal("rebound contract ID", err)
	}
	again, err := f.submit(cmd)
	if err != nil || again.TaskID != a.TaskID || again.ReceiptID != a.ReceiptID || !again.Existing {
		t.Fatal(again, err)
	}
	if f.integer("SELECT contract_version FROM tasks WHERE task_id=$1", a.TaskID) != 1 {
		t.Fatal("old task changed")
	}
}

func TestKernelConcurrentContractConflict(t *testing.T) {
	f := setup(t)
	a := f.command()
	b := a
	b.ID = id("cmd")
	var body map[string]any
	json.Unmarshal(b.Contract, &body)
	body["goal"] = "conflicting same version"
	b.Contract, _ = json.Marshal(body)
	// Sign before goroutines: fixture helpers may call testing.Fatal.
	signed := []Envelope{f.sign(Submission{Kind: "anet.command/1", Caller: f.caller, Command: a}, "gateway"), f.sign(Submission{Kind: "anet.command/1", Caller: f.caller, Command: b}, "gateway")}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range signed {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, errs[i] = f.s.Submit(f.ctx, f.caller, signed[i]) }(i)
	}
	wg.Wait()
	if errs[0] == nil {
		wantCode(t, errs[1], Conflict)
	} else {
		wantCode(t, errs[0], Conflict)
		if errs[1] != nil {
			t.Fatal(errs)
		}
	}
	if f.integer("SELECT count(*) FROM tasks WHERE contract_id=$1", a.ContractID) != 1 || f.integer("SELECT count(*) FROM budget_accounts a JOIN tasks t USING(task_id) WHERE t.contract_id=$1", a.ContractID) != 4 || f.integer("SELECT count(*) FROM receipts r JOIN tasks t USING(task_id) WHERE t.contract_id=$1", a.ContractID) != 1 {
		t.Fatal("contract conflict leaked admission state")
	}
}
