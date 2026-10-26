package tcs

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/jackc/pgx/v5"
)

func (s *Store) Submit(ctx context.Context, c Caller, signed Envelope) (Admission, error) {
	return s.submit(ctx, c, signed, nil)
}
func (s *Store) submit(ctx context.Context, c Caller, signed Envelope, root *RootPermit) (Admission, error) {
	var submission Submission
	if err := verify(signed, s.cfg.CommandKeys, &submission); err != nil {
		return Admission{}, err
	}
	if submission.Kind != "anet.command/1" || !submission.Caller.SameIdentity(c) {
		return Admission{}, Denied
	}
	cmd := submission.Command
	if (cmd.Root != nil) != (root != nil) {
		return Admission{}, Denied
	}
	var out Admission
	if cmd.ID == "" || cmd.Audience != s.cfg.Domain || cmd.NotAfter.IsZero() || cmd.ContractID == "" || cmd.ContractVersion < 1 || len(cmd.Limits) != 4 {
		return out, Invalid
	}
	for d, a := range cmd.Limits {
		if !dimension(d) || !amount(a, false) {
			return out, Invalid
		}
	}
	contractDigest, err := canonical.Digest(cmd.Contract)
	if err != nil {
		return out, Invalid
	}
	var body struct {
		Deadline  time.Time `json:"deadline"`
		InputRefs []struct {
			Version int    `json:"version"`
			Digest  string `json:"digest"`
		} `json:"input_refs"`
		OutputSchema json.RawMessage `json:"output_schema"`
		Acceptance   json.RawMessage `json:"acceptance_policy"`
	}
	if json.Unmarshal(cmd.Contract, &body) != nil || body.Deadline.IsZero() || len(body.InputRefs) == 0 || len(body.OutputSchema) == 0 || len(body.Acceptance) == 0 {
		return out, Invalid
	}
	for _, r := range body.InputRefs {
		if r.Version < 1 || !digestRE.MatchString(r.Digest) {
			return out, Invalid
		}
	}
	raw, err := json.Marshal(struct {
		Command Command `json:"command"`
		Caller  Caller  `json:"caller"`
	}{cmd, c})
	if err != nil {
		return out, Invalid
	}
	digest, err := canonical.Digest(raw)
	if err != nil {
		return out, Invalid
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	// Lock the logical command before it exists. A hash collision only serializes unrelated commands.
	_, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "anet.command:"+cmd.Audience+":"+cmd.ID)
	if err != nil {
		return out, err
	}
	if err = s.identity(ctx, tx, c); err != nil {
		return out, err
	}
	now, err := currentTime(ctx, tx)
	if err != nil {
		return out, err
	}
	if now.After(cmd.NotAfter) {
		return out, Expired
	}
	var stored string
	err = tx.QueryRow(ctx, `SELECT i.task_id,i.payload_digest,r.receipt_id,COALESCE((SELECT grant_id FROM grants WHERE task_id=i.task_id AND consent_id IS NOT NULL LIMIT 1),'') FROM command_inbox i JOIN receipts r ON r.task_id=i.task_id AND r.command_id=i.command_id AND r.kind='ACCEPTED' WHERE i.command_id=$1 AND i.audience_domain_id=$2`, cmd.ID, cmd.Audience).Scan(&out.TaskID, &stored, &out.ReceiptID, &out.RootGrantID)
	if err == nil {
		if stored != digest {
			return Admission{}, Conflict
		}
		out.Existing = true
		return out, commit(ctx, tx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if now.After(body.Deadline) {
		return out, Deadline
	}
	_, err = tx.Exec(ctx, `INSERT INTO contracts(contract_id,version,body,digest) VALUES($1,$2,$3::jsonb,$4) ON CONFLICT DO NOTHING`, cmd.ContractID, cmd.ContractVersion, string(cmd.Contract), contractDigest)
	if err != nil {
		return out, err
	}
	if err = tx.QueryRow(ctx, "SELECT digest FROM contracts WHERE contract_id=$1 AND version=$2", cmd.ContractID, cmd.ContractVersion).Scan(&stored); err != nil {
		return out, err
	}
	if stored != contractDigest {
		return out, Conflict
	}
	now, err = currentTime(ctx, tx)
	if err != nil {
		return out, err
	}
	if err = s.identity(ctx, tx, c); err != nil {
		return out, err
	}
	if now.After(cmd.NotAfter) {
		return out, Expired
	}
	if now.After(body.Deadline) {
		return out, Deadline
	}
	out.TaskID = id("task")
	out.ReceiptID = id("rct")
	limits, _ := json.Marshal(cmd.Limits)
	_, err = tx.Exec(ctx, `INSERT INTO tasks(task_id,root_task_id,domain_id,contract_id,contract_version,owner_principal_id,assignee_agent_id,deadline,limits)
 VALUES($1,$1,$2,$3,$4,$5,$6,$7,$8::jsonb)`, out.TaskID, s.cfg.Domain, cmd.ContractID, cmd.ContractVersion, c.Principal, c.Agent, body.Deadline, string(limits))
	if err != nil {
		return out, err
	}
	if root != nil {
		if err = s.insertRoot(ctx, tx, c, cmd, root, out.TaskID, body.Deadline); err != nil {
			return Admission{}, err
		}
		out.RootGrantID = root.GrantID
	}
	_, err = tx.Exec(ctx, `INSERT INTO command_inbox(command_id,audience_domain_id,task_id,profile,payload_digest,not_after,expires_at)
 VALUES($1,$2,$3,'agent-network/0.1',$4,$5,GREATEST(clock_timestamp()+interval '168 hours',$5))`, cmd.ID, s.cfg.Domain, out.TaskID, digest, cmd.NotAfter)
	if err != nil {
		return out, err
	}
	dims := []string{"calls", "messages", "money", "tokens"}
	for _, d := range dims {
		_, err = tx.Exec(ctx, `INSERT INTO budget_accounts(account_id,task_id,dimension,budget_limit) VALUES($1,$2,$3,$4::numeric)`, id("acc"), out.TaskID, d, cmd.Limits[d])
		if err != nil {
			return out, err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO receipts(receipt_id,kind,issuer_domain_id,task_id,command_id) VALUES($1,'ACCEPTED',$2,$3,$4)`, out.ReceiptID, s.cfg.Domain, out.TaskID, cmd.ID)
	if err != nil {
		return out, err
	}
	if err = event(ctx, tx, out.TaskID, "TASK_ACCEPTED", map[string]string{"command_id": cmd.ID}); err != nil {
		return out, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox(task_id,event_type,payload,dedup_key) VALUES($1,'TASK_ACCEPTED',jsonb_build_object('receipt_id',$2::text),$3)`, out.TaskID, out.ReceiptID, "accepted:"+s.cfg.Domain+":"+cmd.ID)
	if err != nil {
		return out, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log(domain_id,principal_id,agent_instance_id,task_id,command_id,action,decision,detail_digest) VALUES($1,$2,$3,$4,$5,'tcs.command_accepted','allow',$6)`, s.cfg.Domain, c.Principal, c.Instance, out.TaskID, cmd.ID, digest)
	if err != nil {
		return Admission{}, err
	}
	if err = commit(ctx, tx); err != nil {
		return Admission{}, err
	}
	return out, nil
}

func (s *Store) Reserve(ctx context.Context, c Caller, taskID, d, purpose, a string) (Reservation, error) {
	out := Reservation{Dimension: d, Amount: a, State: "HELD"}
	if !dimension(d) || !amount(a, true) || purpose != "tool_call" {
		return out, Invalid
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	t, err := s.lockTask(ctx, tx, c, taskID)
	if err != nil {
		return out, err
	}
	var account string
	err = tx.QueryRow(ctx, `SELECT account_id FROM budget_accounts WHERE task_id=$1 AND dimension=$2 AND parent_account_id IS NULL FOR UPDATE`, taskID, d).Scan(&account)
	if err != nil {
		return out, err
	}
	now, err := currentTime(ctx, tx)
	if err != nil {
		return out, err
	}
	if err = active(t, now); err != nil {
		return out, err
	}
	var enough bool
	if err = s.identity(ctx, tx, c); err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT settled+delegated+$2::numeric+COALESCE((SELECT sum(amount) FROM budget_reservations WHERE account_id=$1 AND state='HELD'),0)<=budget_limit FROM budget_accounts WHERE account_id=$1`, account, a).Scan(&enough)
	if err != nil {
		return out, err
	}
	if !enough {
		return out, BudgetExhausted
	}
	out.ID = id("res")
	_, err = tx.Exec(ctx, `INSERT INTO budget_reservations(reservation_id,account_id,holder_task_id,purpose,amount) VALUES($1,$2,$3,$4,$5::numeric)`, out.ID, account, taskID, purpose, a)
	if err != nil {
		return out, err
	}
	if err = commit(ctx, tx); err != nil {
		return Reservation{}, err
	}
	return out, nil
}

func (s *Store) AcquireLease(ctx context.Context, c Caller, taskID string) (Lease, error) {
	var out Lease
	tx, err := s.begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	t, err := s.lockTask(ctx, tx, c, taskID)
	if err != nil {
		return out, err
	}
	now, err := currentTime(ctx, tx)
	if err != nil {
		return out, err
	}
	if err = active(t, now); err != nil {
		return out, err
	}
	var holder string
	err = tx.QueryRow(ctx, `SELECT lease_id,worker_instance_id,epoch,expires_at FROM leases WHERE task_id=$1 AND superseded_at IS NULL ORDER BY created_at DESC LIMIT 1`, taskID).Scan(&out.ID, &holder, &out.Epoch, &out.ExpiresAt)
	if err == nil && !now.After(out.ExpiresAt) {
		if holder != c.Instance {
			return Lease{}, LeaseInvalid
		}
		out.Version = t.Version
		return out, commit(ctx, tx)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if err == nil && now.Before(out.ExpiresAt.Add(s.cfg.LeaseGrace)) {
		return Lease{}, LeaseInvalid
	}
	epoch := t.Epoch
	if err == nil {
		epoch++
	}
	_, err = tx.Exec(ctx, "UPDATE leases SET superseded_at=clock_timestamp() WHERE task_id=$1 AND superseded_at IS NULL", taskID)
	if err != nil {
		return out, err
	}
	out = Lease{ID: id("lease"), Epoch: epoch, Version: t.Version + 1, ExpiresAt: now.Add(s.cfg.LeaseTTL)}
	_, err = tx.Exec(ctx, "INSERT INTO leases(lease_id,task_id,worker_instance_id,epoch,expires_at) VALUES($1,$2,$3,$4,$5)", out.ID, taskID, c.Instance, out.Epoch, out.ExpiresAt)
	if err != nil {
		return out, err
	}
	_, err = tx.Exec(ctx, "UPDATE tasks SET status='RUNNING',current_epoch=$2,task_version=task_version+1 WHERE task_id=$1", taskID, epoch)
	if err != nil {
		return out, err
	}
	if err = event(ctx, tx, taskID, "LEASE_ACQUIRED", out); err != nil {
		return out, err
	}
	if err = commit(ctx, tx); err != nil {
		return Lease{}, err
	}
	return out, nil
}

func (s *Store) Cancel(ctx context.Context, c Caller, taskID string) (int64, error) {
	version, _, err := s.CancelDetailed(ctx, c, taskID)
	return version, err
}

func (s *Store) CancelDetailed(ctx context.Context, c Caller, taskID string) (int64, bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
	t, err := s.lockTask(ctx, tx, c, taskID)
	if err != nil {
		return 0, false, err
	}
	var version int64
	if t.Status == "CANCELLED" {
		err = tx.QueryRow(ctx, "SELECT cancel_version FROM tasks WHERE task_id=$1", taskID).Scan(&version)
		if err != nil {
			return 0, false, err
		}
		return version, true, commit(ctx, tx)
	}
	if t.Status == "SUCCEEDED" || t.Status == "FAILED" {
		return 0, false, Terminal
	}
	err = tx.QueryRow(ctx, `UPDATE tasks SET status='CANCELLED',cancelled_at=clock_timestamp(),cancel_version=COALESCE(cancel_version,0)+1,task_version=task_version+1 WHERE task_id=$1 RETURNING cancel_version`, taskID).Scan(&version)
	if err != nil {
		return 0, false, err
	}
	if err = event(ctx, tx, taskID, "CANCELLED", map[string]int64{"cancel_version": version}); err != nil {
		return 0, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox(task_id,event_type,payload,dedup_key) VALUES($1,'CANCELLED',jsonb_build_object('cancel_version',$2::bigint),$3)`, taskID, version, "cancel:"+taskID)
	if err != nil {
		return 0, false, err
	}
	return version, false, commit(ctx, tx)
}

// lockReservations implements tasks -> accounts (sorted IDs) -> reservations
// (sorted IDs). Account ownership and reservation bindings are rechecked after locking.
type held struct{ ID, Account, Dimension, Amount, State, Effect, Attempt, Purpose, Task string }

func lockReservations(ctx context.Context, tx pgx.Tx, ids []string) ([]held, error) {
	if len(ids) == 0 {
		return nil, ReservationInvalid
	}
	sort.Strings(ids)
	accountSet := map[string]bool{}
	for _, r := range ids {
		var a string
		if err := tx.QueryRow(ctx, "SELECT account_id FROM budget_reservations WHERE reservation_id=$1", r).Scan(&a); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ReservationInvalid
			}
			return nil, err
		}
		accountSet[a] = true
	}
	accounts := []string{}
	for a := range accountSet {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	for _, a := range accounts {
		var got string
		if err := tx.QueryRow(ctx, "SELECT account_id FROM budget_accounts WHERE account_id=$1 FOR UPDATE", a).Scan(&got); err != nil {
			return nil, err
		}
	}
	result := []held{}
	for _, r := range ids {
		var h held
		var accountTask string
		err := tx.QueryRow(ctx, `SELECT r.reservation_id,r.account_id,a.dimension,r.amount::text,r.state,COALESCE(r.effect_id,''),COALESCE(r.attempt_id,''),r.purpose,r.holder_task_id,a.task_id
   FROM budget_reservations r JOIN budget_accounts a USING(account_id) WHERE r.reservation_id=$1 FOR UPDATE OF r`, r).Scan(&h.ID, &h.Account, &h.Dimension, &h.Amount, &h.State, &h.Effect, &h.Attempt, &h.Purpose, &h.Task, &accountTask)
		if err != nil {
			return nil, err
		}
		if !accountSet[h.Account] || h.Task != accountTask {
			return nil, ReservationInvalid
		}
		result = append(result, h)
	}
	return result, nil
}
