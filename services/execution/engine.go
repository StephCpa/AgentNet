package execution

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentnet/agentnet/internal/amount"
	"github.com/agentnet/agentnet/internal/authority"
	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/services/tcs"
	"github.com/jackc/pgx/v5"
)

func (e *Engine) claim(ctx context.Context, id string) (*job, error) {
	tx, err := e.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	j := &job{EffectID: id}
	err = tx.QueryRow(ctx, `SELECT t.task_id FROM tasks t JOIN effects f ON f.task_id=t.task_id WHERE f.effect_id=$1 AND t.domain_id=$2 FOR UPDATE OF t`, id, e.cfg.Domain).Scan(&j.TaskID)
	if err != nil {
		return nil, err
	}
	var due, escalated, finished, notified bool
	var payload, signature []byte
	var key string
	err = tx.QueryRow(ctx, `SELECT f.target_ref,f.capability,f.params_digest,f.state,f.epoch,x.send_count,COALESCE(x.attempt_id::text,''),x.key_expires_at,x.reconcile_count,x.retry_count,x.created_at,
 (x.lease_until IS NULL OR x.lease_until<=clock_timestamp()) AND x.next_attempt_at<=clock_timestamp(),x.escalated,x.finished,x.result_payload,x.result_signature,COALESCE(x.result_key_id,''),
 EXISTS(SELECT 1 FROM delivery_inbox i WHERE i.source_domain=$2 AND i.destination='local-control' AND i.dedup_key='effect:'||f.effect_id AND i.task_id=f.task_id AND i.event_type='EFFECT_ADMITTED' AND i.payload->>'effect_id'=f.effect_id)
 FROM effects f JOIN effect_execution x USING(effect_id) WHERE f.effect_id=$1 FOR UPDATE OF x`, id, e.cfg.Domain).
		Scan(&j.Target, &j.Capability, &j.Digest, &j.State, &j.Epoch, &j.Sends, &j.Attempt, &j.KeyUntil, &j.Reconciles, &j.Retries, &j.Created, &due, &escalated, &finished, &payload, &signature, &key, &notified)
	if err != nil {
		return nil, err
	}
	if j.State == Committed || j.State == "REJECTED" {
		_, err = tx.Exec(ctx, `UPDATE effect_execution SET finished=TRUE,lease_token=NULL,lease_until=NULL WHERE effect_id=$1`, id)
		if err != nil {
			return nil, err
		}
		return nil, tx.Commit(ctx)
	}
	if !due || escalated || finished || (!notified && j.Sends == 0) {
		return nil, ErrBusy
	}
	if payload != nil {
		j.Saved = &tcs.Envelope{KeyID: key, Payload: payload, Signature: signature}
	}
	err = tx.QueryRow(ctx, `UPDATE effect_execution SET lease_token=gen_random_uuid(),lease_until=clock_timestamp()+$2::bigint*interval '1 microsecond' WHERE effect_id=$1 RETURNING lease_token::text`, id, e.cfg.Lease.Microseconds()).Scan(&j.Token)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return j, nil
}

