// Package ics issues root permits and bound decisions from authoritative consent.
package ics

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/agentnet/agentnet/internal/amount"
	"github.com/agentnet/agentnet/internal/authority"
	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/internal/signed"
	"github.com/agentnet/agentnet/services/tcs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Domain, KeyID          string
	Key                    ed25519.PrivateKey
	CommandKeys            map[string]ed25519.PublicKey
	Operations             map[string]tcs.Operation
	DecisionTTL, PermitTTL time.Duration
}
type Service struct {
	db  *pgxpool.Pool
	cfg Config
}

func New(db *pgxpool.Pool, c Config) (*Service, error) {
	if db == nil || c.Domain == "" || c.KeyID == "" || len(c.Key) != ed25519.PrivateKeySize || len(c.CommandKeys) == 0 || len(c.Operations) == 0 {
		return nil, tcs.Invalid
	}
	if c.DecisionTTL == 0 {
		c.DecisionTTL = 15 * time.Second
	}
	if c.PermitTTL == 0 {
		c.PermitTTL = time.Minute
	}
	if c.DecisionTTL <= 0 || c.DecisionTTL > time.Minute || c.PermitTTL <= 0 || c.PermitTTL > time.Minute {
		return nil, tcs.Invalid
	}
	c.Key = append(ed25519.PrivateKey(nil), c.Key...)
	keys := map[string]ed25519.PublicKey{}
	for id, key := range c.CommandKeys {
		if id == "" || len(key) != ed25519.PublicKeySize {
			return nil, tcs.Invalid
		}
		keys[id] = append(ed25519.PublicKey(nil), key...)
	}
	c.CommandKeys = keys
	ops := map[string]tcs.Operation{}
	for target, op := range c.Operations {
		if target == "" || op.Action == "" || op.Resource == "" || op.DataClass == "" || (op.Sensitivity != "sensitive" && op.Sensitivity != "standard") {
			return nil, tcs.Invalid
		}
		ops[target] = op
	}
	c.Operations = ops
	return &Service{db, c}, nil
}
func (s *Service) identity(ctx context.Context, tx pgx.Tx, c tcs.Caller) error {
	var live bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM principals p JOIN agents a ON a.owner_principal_id=p.principal_id JOIN agent_instances i ON i.agent_id=a.agent_id WHERE p.principal_id=$1 AND a.agent_id=$2 AND i.instance_id=$3 AND p.domain_id=$4 AND a.domain_id=$4 AND p.revoked_at IS NULL AND i.revoked_at IS NULL AND a.status='active' AND ($5='' OR i.cred_hash=$5))`, c.Principal, c.Agent, c.Instance, s.cfg.Domain, c.CredentialHash).Scan(&live)
	if err != nil {
		return err
	}
	if !live {
		return tcs.Denied
	}
	return nil
}
func timestamp(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now)
	return now.UTC(), err
}
func min(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func (s *Service) audit(ctx context.Context, tx pgx.Tx, c tcs.Caller, task, action, id string) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_log(domain_id,principal_id,agent_instance_id,task_id,action,decision,detail_digest) VALUES($1,$2,$3,NULLIF($4,''),$5,'allow',$6)`, s.cfg.Domain, c.Principal, c.Instance, task, action, id)
	return err
}
func (s *Service) IssueRoot(ctx context.Context, c tcs.Caller, command tcs.Envelope) (tcs.Envelope, error) {
	var submission tcs.Submission
	if signed.Decode(command, s.cfg.CommandKeys, &submission) != nil || submission.Kind != "anet.command/1" || !submission.Caller.SameIdentity(c) {
		return tcs.Envelope{}, tcs.Denied
	}
	cmd := submission.Command
	if cmd.Root == nil || cmd.Root.ConsentID == "" || cmd.Audience != s.cfg.Domain || len(cmd.Limits) != 4 || !authority.Valid(cmd.Root.Rights, s.cfg.Domain) {
		return tcs.Envelope{}, tcs.Denied
	}
	var contract struct {
		Deadline time.Time `json:"deadline"`
	}
	if json.Unmarshal(cmd.Contract, &contract) != nil || contract.Deadline.IsZero() {
		return tcs.Envelope{}, tcs.Invalid
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return tcs.Envelope{}, err
	}
	defer tx.Rollback(ctx)
	if err = s.identity(ctx, tx, c); err != nil {
		return tcs.Envelope{}, err
	}
	var rb, lb []byte
	var start, end time.Time
	var version int64
	var policy string
	err = tx.QueryRow(ctx, `SELECT rights,task_limits,not_before,expires_at,version,policy_version FROM root_consents WHERE consent_id=$1 AND domain_id=$2 AND subject_principal_id=$3 AND actor_agent_id=$4 AND (actor_instance_id IS NULL OR actor_instance_id=$5) AND revoked_at IS NULL FOR SHARE`, cmd.Root.ConsentID, s.cfg.Domain, c.Principal, c.Agent, c.Instance).Scan(&rb, &lb, &start, &end, &version, &policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return tcs.Envelope{}, tcs.Denied
	}
	if err != nil {
		return tcs.Envelope{}, err
	}
	now, err := timestamp(ctx, tx)
	if err != nil {
		return tcs.Envelope{}, err
	}
	if now.Before(start) || !now.Before(end) || !now.Before(cmd.NotAfter) || !now.Before(contract.Deadline) {
		return tcs.Envelope{}, tcs.Expired
	}
	if err = s.identity(ctx, tx, c); err != nil {
		return tcs.Envelope{}, err
	}
	var ceiling authority.Rights
	var limits map[string]string
	if json.Unmarshal(rb, &ceiling) != nil || json.Unmarshal(lb, &limits) != nil || !authority.Subset(cmd.Root.Rights, ceiling, s.cfg.Domain) || len(limits) != 4 {
		return tcs.Envelope{}, tcs.Denied
	}
	for _, dim := range []string{"money", "tokens", "calls", "messages"} {
		if !amount.Valid(cmd.Limits[dim]) || !amount.Valid(limits[dim]) {
			return tcs.Envelope{}, tcs.Invalid
		}
		var within bool
		err = tx.QueryRow(ctx, `SELECT $1::numeric <= $2::numeric`, cmd.Limits[dim], limits[dim]).Scan(&within)
		if err != nil {
			return tcs.Envelope{}, err
		}
		if !within {
			return tcs.Envelope{}, tcs.BudgetExhausted
		}
	}
	digest, err := canonical.Digest(command.Payload)
	if err != nil {
		return tcs.Envelope{}, err
	}
	var id string
	if err = tx.QueryRow(ctx, `SELECT 'grt_'||gen_random_uuid()::text`).Scan(&id); err != nil {
		return tcs.Envelope{}, err
	}
	permit := tcs.RootPermit{Kind: "anet.root-permit/1", GrantID: id, CommandDigest: digest, ConsentID: cmd.Root.ConsentID, ConsentVersion: version, Caller: c, Audience: s.cfg.Domain, IssuedAt: now, ExpiresAt: min(now.Add(s.cfg.PermitTTL), min(end, cmd.NotAfter)), GrantNotBefore: now, GrantExpiresAt: min(end, contract.Deadline).UTC(), PolicyVersion: policy, Rights: cmd.Root.Rights}
	out, err := signed.Encode(s.cfg.KeyID, s.cfg.Key, permit)
	if err != nil {
		return out, err
	}
	if err = s.audit(ctx, tx, c, "", "authz.root_permit", digest); err != nil {
		return tcs.Envelope{}, err
	}
	return out, tx.Commit(ctx)
}

