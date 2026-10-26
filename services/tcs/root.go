package tcs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/agentnet/agentnet/internal/authority"
	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/jackc/pgx/v5"
)

func (s *Store) SubmitAuthorized(ctx context.Context, c Caller, command, root Envelope) (Admission, error) {
	var permit RootPermit
	if err := verify(root, s.cfg.RootKeys, &permit); err != nil {
		return Admission{}, err
	}
	digest, err := canonical.Digest(command.Payload)
	if err != nil || permit.Kind != "anet.root-permit/1" || permit.CommandDigest != digest || permit.Caller != c || permit.Audience != s.cfg.Domain || permit.GrantID == "" || permit.ConsentID == "" || !authority.Valid(permit.Rights, s.cfg.Domain) {
		return Admission{}, Denied
	}
	return s.submit(ctx, c, command, &permit)
}

func (s *Store) insertRoot(ctx context.Context, tx pgx.Tx, c Caller, cmd Command, p *RootPermit, taskID string, deadline time.Time) error {
	if cmd.Root == nil || cmd.Root.ConsentID != p.ConsentID {
		return Denied
	}
	a, _ := json.Marshal(cmd.Root.Rights)
	b, _ := json.Marshal(p.Rights)
	ad, _ := canonical.Digest(a)
	bd, _ := canonical.Digest(b)
	if ad != bd {
		return Denied
	}
	var rightsJSON, limitsJSON []byte
	var before, expires time.Time
	var version int64
	var policy string
	err := tx.QueryRow(ctx, `SELECT rights,task_limits,not_before,expires_at,version,policy_version FROM root_consents
 WHERE consent_id=$1 AND domain_id=$2 AND subject_principal_id=$3 AND actor_agent_id=$4
 AND (actor_instance_id IS NULL OR actor_instance_id=$5) AND revoked_at IS NULL FOR SHARE`, p.ConsentID, s.cfg.Domain, c.Principal, c.Agent, c.Instance).Scan(&rightsJSON, &limitsJSON, &before, &expires, &version, &policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Denied
	}
	if err != nil {
		return err
	}
	now, err := currentTime(ctx, tx)
	if err != nil {
		return err
	}
	if now.After(cmd.NotAfter) {
		return Expired
	}
	if now.After(deadline) {
		return Deadline
	}
	if version != p.ConsentVersion || policy != p.PolicyVersion || now.Before(before) || !now.Before(expires) || p.IssuedAt.After(now) || !now.Before(p.ExpiresAt) || p.GrantNotBefore.Before(before) || p.GrantNotBefore.After(now) || p.GrantExpiresAt.After(expires) || p.GrantExpiresAt.After(deadline) || !p.GrantExpiresAt.After(now) {
		return Denied
	}
	if err = s.identity(ctx, tx, c); err != nil {
		return err
	}
	var ceiling authority.Rights
	var limits map[string]string
	if json.Unmarshal(rightsJSON, &ceiling) != nil || json.Unmarshal(limitsJSON, &limits) != nil || !authority.Subset(p.Rights, ceiling, s.cfg.Domain) || len(limits) != 4 {
		return Denied
	}
	for dim, value := range cmd.Limits {
		limit, ok := limits[dim]
		if !ok || !amount(limit, false) {
			return Denied
		}
		var within bool
		if err = tx.QueryRow(ctx, `SELECT $1::numeric <= $2::numeric`, value, limit).Scan(&within); err != nil {
			return err
		}
		if !within {
			return BudgetExhausted
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO grants(grant_id,root_grant_id,task_id,subject_principal_id,actor_agent_id,actor_instance_id,rights,not_before,expires_at,policy_version,consent_id,consent_version)
 VALUES($1,$1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10,$11)`, p.GrantID, taskID, c.Principal, c.Agent, c.Instance, string(b), p.GrantNotBefore, p.GrantExpiresAt, p.PolicyVersion, p.ConsentID, p.ConsentVersion)
	return err
}