// startSend is the last durable gate before external I/O. A crash after this
// marker is an uncertain send, even if the actual network call never happened.
func (e *Engine) startSend(ctx context.Context, j *job) error {
	tx, err := e.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var allowed bool
	err = tx.QueryRow(ctx, `SELECT t.current_epoch=f.epoch AND t.cancel_version IS NULL AND NOT t.budget_frozen
 AND t.status NOT IN ('SUCCEEDED','FAILED','CANCELLED') AND t.deadline>=clock_timestamp()
 FROM tasks t JOIN effects f ON f.task_id=t.task_id WHERE f.effect_id=$1 AND t.domain_id=$2 FOR UPDATE OF t`, j.EffectID, e.cfg.Domain).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrFenced
	}
	var grant string
	err = tx.QueryRow(ctx, "SELECT dispatch_grant_id FROM effects WHERE effect_id=$1", j.EffectID).Scan(&grant)
	if err != nil {
		return err
	}
	chain, err := authority.LockChain(ctx, tx, grant, false)
	if errors.Is(err, authority.ErrInvalidChain) {
		return ErrFenced
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT g.grant_id FROM effects f JOIN grants g ON g.grant_id=f.dispatch_grant_id
 JOIN tasks t ON t.task_id=f.task_id JOIN agent_instances i ON i.instance_id=f.dispatch_actor_instance_id
 JOIN agents a ON a.agent_id=i.agent_id JOIN principals p ON p.principal_id=a.owner_principal_id
 WHERE f.effect_id=$1 AND g.task_id=f.task_id AND g.subject_principal_id=p.principal_id AND t.owner_principal_id=p.principal_id
 AND g.actor_agent_id=a.agent_id AND (g.actor_instance_id IS NULL OR g.actor_instance_id=i.instance_id)
 AND g.status='active' AND g.revoked_at IS NULL
 AND g.not_before<=clock_timestamp() AND g.expires_at>=clock_timestamp()
 AND g.rights->'actions' ? f.dispatch_action AND g.rights->'resources' ? f.dispatch_resource
 AND (g.consent_id IS NULL OR (g.rights->'data_classes' ? f.dispatch_data_class AND g.rights->'spatial'->'domains' ? $2
 AND EXISTS(SELECT 1 FROM root_consents c WHERE c.consent_id=g.consent_id AND c.version=g.consent_version AND c.revoked_at IS NULL AND c.not_before<=clock_timestamp() AND c.expires_at>=clock_timestamp())))
 AND i.revoked_at IS NULL AND p.revoked_at IS NULL AND a.status='active' AND p.domain_id=$2 AND a.domain_id=$2
`, j.EffectID, e.cfg.Domain).Scan(&grant)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	}
	if err != nil {
		return err
	}
	var count int
	var live bool
	err = tx.QueryRow(ctx, `SELECT send_count,lease_token::text=$2 AND lease_until>clock_timestamp() AND NOT finished AND NOT escalated AND result_payload IS NULL
 FROM effect_execution WHERE effect_id=$1 FOR UPDATE`, j.EffectID, j.Token).Scan(&count, &live)
	if err != nil {
		return err
	}
	if !live || count != j.Sends {
		return ErrFenced
	}
	// Re-check the time-sensitive grant and actor state after any job lock wait.
	err = tx.QueryRow(ctx, `SELECT g.expires_at>=clock_timestamp() AND t.deadline>=clock_timestamp()
 AND i.revoked_at IS NULL AND p.revoked_at IS NULL AND a.status='active'
 AND (g.consent_id IS NULL OR EXISTS(SELECT 1 FROM root_consents c WHERE c.consent_id=g.consent_id AND c.version=g.consent_version AND c.revoked_at IS NULL AND c.expires_at>=clock_timestamp()))
 FROM effects f JOIN grants g ON g.grant_id=f.dispatch_grant_id JOIN tasks t ON t.task_id=f.task_id
 JOIN agent_instances i ON i.instance_id=f.dispatch_actor_instance_id JOIN agents a ON a.agent_id=i.agent_id
 JOIN principals p ON p.principal_id=a.owner_principal_id WHERE f.effect_id=$1`, j.EffectID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrFenced
	}
	chainLive, err := chain.Live(ctx, tx)
	if err != nil {
		return err
	}
	if !chainLive {
		return ErrFenced
	}
	if count > 0 && j.Capability != "idempotent-key" {
		return ErrFenced
	}
	err = tx.QueryRow(ctx, `UPDATE effect_execution x SET send_count=send_count+1,attempt_id=gen_random_uuid(),
 key_expires_at=CASE WHEN f.capability='idempotent-key' THEN COALESCE(x.key_expires_at,clock_timestamp()+f.target_ttl_us*interval '1 microsecond') ELSE NULL END
 FROM effects f WHERE x.effect_id=$1 AND f.effect_id=x.effect_id AND x.lease_until>clock_timestamp()
 AND (x.key_expires_at IS NULL OR x.key_expires_at>clock_timestamp())
 AND x.created_at+$2::bigint*interval '1 microsecond'>clock_timestamp()
 RETURNING x.attempt_id::text,x.key_expires_at,x.send_count`, j.EffectID, e.cfg.RecoveryWindow.Microseconds()).Scan(&j.Attempt, &j.KeyUntil, &j.Sends)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (e *Engine) sign(r tcs.Result) (tcs.Envelope, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return tcs.Envelope{}, err
	}
	b, err = canonical.Bytes(b)
	if err != nil {
		return tcs.Envelope{}, err
	}
	return tcs.Envelope{KeyID: e.cfg.ResultKeyID, Payload: b, Signature: ed25519.Sign(e.cfg.ResultKey, b)}, nil
}
func (e *Engine) result(j *job, o Observation) tcs.Result {
	outcome := o.Status
	if outcome == Final {
		outcome = "REJECTED"
	}
	return tcs.Result{Kind: "anet.result/1", Task: j.TaskID, Effect: j.EffectID, ParamsDigest: j.Digest, Audience: e.cfg.Domain, Outcome: outcome, Usage: o.Usage, UsageSource: "provider", Evidence: o.Evidence}
}
func (e *Engine) finish(ctx context.Context, j *job) error {
	_, err := e.db.Exec(ctx, `UPDATE effect_execution SET finished=TRUE,lease_token=NULL,lease_until=NULL WHERE effect_id=$1 AND lease_token::text=$2`, j.EffectID, j.Token)
	return err
}
func (e *Engine) terminal(ctx context.Context, j *job, o Observation) error {
	rows, err := e.db.Query(ctx, `SELECT a.dimension FROM budget_reservations r JOIN budget_accounts a USING(account_id) WHERE r.effect_id=$1`, j.EffectID)
	if err != nil {
		return err
	}
	dims, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(dims) == 0 || len(dims) != len(o.Usage) {
		return e.uncertain(ctx, j, "invalid_usage_evidence", true)
	}
	for _, dim := range dims {
		if !amount.Valid(o.Usage[dim]) {
			return e.uncertain(ctx, j, "invalid_usage_evidence", true)
		}
	}
	signed, err := e.sign(e.result(j, o))
	if err != nil {
		return err
	}
	// A trustworthy late business result remains useful after a lease expires.
	// Store the first terminal observation before reporting it; retries send the
	// exact original signed evidence instead of inventing a new receipt.
	_, err = e.db.Exec(ctx, `UPDATE effect_execution SET result_payload=$2::jsonb,result_signature=$3,result_key_id=$4 WHERE effect_id=$1 AND result_payload IS NULL`, j.EffectID, string(signed.Payload), signed.Signature, signed.KeyID)
	if err != nil {
		return err
	}
	var saved tcs.Envelope
	err = e.db.QueryRow(ctx, `SELECT result_payload,result_signature,result_key_id FROM effect_execution WHERE effect_id=$1`, j.EffectID).Scan(&saved.Payload, &saved.Signature, &saved.KeyID)
	if err != nil {
		return err
	}
	return e.reportSaved(ctx, j, saved)
}
func (e *Engine) reportSaved(ctx context.Context, j *job, saved tcs.Envelope) error {
	_, err := e.cfg.Resolver.Resolve(ctx, saved)
	if errors.Is(err, tcs.BudgetViolation) {
		return e.pause(ctx, j, "budget_violation", true)
	}
	if err != nil {
		return err
	}
	return e.finish(ctx, j)
}
func (e *Engine) uncertain(ctx context.Context, j *job, reason string, escalate bool) error {
	// A failed pre-send lookup is not an uncertain business write. Preserve the
	// ADMITTED state and retry lookup; the durable send marker is still absent.
	if j.Sends == 0 && j.State == "ADMITTED" {
		return e.pause(ctx, j, reason, escalate)
	}
	signed, err := e.sign(e.result(j, Observation{Status: Unknown, Evidence: "adapter:uncertain:" + j.EffectID}))
	if err != nil {
		return err
	}
	_, err = e.cfg.Resolver.Resolve(ctx, signed)
	if errors.Is(err, tcs.ResultConflict) {
		var terminal bool
		if readErr := e.db.QueryRow(ctx, `SELECT state IN ('COMMITTED','REJECTED') FROM effects WHERE effect_id=$1`, j.EffectID).Scan(&terminal); readErr != nil {
			return readErr
		}
		if terminal {
			return e.finish(ctx, j)
		}
	}
	if err != nil {
		return err
	}
	return e.pause(ctx, j, reason, escalate)
}
func (e *Engine) pause(ctx context.Context, j *job, reason string, escalate bool) error {
	tx, err := e.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var status string
	if err = tx.QueryRow(ctx, `SELECT status FROM tasks WHERE task_id=$1 FOR UPDATE`, j.TaskID).Scan(&status); err != nil {
		return err
	}
	index := j.Retries
	if index >= len(e.cfg.Backoff) {
		index = len(e.cfg.Backoff) - 1
	}
	var n int
	var escalation bool
	err = tx.QueryRow(ctx, `UPDATE effect_execution SET retry_count=retry_count+1,lease_token=NULL,lease_until=NULL,
 next_attempt_at=clock_timestamp()+$3::bigint*interval '1 microsecond',
 escalated=$4 OR created_at+$5::bigint*interval '1 microsecond'<=clock_timestamp()
 WHERE effect_id=$1 AND lease_token::text=$2 RETURNING retry_count,escalated`, j.EffectID, j.Token, e.cfg.Backoff[index].Microseconds(), escalate, e.cfg.RecoveryWindow.Microseconds()).Scan(&n, &escalation)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	}
	if err != nil {
		return err
	}
	if escalation {
		if status != "SUCCEEDED" && status != "FAILED" && status != "CANCELLED" {
			_, err = tx.Exec(ctx, `UPDATE tasks SET status='WAITING',task_version=task_version+1 WHERE task_id=$1`, j.TaskID)
			if err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO task_events(task_id,root_task_id,event_type,payload) SELECT task_id,root_task_id,'RECOVERY_ESCALATED',jsonb_build_object('effect_id',$2::text,'reason',$3::text) FROM tasks WHERE task_id=$1`, j.TaskID, j.EffectID, reason)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO outbox(task_id,event_type,payload,dedup_key) VALUES($1,'RECOVERY_ESCALATED',jsonb_build_object('effect_id',$2::text,'reason',$3::text),$4)`, j.TaskID, j.EffectID, reason, "recovery:"+j.EffectID)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (e *Engine) queryRecord(ctx context.Context, j *job, status string) error {
	tx, err := e.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var n int
	err = tx.QueryRow(ctx, `UPDATE effect_execution SET reconcile_count=reconcile_count+1 WHERE effect_id=$1 RETURNING reconcile_count`, j.EffectID).Scan(&n)
	if err != nil {
		return err
	}
	method := "query"
	if j.Capability == "idempotent-key" {
		method = "reconcile_api"
	}
	outcome := "STILL_UNKNOWN"
	if status == Committed {
		outcome = Committed
	}
	if status == Final {
		outcome = "REJECTED"
	}
	_, err = tx.Exec(ctx, `INSERT INTO reconciliations(effect_id,attempt_no,method,outcome,detail,finished_at) VALUES($1,$2,$3,$4,jsonb_build_object('query_status',$5::text),clock_timestamp())`, j.EffectID, n, method, outcome, status)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func valid(j *job, o Observation) bool {
	if o.EffectID != j.EffectID || o.ParamsDigest != j.Digest || o.Evidence == "" {
		return false
	}
	switch o.Status {
	case Committed, Pending, Absent, Unknown:
		return true
	case Final:
		if j.Attempt == "" || o.AttemptID != j.Attempt {
			return false
		}
		if o.FinalScope == "effect" {
			return true
		}
		return o.FinalScope == "attempt" && j.Sends == 1
	default:
		return false
	}
}
func (e *Engine) Execute(ctx context.Context, id string) error {
	j, err := e.claim(ctx, id)
	if err != nil {
		return err
	}
	if j == nil {
		return nil
	}
	if j.Saved != nil {
		return e.reportSaved(ctx, j, *j.Saved)
	}
	binding, ok := e.cfg.Targets[j.Target]
	if !ok || binding.Capability != j.Capability {
		return e.uncertain(ctx, j, "target_binding", true)
	}
	callCtx, cancel := context.WithTimeout(ctx, e.cfg.Lease)
	defer cancel()
	req := Request{EffectID: j.EffectID, AttemptID: j.Attempt, ParamsDigest: j.Digest, KeyExpiresAt: j.KeyUntil}
	if j.Capability == "opaque" && (j.Sends > 0 || j.State == Unknown) {
		return e.uncertain(ctx, j, "opaque_requires_manual", true)
	}
	if j.Capability == "queryable" || j.Sends > 0 || j.State == Unknown {
		o, queryErr := binding.Target.Query(callCtx, req)
		if queryErr != nil {
			o = Observation{Status: Unknown, EffectID: j.EffectID, ParamsDigest: j.Digest, Evidence: "adapter:query_unavailable"}
		}
		if !valid(j, o) {
			if err = e.queryRecord(ctx, j, Unknown); err != nil {
				return err
			}
			return e.uncertain(ctx, j, "query_evidence_mismatch", true)
		}
		if err = e.queryRecord(ctx, j, o.Status); err != nil {
			return err
		}
		if queryErr != nil && j.Capability != "idempotent-key" {
			return e.uncertain(ctx, j, "query_unknown", false)
		}
		switch o.Status {
		case Committed, Final:
			return e.terminal(ctx, j, o)
		case Pending:
			return e.uncertain(ctx, j, "query_pending", false)
		}
		if j.Capability == "queryable" && (j.Sends > 0 || j.State == Unknown || o.Status != Absent) {
			return e.uncertain(ctx, j, "query_"+o.Status, false)
		}
	}
	if j.State == Unknown && j.Sends == 0 {
		return e.uncertain(ctx, j, "unknown_without_send_provenance", true)
	}
	payload, err := e.cfg.Payloads.Load(callCtx, j.Digest)
	if err != nil {
		return e.uncertain(ctx, j, "parameters_unavailable", true)
	}
	digest, err := canonical.Digest(payload)
	if err != nil || digest != j.Digest {
		return e.uncertain(ctx, j, "parameters_mismatch", true)
	}
	if err = e.startSend(ctx, j); err != nil {
		if errors.Is(err, ErrFenced) {
			return e.uncertain(ctx, j, "send_fenced_or_ttl_expired", true)
		}
		return err
	}
	req.AttemptID = j.Attempt
	req.KeyExpiresAt = j.KeyUntil
	req.Parameters = payload
	o, sendErr := binding.Target.Send(callCtx, req)
	if sendErr != nil || !valid(j, o) {
		return e.uncertain(ctx, j, "send_unknown", j.Capability == "opaque")
	}
	if o.Status == Committed || o.Status == Final {
		return e.terminal(ctx, j, o)
	}
	return e.uncertain(ctx, j, "send_"+o.Status, j.Capability == "opaque")
}

// Step resumes durable work; notifications only wake the first send. After a
// crash/UNKNOWN, the job itself drives reconciliation independently of messages.
func (e *Engine) Step(ctx context.Context) (bool, error) {
	var id string
	err := e.db.QueryRow(ctx, `SELECT x.effect_id FROM effect_execution x JOIN effects f USING(effect_id) JOIN tasks t ON t.task_id=f.task_id
 WHERE t.domain_id=$1 AND NOT x.finished AND NOT x.escalated AND x.next_attempt_at<=clock_timestamp()
 AND (x.lease_until IS NULL OR x.lease_until<=clock_timestamp()) AND
 (x.send_count>0 OR EXISTS(SELECT 1 FROM delivery_inbox i WHERE i.source_domain=$1 AND i.destination='local-control' AND i.dedup_key='effect:'||x.effect_id AND i.task_id=f.task_id AND i.event_type='EFFECT_ADMITTED'))
 ORDER BY x.next_attempt_at,x.effect_id LIMIT 1`, e.cfg.Domain).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = e.Execute(ctx, id)
	if errors.Is(err, ErrBusy) {
		return false, nil
	}
	return true, err
}
func (e *Engine) Run(ctx context.Context) error {
	for {
		work, err := e.Step(ctx)
		if err != nil {
			return fmt.Errorf("execution: %w", err)
		}
		if work {
			continue
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