type Request struct{ GrantID, TaskID, Target, ParamsDigest, AuthorityDigest, Audience, Sensitivity, DataClass string }

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (s *Service) Decide(ctx context.Context, c tcs.Caller, r Request) (tcs.Envelope, error) {
	op, ok := s.cfg.Operations[r.Target]
	if !ok || r.Audience != s.cfg.Domain || r.Sensitivity != op.Sensitivity || r.DataClass != op.DataClass || !digestRE.MatchString(r.ParamsDigest) {
		return tcs.Envelope{}, tcs.Denied
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return tcs.Envelope{}, err
	}
	defer tx.Rollback(ctx)
	var task string
	err = tx.QueryRow(ctx, `SELECT task_id FROM tasks WHERE task_id=$1 AND domain_id=$2 AND owner_principal_id=$3 AND cancel_version IS NULL AND status NOT IN ('SUCCEEDED','FAILED','CANCELLED') AND NOT budget_frozen AND deadline>=clock_timestamp() FOR SHARE`, r.TaskID, s.cfg.Domain, c.Principal).Scan(&task)
	if errors.Is(err, pgx.ErrNoRows) {
		return tcs.Envelope{}, tcs.Denied
	}
	if err != nil {
		return tcs.Envelope{}, err
	}
	if err = s.identity(ctx, tx, c); err != nil {
		return tcs.Envelope{}, err
	}
	var grant string
	err = tx.QueryRow(ctx, `SELECT grant_id FROM grants WHERE grant_id=$1 AND task_id=$2 AND subject_principal_id=$3 AND actor_agent_id=$4 AND actor_instance_id=$5 AND parent_grant_id IS NULL AND root_grant_id=grant_id AND status='active' AND revoked_at IS NULL AND consent_id IS NOT NULL FOR SHARE`, r.GrantID, r.TaskID, c.Principal, c.Agent, c.Instance).Scan(&grant)
	if errors.Is(err, pgx.ErrNoRows) {
		return tcs.Envelope{}, tcs.Denied
	}
	if err != nil {
		return tcs.Envelope{}, err
	}
	g, err := authority.LoadGrant(ctx, tx, grant)
	if err != nil {
		return tcs.Envelope{}, err
	}
	var consentEnd time.Time
	err = tx.QueryRow(ctx, `SELECT expires_at FROM root_consents WHERE consent_id=$1 AND version=$2 AND revoked_at IS NULL AND not_before<=clock_timestamp() AND expires_at>=clock_timestamp() FOR SHARE`, g.ConsentID, g.ConsentVersion).Scan(&consentEnd)
	if errors.Is(err, pgx.ErrNoRows) {
		return tcs.Envelope{}, tcs.Denied
	}
	if err != nil {
		return tcs.Envelope{}, err
	}
	now, err := timestamp(ctx, tx)
	if err != nil {
		return tcs.Envelope{}, err
	}
	if now.Before(g.NotBefore) || !now.Before(g.ExpiresAt) || !authority.Contains(g.Rights.Actions, op.Action) || !authority.Contains(g.Rights.Resources, op.Resource) || !authority.Contains(g.Rights.DataClasses, op.DataClass) || !authority.Contains(g.Rights.Spatial.Domains, s.cfg.Domain) {
		return tcs.Envelope{}, tcs.Denied
	}
	if err = s.identity(ctx, tx, c); err != nil {
		return tcs.Envelope{}, err
	}
	digest, err := g.Digest()
	if err != nil {
		return tcs.Envelope{}, err
	}
	if r.AuthorityDigest != "" && r.AuthorityDigest != digest {
		return tcs.Envelope{}, tcs.DecisionMismatch
	}
	var id string
	if err = tx.QueryRow(ctx, `SELECT 'dec_'||gen_random_uuid()::text`).Scan(&id); err != nil {
		return tcs.Envelope{}, err
	}
	decision := tcs.Decision{Kind: "anet.authorization/1", ID: id, Allow: true, At: now, ExpiresAt: min(now.Add(s.cfg.DecisionTTL), min(g.ExpiresAt, consentEnd)), Subject: c.Principal, Agent: c.Agent, Instance: c.Instance, Grant: g.ID, Task: task, Target: r.Target, Action: op.Action, Resource: op.Resource, ParamsDigest: r.ParamsDigest, AuthorityDigest: digest, Audience: s.cfg.Domain, Sensitivity: op.Sensitivity, DataClass: op.DataClass, PolicyVersion: g.Policy}
	out, err := signed.Encode(s.cfg.KeyID, s.cfg.Key, decision)
	if err != nil {
		return out, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO authorization_decisions(decision_id,domain_id,task_id,grant_id,signed_payload,signature,key_id) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7)`, id, s.cfg.Domain, task, g.ID, string(out.Payload), out.Signature, out.KeyID)
	if err != nil {
		return tcs.Envelope{}, err
	}
	if err = s.audit(ctx, tx, c, task, "authz.decide", digest); err != nil {
		return tcs.Envelope{}, err
	}
	return out, tx.Commit(ctx)
}
func (s *Service) RevokeGrant(ctx context.Context, c tcs.Caller, id, reason string) (time.Time, error) {
	if id == "" || reason == "" || len(reason) > 256 {
		return time.Time{}, tcs.Invalid
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback(ctx)
	if err = s.identity(ctx, tx, c); err != nil {
		return time.Time{}, err
	}
	var task string
	err = tx.QueryRow(ctx, `SELECT t.task_id FROM tasks t JOIN grants g ON g.task_id=t.task_id WHERE g.grant_id=$1 AND t.domain_id=$2 AND t.owner_principal_id=$3 AND g.actor_agent_id=$4 FOR UPDATE OF t`, id, s.cfg.Domain, c.Principal, c.Agent).Scan(&task)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, tcs.Denied
	}
	if err != nil {
		return time.Time{}, err
	}
	if err = s.identity(ctx, tx, c); err != nil {
		return time.Time{}, err
	}
	var at time.Time
	err = tx.QueryRow(ctx, `UPDATE grants SET status='revoked',revoked_at=COALESCE(revoked_at,clock_timestamp()),revoke_reason=COALESCE(revoke_reason,$2) WHERE grant_id=$1 RETURNING revoked_at`, id, reason).Scan(&at)
	if err != nil {
		return time.Time{}, err
	}
	if err = s.audit(ctx, tx, c, task, "authz.revoke", id); err != nil {
		return time.Time{}, err
	}
	return at, tx.Commit(ctx)
}
