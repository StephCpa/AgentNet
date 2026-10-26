package tcs

import (
	"context"
	"errors"

	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/jackc/pgx/v5"
)

func (s *Store) Admit(ctx context.Context, c Caller, p Proposal, signed Envelope) (Effect, error) {
	out := Effect{ID: p.EffectID}
	if p.EffectID == "" || p.TaskID == "" || p.CommandID == "" || !digestRE.MatchString(p.ParamsDigest) || !digestRE.MatchString(p.AuthorityDigest) {
		return out, Invalid
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	t, err := s.lockTask(ctx, tx, c, p.TaskID)
	if err != nil {
		return out, err
	}
	var taskID, commandID, target, pd, ad string
	err = tx.QueryRow(ctx, "SELECT task_id,command_id,target_ref,params_digest,authority_digest,state FROM effects WHERE effect_id=$1", p.EffectID).Scan(&taskID, &commandID, &target, &pd, &ad, &out.State)
	if err == nil {
		if taskID != p.TaskID {
			return Effect{}, EffectConflict
		}
		if commandID == p.CommandID && target == p.Target && pd == p.ParamsDigest && ad == p.AuthorityDigest {
			out.Existing = true
			return out, commit(ctx, tx)
		}
		_, err = tx.Exec(ctx, `INSERT INTO effect_conflict_attempts(effect_id,command_id,presented_params_digest,bound_params_digest,requester_instance_id) VALUES($1,$2,$3,$4,$5)`, p.EffectID, p.CommandID, p.ParamsDigest, pd, c.Instance)
		if err != nil {
			return out, err
		}
		if err = audit(ctx, tx, s.cfg.Domain, p.TaskID, p.EffectID, "effect.conflict_rejected", map[string]string{"presented_params_digest": p.ParamsDigest}); err != nil {
			return out, err
		}
		if err = commit(ctx, tx); err != nil {
			return out, err
		}
		return out, EffectConflict
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	now, err := currentTime(ctx, tx)
	if err != nil {
		return out, err
	}
	if err = active(t, now); err != nil {
		return out, err
	}
	if p.Epoch != t.Epoch {
		return out, Fenced
	}
	if p.ExpectedVersion != t.Version {
		return out, VersionConflict
	}
	op, ok := s.cfg.Operations[p.Target]
	if !ok {
		return out, Denied
	}
	if p.Sensitivity != "standard" && p.Sensitivity != "sensitive" {
		return out, Invalid
	}
	if len(p.Reservations) != len(op.Dimensions) || len(p.UpperBounds) != len(op.Dimensions) {
		return out, ReservationInvalid
	}
	var d Decision
	if err = verify(signed, s.cfg.DecisionKeys, &d); err != nil {
		return out, err
	}
	if d.Kind != "anet.authorization/1" || !d.Allow || d.ID == "" || d.Subject != c.Principal || d.Agent != c.Agent || d.Instance != c.Instance || d.Task != p.TaskID || d.Target != p.Target || d.Action != op.Action || d.Resource != op.Resource || d.ParamsDigest != p.ParamsDigest || d.AuthorityDigest != p.AuthorityDigest || d.Audience != s.cfg.Domain || d.Sensitivity != p.Sensitivity {
		return out, DecisionMismatch
	}
	// The persisted grant must also be live. Its rights cannot be replaced by a
	// caller-supplied decision; the signature only authenticates the ICS issuer.
	var grant string
	err = tx.QueryRow(ctx, `SELECT grant_id FROM grants WHERE grant_id=$1 AND task_id=$2 AND subject_principal_id=$3 AND actor_agent_id=$4
 AND (actor_instance_id IS NULL OR actor_instance_id=$5) AND status='active' AND revoked_at IS NULL
 AND parent_grant_id IS NULL AND root_grant_id=grant_id
 AND rights->'actions' ? $6 AND rights->'resources' ? $7 FOR SHARE`, d.Grant, p.TaskID, c.Principal, c.Agent, c.Instance, op.Action, op.Resource).Scan(&grant)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, Denied
	}
	if err != nil {
		return out, err
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, dim := range op.Dimensions {
		r := p.Reservations[dim]
		if r == "" || seen[r] || !amount(p.UpperBounds[dim], true) {
			return out, ReservationInvalid
		}
		seen[r] = true
		ids = append(ids, r)
	}
	rows, err := lockReservations(ctx, tx, ids)
	if err != nil {
		return out, err
	}
	for _, h := range rows {
		if h.Task != p.TaskID || h.State != "HELD" || h.Effect != "" || h.Attempt != "" || h.Purpose != op.Purpose || p.Reservations[h.Dimension] != h.ID {
			return out, ReservationInvalid
		}
		var enough bool
		err = tx.QueryRow(ctx, "SELECT $1::numeric >= $2::numeric", h.Amount, p.UpperBounds[h.Dimension]).Scan(&enough)
		if err != nil {
			return out, err
		}
		if !enough {
			return out, ReservationInvalid
		}
	}
	now, err = currentTime(ctx, tx)
	if err != nil {
		return out, err
	}
	if err = s.identity(ctx, tx, c); err != nil {
		return out, err
	}
	if err = active(t, now); err != nil {
		return out, err
	}
	if d.At.After(now) || now.Sub(d.At) > s.cfg.DecisionMaxAge || d.ExpiresAt.Before(now) {
		return out, DecisionMismatch
	}
	var live bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM leases WHERE lease_id=$1 AND task_id=$2 AND worker_instance_id=$3 AND epoch=$4 AND superseded_at IS NULL AND expires_at>=clock_timestamp())`, p.LeaseID, p.TaskID, c.Instance, p.Epoch).Scan(&live)
	if err != nil {
		return out, err
	}
	if !live {
		return out, LeaseInvalid
	}
	err = tx.QueryRow(ctx, "SELECT not_before<=clock_timestamp() AND expires_at>=clock_timestamp() FROM grants WHERE grant_id=$1", grant).Scan(&live)
	if err != nil {
		return out, err
	}
	if !live {
		return out, Denied
	}
	for _, h := range rows {
		_, err = tx.Exec(ctx, "UPDATE budget_reservations SET effect_id=$2 WHERE reservation_id=$1", h.ID, p.EffectID)
		if err != nil {
			return out, err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO effects(effect_id,task_id,command_id,target_ref,capability,params_digest,authority_digest,epoch,state,admitted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'ADMITTED',clock_timestamp())`, p.EffectID, p.TaskID, p.CommandID, p.Target, op.Capability, p.ParamsDigest, p.AuthorityDigest, p.Epoch)
	if err != nil {
		return out, err
	}
	_, err = tx.Exec(ctx, `UPDATE effects SET dispatch_grant_id=$2,dispatch_actor_instance_id=$3,
 target_ttl_us=NULLIF($4::bigint,0),dispatch_action=$5,dispatch_resource=$6 WHERE effect_id=$1`, p.EffectID, d.Grant, c.Instance, op.TargetTTL.Microseconds(), op.Action, op.Resource)
	if err != nil {
		return out, err
	}
	_, err = tx.Exec(ctx, "INSERT INTO effect_execution(effect_id) VALUES($1)", p.EffectID)
	if err != nil {
		return out, err
	}
	_, err = tx.Exec(ctx, "UPDATE tasks SET task_version=task_version+1 WHERE task_id=$1", p.TaskID)
	if err != nil {
		return out, err
	}
	if err = event(ctx, tx, p.TaskID, "EFFECT_ADMITTED", map[string]string{"effect_id": p.EffectID, "actor": c.Instance, "decision_id": d.ID}); err != nil {
		return out, err
	}
	// Durable dispatch intent is in the same transaction; no network call happens here.
	_, err = tx.Exec(ctx, `INSERT INTO outbox(task_id,event_type,payload,dedup_key) VALUES($1,'EFFECT_ADMITTED',jsonb_build_object('effect_id',$2::text),$3)`, p.TaskID, p.EffectID, "effect:"+p.EffectID)
	if err != nil {
		return out, err
	}
	if err = commit(ctx, tx); err != nil {
		return Effect{}, err
	}
	out.State = "ADMITTED"
	return out, nil
}

