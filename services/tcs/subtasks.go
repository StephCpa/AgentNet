package tcs

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/agentnet/agentnet/internal/authority"
	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/jackc/pgx/v5"
)

type SubtaskAllocation struct {
	Amount      string `json:"amount"`
	Reservation string `json:"reservation_id"`
}
type SubtaskCommand struct {
	ID                string                       `json:"command_id"`
	Audience          string                       `json:"audience"`
	NotAfter          time.Time                    `json:"not_after"`
	Parent            string                       `json:"parent_task_id"`
	Grant             string                       `json:"parent_grant_id"`
	Lease             string                       `json:"lease_id"`
	Epoch             int64                        `json:"epoch"`
	ExpectedVersion   int64                        `json:"expected_task_version"`
	RecipientPolicy   string                       `json:"recipient_policy_id"`
	RecipientAgent    string                       `json:"recipient_agent_id"`
	RecipientInstance string                       `json:"recipient_instance_id"`
	Contract          json.RawMessage              `json:"contract"`
	Rights            authority.Rights             `json:"rights"`
	NotBefore         time.Time                    `json:"not_before"`
	ExpiresAt         time.Time                    `json:"expires_at"`
	Allocations       map[string]SubtaskAllocation `json:"allocations"`
}
type SubtaskSubmission struct {
	Kind    string         `json:"kind"`
	Caller  Caller         `json:"caller"`
	Command SubtaskCommand `json:"command"`
}
type delegationContract struct {
	Deadline  time.Time `json:"deadline"`
	TaskType  string    `json:"task_type"`
	InputRefs []struct {
		Version int    `json:"version"`
		Digest  string `json:"digest"`
	} `json:"input_refs"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Acceptance   json.RawMessage `json:"acceptance_policy"`
	Delegation   struct {
		Allow         bool     `json:"allow"`
		MaxDepth      int      `json:"max_depth"`
		MaxConcurrent int      `json:"max_concurrent_subtasks"`
		Scope         []string `json:"scope"`
	} `json:"delegation_policy"`
}

func delegationBody(raw []byte) (delegationContract, error) {
	var b delegationContract
	if json.Unmarshal(raw, &b) != nil || b.Deadline.IsZero() || len(b.InputRefs) == 0 || len(b.OutputSchema) == 0 || string(b.OutputSchema) == "null" || len(b.Acceptance) == 0 || string(b.Acceptance) == "null" {
		return b, Invalid
	}
	for _, ref := range b.InputRefs {
		if ref.Version < 1 || !digestRE.MatchString(ref.Digest) {
			return b, Invalid
		}
	}
	if b.Delegation.Allow && (b.Delegation.MaxDepth < 1 || b.Delegation.MaxDepth >= authority.MaxChainLength || len(b.Delegation.Scope) == 0 || b.Delegation.MaxConcurrent < 0) {
		return b, Invalid
	}
	if b.Delegation.MaxConcurrent == 0 {
		b.Delegation.MaxConcurrent = 16
	}
	return b, nil
}

// ValidateDelegableContract requires explicit contract permission when consent
// allows delegation. The root task's absolute depth ceiling never increases.
func ValidateDelegableContract(raw []byte, r authority.Rights) error {
	if r.DepthLeft == 0 {
		return nil
	}
	b, err := delegationBody(raw)
	if err != nil {
		return err
	}
	if !b.Delegation.Allow || b.Delegation.MaxDepth < r.DepthLeft {
		return Denied
	}
	for _, v := range r.DelegationScope {
		if !authority.Contains(b.Delegation.Scope, v) {
			return Denied
		}
	}
	return nil
}

func (s *Store) CreateSubtask(ctx context.Context, c Caller, envelope Envelope) (Admission, error) {
	var submission SubtaskSubmission
	var out Admission
	if err := verify(envelope, s.cfg.CommandKeys, &submission); err != nil {
		return out, err
	}
	if submission.Kind != "anet.subtask/1" || !submission.Caller.SameIdentity(c) {
		return out, Denied
	}
	cmd := submission.Command
	if cmd.ID == "" || cmd.Parent == "" || cmd.Grant == "" || cmd.Lease == "" || cmd.Audience != s.cfg.Domain || cmd.NotAfter.IsZero() || cmd.RecipientPolicy == "" || cmd.RecipientAgent == "" || cmd.RecipientInstance == "" || len(cmd.Allocations) != 4 {
		return out, Invalid
	}
	body, err := delegationBody(cmd.Contract)
	if err != nil {
		return out, err
	}
	if body.TaskType == "" {
		return out, Invalid
	}
	if err = ValidateDelegableContract(cmd.Contract, cmd.Rights); err != nil {
		return out, err
	}
	contractDigest, err := canonical.Digest(cmd.Contract)
	if err != nil {
		return out, Invalid
	}
	digest, err := canonical.Digest(envelope.Payload)
	if err != nil {
		return out, Invalid
	}
	ids := []string{}
	seen := map[string]bool{}
	for dim, a := range cmd.Allocations {
		if !dimension(dim) || !amount(a.Amount, false) || (a.Amount == "0") != (a.Reservation == "") {
			return out, Invalid
		}
		if a.Reservation != "" {
			if seen[a.Reservation] {
				return out, Invalid
			}
			seen[a.Reservation] = true
			ids = append(ids, a.Reservation)
		}
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "anet.command:"+cmd.Audience+":"+cmd.ID); err != nil {
		return out, err
	}
	parent, err := s.lockTask(ctx, tx, c, cmd.Parent)
	if err != nil {
		return out, err
	}
	now, err := currentTime(ctx, tx)
	if err != nil {
		return out, err
	}
	if !now.Before(cmd.NotAfter) {
		return out, Expired
	}
	var old string
	err = tx.QueryRow(ctx, `SELECT i.task_id,i.payload_digest,COALESCE(d.child_grant_id,''),r.receipt_id FROM command_inbox i
 JOIN receipts r ON r.task_id=i.task_id AND r.command_id=i.command_id AND r.kind='ACCEPTED'
 LEFT JOIN task_delegations d ON d.child_task_id=i.task_id WHERE i.command_id=$1 AND i.audience_domain_id=$2`, cmd.ID, cmd.Audience).Scan(&out.TaskID, &old, &out.RootGrantID, &out.ReceiptID)
	if err == nil {
		if old != digest || out.RootGrantID == "" {
			return Admission{}, Conflict
		}
		out.Existing = true
		return out, commit(ctx, tx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if parent.Version != cmd.ExpectedVersion {
		return out, VersionConflict
	}
	if parent.Epoch != cmd.Epoch {
		return out, Fenced
	}
	chain, err := authority.LockChain(ctx, tx, cmd.Grant, true)
	if errors.Is(err, authority.ErrInvalidChain) {
		return out, Denied
	}
	if err != nil {
		return out, err
	}
	g := chain.Leaf
	if g.Task != cmd.Parent || g.Subject != parent.Subject || g.Agent != c.Agent || (g.Instance != "" && g.Instance != c.Instance) || !authority.Contains(g.Rights.DelegationScope, body.TaskType) {
		return out, Denied
	}
	var raw []byte
	var root string
	var depth int
	err = tx.QueryRow(ctx, `SELECT t.root_task_id,t.delegation_depth,c.body FROM tasks t JOIN contracts c ON c.contract_id=t.contract_id AND c.version=t.contract_version WHERE t.task_id=$1`, cmd.Parent).Scan(&root, &depth, &raw)
	if err != nil {
		return out, err
	}
	pb, err := delegationBody(raw)
	if err != nil {
		return out, err
	}
	if !pb.Delegation.Allow || depth+1 > pb.Delegation.MaxDepth || !authority.Contains(pb.Delegation.Scope, body.TaskType) || body.Deadline.After(parent.Deadline) || cmd.ExpiresAt.After(body.Deadline) {
		return out, Denied
	}
	if body.Delegation.Allow {
		if body.Delegation.MaxDepth > pb.Delegation.MaxDepth || body.Delegation.MaxConcurrent > pb.Delegation.MaxConcurrent {
			return out, Denied
		}
		for _, v := range body.Delegation.Scope {
			if !authority.Contains(pb.Delegation.Scope, v) {
				return out, Denied
			}
		}
	}
	var concurrent int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE parent_task_id=$1 AND status NOT IN ('SUCCEEDED','FAILED','CANCELLED')`, cmd.Parent).Scan(&concurrent); err != nil {
		return out, err
	}
	if concurrent >= pb.Delegation.MaxConcurrent {
		return out, BudgetExhausted
	}
	var recipient Caller
	var policy string
	var before, end time.Time
	err = tx.QueryRow(ctx, `SELECT p.rights,p.not_before,p.expires_at,p.version,a.owner_principal_id,p.agent_id,p.instance_id FROM recipient_policies p JOIN agents a ON a.agent_id=p.agent_id JOIN agent_instances i ON i.instance_id=p.instance_id AND i.agent_id=a.agent_id
 WHERE p.policy_id=$1 AND p.domain_id=$2 AND p.agent_id=$3 AND p.instance_id=$4 AND p.revoked_at IS NULL FOR SHARE OF p`, cmd.RecipientPolicy, s.cfg.Domain, cmd.RecipientAgent, cmd.RecipientInstance).Scan(&raw, &before, &end, &policy, &recipient.Principal, &recipient.Agent, &recipient.Instance)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, Denied
	}
	if err != nil {
		return out, err
	}
	rights, err := authority.DecodeRights(raw)
	if err != nil {
		return out, Denied
	}
	child, ok := authority.Attenuate(authority.Scope{Rights: g.Rights, NotBefore: g.NotBefore, ExpiresAt: g.ExpiresAt}, authority.Scope{Rights: cmd.Rights, NotBefore: cmd.NotBefore, ExpiresAt: cmd.ExpiresAt}, authority.Scope{Rights: rights, NotBefore: before, ExpiresAt: end})
	if !ok {
		return out, Denied
	}
	// Existing parent accounts are locked in lexical order before reservations.
	rows, err := tx.Query(ctx, `SELECT account_id,dimension FROM budget_accounts WHERE task_id=$1 ORDER BY account_id FOR UPDATE`, cmd.Parent)
	if err != nil {
		return out, err
	}
	accounts := map[string]string{}
	for rows.Next() {
		var id, dim string
		if err = rows.Scan(&id, &dim); err != nil {
			rows.Close()
			return out, err
		}
		accounts[dim] = id
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(accounts) != 4 {
		return out, Invalid
	}
	sort.Strings(ids)
	for _, reservation := range ids {
		var h held
		err = tx.QueryRow(ctx, `SELECT r.reservation_id,r.account_id,a.dimension,r.amount::text,r.state,COALESCE(r.effect_id,''),COALESCE(r.attempt_id,''),r.purpose,r.holder_task_id FROM budget_reservations r JOIN budget_accounts a USING(account_id) WHERE r.reservation_id=$1 AND a.task_id=$2 FOR UPDATE OF r`, reservation, cmd.Parent).Scan(&h.ID, &h.Account, &h.Dimension, &h.Amount, &h.State, &h.Effect, &h.Attempt, &h.Purpose, &h.Task)
		if errors.Is(err, pgx.ErrNoRows) {
			return out, ReservationInvalid
		}
		if err != nil {
			return out, err
		}
		a := cmd.Allocations[h.Dimension]
		if h.Task != cmd.Parent || h.Account != accounts[h.Dimension] || h.ID != a.Reservation || h.Purpose != "subtask" || h.State != "HELD" || h.Effect != "" || h.Attempt != "" {
			return out, ReservationInvalid
		}
		var equal bool
		if err = tx.QueryRow(ctx, "SELECT $1::numeric=$2::numeric", a.Amount, h.Amount).Scan(&equal); err != nil {
			return out, err
		}
		if !equal {
			return out, ReservationInvalid
		}
	}
	// Check again after ordinary locks and after inserts/FK/trigger waits.
	recheck := func() error {
		now, err = currentTime(ctx, tx)
		if err != nil {
			return err
		}
		if err = active(parent, now); err != nil {
			return err
		}
		if !now.Before(cmd.NotAfter) {
			return Expired
		}
		if !now.Before(body.Deadline) || now.Before(child.NotBefore) || !now.Before(child.ExpiresAt) || !chain.ValidAt(now) {
			return Denied
		}
		if err = s.identity(ctx, tx, c); err != nil {
			return err
		}
		if err = s.identity(ctx, tx, recipient); err != nil {
			return err
		}
		live, err := chain.Live(ctx, tx)
		if err != nil {
			return err
		}
		if !live {
			return Denied
		}
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM leases WHERE task_id=$1 AND lease_id=$2 AND worker_instance_id=$3 AND epoch=$4 AND superseded_at IS NULL AND expires_at>clock_timestamp())`, cmd.Parent, cmd.Lease, c.Instance, cmd.Epoch).Scan(&live); err != nil {
			return err
		}
		if !live {
			return LeaseInvalid
		}

		return nil
	}
	if err = recheck(); err != nil {
		return out, err
	}
	out = Admission{TaskID: id("task"), ReceiptID: id("rct"), RootGrantID: id("grt")}
	contractID := id("con")
	if _, err = tx.Exec(ctx, `INSERT INTO contracts(contract_id,version,body,digest) VALUES($1,1,$2::jsonb,$3)`, contractID, string(cmd.Contract), contractDigest); err != nil {
		return out, err
	}
	limits := map[string]string{}
	for dim, a := range cmd.Allocations {
		limits[dim] = a.Amount
	}
	lb, _ := json.Marshal(limits)
	if _, err = tx.Exec(ctx, `INSERT INTO tasks(task_id,root_task_id,parent_task_id,domain_id,contract_id,contract_version,owner_principal_id,assignee_agent_id,assignee_domain_id,deadline,delegation_depth,limits) VALUES($1,$2,$3,$4,$5,1,$6,$7,$4,$8,$9,$10::jsonb)`, out.TaskID, root, cmd.Parent, s.cfg.Domain, contractID, parent.Subject, recipient.Agent, body.Deadline, depth+1, string(lb)); err != nil {
		return out, err
	}
	rb, _ := json.Marshal(child.Rights)
	if _, err = tx.Exec(ctx, `INSERT INTO grants(grant_id,root_grant_id,parent_grant_id,task_id,subject_principal_id,actor_agent_id,actor_instance_id,rights,not_before,expires_at,policy_version,consent_id,consent_version) SELECT $1,root_grant_id,grant_id,$2,subject_principal_id,$3,$4,$5::jsonb,$6,$7,$8,consent_id,consent_version FROM grants WHERE grant_id=$9`, out.RootGrantID, out.TaskID, recipient.Agent, recipient.Instance, string(rb), child.NotBefore, child.ExpiresAt, policy, cmd.Grant); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO task_delegations(child_task_id,parent_task_id,child_grant_id,recipient_policy_id,creator_principal_id,creator_agent_id,creator_instance_id,command_id,request_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, out.TaskID, cmd.Parent, out.RootGrantID, cmd.RecipientPolicy, c.Principal, c.Agent, c.Instance, cmd.ID, digest); err != nil {
		return out, err
	}
	for _, dim := range []string{"calls", "messages", "money", "tokens"} {
		a := cmd.Allocations[dim]
		account := id("acc")
		if a.Amount != "0" {
			if _, err = tx.Exec(ctx, `UPDATE budget_reservations SET state='RELEASED',released_at=clock_timestamp(),release_reason='delegated',command_id=$2 WHERE reservation_id=$1`, a.Reservation, cmd.ID); err != nil {
				return out, err
			}
			if _, err = tx.Exec(ctx, `UPDATE budget_accounts SET delegated=delegated+$2::numeric WHERE account_id=$1`, accounts[dim], a.Amount); err != nil {
				return out, err
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO budget_accounts(account_id,task_id,parent_account_id,dimension,budget_limit) VALUES($1,$2,$3,$4,$5::numeric)`, account, out.TaskID, accounts[dim], dim, a.Amount); err != nil {
			return out, err
		}
		if a.Amount != "0" {
			if _, err = tx.Exec(ctx, `INSERT INTO delegation_allocations(child_account_id,parent_account_id,reservation_id,child_task_id,amount) VALUES($1,$2,$3,$4,$5::numeric)`, account, accounts[dim], a.Reservation, out.TaskID, a.Amount); err != nil {
				return out, err
			}
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO command_inbox(command_id,audience_domain_id,task_id,profile,payload_digest,not_after,expires_at) VALUES($1,$2,$3,'agent-network/0.1',$4,$5,GREATEST(clock_timestamp()+interval '168 hours',$5))`, cmd.ID, cmd.Audience, out.TaskID, digest, cmd.NotAfter); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO receipts(receipt_id,kind,issuer_domain_id,task_id,command_id) VALUES($1,'ACCEPTED',$2,$3,$4)`, out.ReceiptID, s.cfg.Domain, out.TaskID, cmd.ID); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `UPDATE tasks SET task_version=task_version+1 WHERE task_id=$1`, cmd.Parent); err != nil {
		return out, err
	}
	payload := map[string]string{"parent_task_id": cmd.Parent, "child_task_id": out.TaskID, "command_id": cmd.ID}
	if err = event(ctx, tx, cmd.Parent, "SUBTASK_CREATED", payload); err != nil {
		return out, err
	}
	if err = event(ctx, tx, out.TaskID, "TASK_ACCEPTED", payload); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox(task_id,event_type,payload,dedup_key) VALUES($1,'SUBTASK_CREATED',jsonb_build_object('child_task_id',$2::text),'subtask:'||$2::text)`, cmd.Parent, out.TaskID); err != nil {
		return out, err
	}
	if err = audit(ctx, tx, s.cfg.Domain, out.TaskID, "", "tcs.subtask_created", payload); err != nil {
		return out, err
	}
	if err = recheck(); err != nil {
		return Admission{}, err
	}
	if err = commit(ctx, tx); err != nil {
		return Admission{}, err
	}
	return out, nil
}
