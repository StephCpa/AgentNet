package tcs

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/internal/localtx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type LocalEffect struct {
	Effect    Effect
	Result    localtx.Result
	ResultRef string
}
type localCall struct {
	parameters []byte
	result     localtx.Result
}

// localDB deliberately exposes neither pgx.Tx nor its underlying connection.
// Handlers remain trusted code; see internal/localtx's contract.
type localDB struct{ tx pgx.Tx }

func (d localDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return d.tx.Exec(ctx, sql, args...)
}
func (d localDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return d.tx.QueryRow(ctx, sql, args...)
}

// ExecuteLocal admits, executes and settles atomically. A returned error never
// exposes a provisional result. If COMMIT's reply is lost, retry the same effect.
func (s *Store) ExecuteLocal(ctx context.Context, c Caller, p Proposal, signed Envelope, parameters []byte) (LocalEffect, error) {
	if len(parameters) == 0 || len(parameters) > 65536 {
		return LocalEffect{}, Invalid
	}
	raw, err := canonical.Bytes(parameters)
	if err != nil {
		return LocalEffect{}, Invalid
	}
	digest, err := canonical.Digest(raw)
	if err != nil || digest != p.ParamsDigest {
		return LocalEffect{}, DecisionMismatch
	}
	call := &localCall{parameters: raw}
	e, err := s.admit(ctx, c, p, signed, call)
	if err != nil {
		return LocalEffect{}, err
	}
	return LocalEffect{Effect: e, Result: call.result, ResultRef: "local-effect:" + p.EffectID}, nil
}

func loadLocal(ctx context.Context, tx pgx.Tx, effect string, call *localCall) error {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT r.result FROM local_effect_results r JOIN effects e USING(effect_id) WHERE effect_id=$1 AND e.capability='local-tx' AND e.state IN ('COMMITTED','REJECTED')`, effect).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invalid
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &call.result); err != nil {
		return err
	}
	call.result.Payload, err = canonical.Bytes(call.result.Payload)
	return err
}

func (s *Store) finishLocal(ctx context.Context, tx pgx.Tx, p Proposal, op Operation, holds []held, call *localCall) error {
	r, err := s.cfg.LocalHandlers[p.Target].Execute(ctx, localDB{tx}, localtx.Request{Domain: s.cfg.Domain, TaskID: p.TaskID, EffectID: p.EffectID, Resource: op.Resource, Parameters: call.parameters})
	if errors.Is(err, localtx.Invalid) {
		return Invalid
	}
	if err != nil {
		return err
	}
	if (r.Outcome != "COMMITTED" && r.Outcome != "REJECTED") || len(r.Payload) == 0 || len(r.Payload) > 65536 || len(r.Usage) != len(holds) {
		return Invalid
	}
	r.Payload, err = canonical.Bytes(r.Payload)
	if err != nil {
		return Invalid
	}
	for _, h := range holds {
		u, ok := r.Usage[h.Dimension]
		if !ok || !amount(u, false) {
			return Invalid
		}
		var within bool
		if err = tx.QueryRow(ctx, "SELECT $1::numeric <= $2::numeric AND $1::numeric <= $3::numeric", u, h.Amount, p.UpperBounds[h.Dimension]).Scan(&within); err != nil {
			return err
		}
		// All local writes can roll back; never commit an overrun business write.
		if !within {
			return BudgetExhausted
		}
	}
	for _, h := range holds {
		if _, err = tx.Exec(ctx, "UPDATE budget_reservations SET state='SETTLED',settled_at=clock_timestamp() WHERE reservation_id=$1", h.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE budget_accounts SET settled=settled+$2::numeric WHERE account_id=$1", h.Account, r.Usage[h.Dimension]); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	digest, err := canonical.Digest(raw)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE effects SET state=$2,result_ref=$3,result_digest=$4,finalized_at=clock_timestamp() WHERE effect_id=$1", p.EffectID, r.Outcome, "local-effect:"+p.EffectID, digest); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO local_effect_results(effect_id,result) VALUES($1,$2::jsonb)", p.EffectID, string(raw)); err != nil {
		return err
	}
	if err = event(ctx, tx, p.TaskID, "EFFECT_RESULT", map[string]string{"effect_id": p.EffectID, "state": r.Outcome, "result_digest": digest}); err != nil {
		return err
	}
	if err = audit(ctx, tx, s.cfg.Domain, p.TaskID, p.EffectID, "budget.settle", r.Usage); err != nil {
		return err
	}
	call.result = r
	return nil
}