// Resolve consumes signed business evidence, including after cancellation or
// takeover. It does not require a live worker lease and never resends an effect.
func (s *Store) Resolve(ctx context.Context, signed Envelope) (Effect, error) {
	var r Result
	if err := verify(signed, s.cfg.ResultKeys, &r); err != nil {
		return Effect{}, err
	}
	out := Effect{ID: r.Effect, State: r.Outcome}
	if r.Kind != "anet.result/1" || r.Task == "" || r.Effect == "" || r.Audience != s.cfg.Domain || r.Evidence == "" || !digestRE.MatchString(r.ParamsDigest) {
		return out, Invalid
	}
	if r.Outcome != "COMMITTED" && r.Outcome != "REJECTED" && r.Outcome != "UNKNOWN" {
		return out, Invalid
	}
	digest, err := canonical.Digest(signed.Payload)
	if err != nil {
		return out, Invalid
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var taskID string
	err = tx.QueryRow(ctx, "SELECT task_id FROM tasks WHERE task_id=$1 AND domain_id=$2 FOR UPDATE", r.Task, s.cfg.Domain).Scan(&taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, Denied
	}
	if err != nil {
		return out, err
	}
	var state, pd, prior string
	err = tx.QueryRow(ctx, "SELECT state,params_digest,COALESCE(result_digest,'') FROM effects WHERE effect_id=$1 AND task_id=$2", r.Effect, r.Task).Scan(&state, &pd, &prior)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, Denied
	}
	if err != nil {
		return out, err
	}
	if pd != r.ParamsDigest {
		return out, ResultConflict
	}
	if prior == digest {
		out.State = state
		out.Existing = true
		return out, commit(ctx, tx)
	}
	if state == "COMMITTED" || state == "REJECTED" {
		return out, ResultConflict
	}
	if state != "ADMITTED" && state != "UNKNOWN" {
		return out, ResultConflict
	}
	if r.Outcome == "UNKNOWN" {
		_, err = tx.Exec(ctx, "UPDATE effects SET state='UNKNOWN',result_ref=$2,result_digest=$3 WHERE effect_id=$1", r.Effect, r.Evidence, digest)
		if err != nil {
			return out, err
		}
		if err = event(ctx, tx, r.Task, "EFFECT_UNKNOWN", map[string]string{"effect_id": r.Effect}); err != nil {
			return out, err
		}
		return out, commit(ctx, tx)
	}
	if r.UsageSource != "provider" && r.UsageSource != "metered" && r.UsageSource != "self_reported" {
		return out, Invalid
	}
	query, err := tx.Query(ctx, "SELECT reservation_id FROM budget_reservations WHERE effect_id=$1 ORDER BY reservation_id", r.Effect)
	if err != nil {
		return out, err
	}
	ids, err := pgx.CollectRows(query, pgx.RowTo[string])
	if err != nil {
		return out, err
	}
	holds, err := lockReservations(ctx, tx, ids)
	if err != nil {
		return out, err
	}
	if len(r.Usage) != len(holds) {
		return out, ReservationInvalid
	}
	costs := map[string]string{}
	overrun := false
	for _, h := range holds {
		usage, ok := r.Usage[h.Dimension]
		if !ok || !amount(usage, false) || h.Task != r.Task || h.Effect != r.Effect || h.State != "HELD" {
			return out, ReservationInvalid
		}
		var exceeded bool
		err = tx.QueryRow(ctx, "SELECT $1::numeric > $2::numeric", usage, h.Amount).Scan(&exceeded)
		if err != nil {
			return out, err
		}
		overrun = overrun || exceeded
		if r.UsageSource == "self_reported" {
			usage = h.Amount
		}
		costs[h.ID] = usage
	}
	if overrun {
		if err = audit(ctx, tx, s.cfg.Domain, r.Task, r.Effect, "budget.violation", r); err != nil {
			return out, err
		}
		_, err = tx.Exec(ctx, "UPDATE tasks SET budget_frozen=TRUE,task_version=task_version+1 WHERE task_id=$1", r.Task)
		if err != nil {
			return out, err
		}
		if err = commit(ctx, tx); err != nil {
			return out, err
		}
		out.State = state
		return out, BudgetViolation
	}
	for _, h := range holds {
		_, err = tx.Exec(ctx, "UPDATE budget_reservations SET state='SETTLED',settled_at=clock_timestamp() WHERE reservation_id=$1", h.ID)
		if err != nil {
			return out, err
		}
		_, err = tx.Exec(ctx, "UPDATE budget_accounts SET settled=settled+$2::numeric WHERE account_id=$1", h.Account, costs[h.ID])
		if err != nil {
			return out, err
		}
	}
	_, err = tx.Exec(ctx, "UPDATE effects SET state=$2,result_ref=$3,result_digest=$4,finalized_at=clock_timestamp() WHERE effect_id=$1", r.Effect, r.Outcome, r.Evidence, digest)
	if err != nil {
		return out, err
	}
	if err = event(ctx, tx, r.Task, "EFFECT_RESULT", r); err != nil {
		return out, err
	}
	if err = audit(ctx, tx, s.cfg.Domain, r.Task, r.Effect, "budget.settle", r); err != nil {
		return out, err
	}
	if err = commit(ctx, tx); err != nil {
		return Effect{}, err
	}
	return out, nil
}
