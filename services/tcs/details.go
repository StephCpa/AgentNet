package tcs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type EffectDetails struct {
	ID, Task, Command, Target, Capability, State, ParamsDigest, AuthorityDigest, ResultRef string
	Epoch                                                                                  int64
}
type ReceiptDetails struct {
	ID, Kind, Issuer, Task, Command, Effect, Verifier, Policy, Manifest, Result string
	At                                                                          time.Time
}
type BudgetDetails struct{ ID, Dimension, Limit, Settled, Held, Delegated string }
type TaskDetails struct {
	ID, Root, Parent, Status, Agent, AssigneeDomain string
	Version, Epoch, CancelVersion                   int64
	Frozen                                          bool
	Deadline                                        *time.Time
	Effects                                         []EffectDetails
	Receipts                                        []ReceiptDetails
	Budgets                                         []BudgetDetails
}

func (s *Store) Details(ctx context.Context, c Caller, id string) (TaskDetails, error) {
	var d TaskDetails
	tx, err := s.begin(ctx)
	if err != nil {
		return d, err
	}
	defer tx.Rollback(ctx)
	if _, err = s.lockTask(ctx, tx, c, id); err != nil {
		return d, err
	}
	err = tx.QueryRow(ctx, `SELECT task_id,root_task_id,COALESCE(parent_task_id,''),status,COALESCE(assignee_agent_id,''),COALESCE(assignee_domain_id,''),task_version,current_epoch,COALESCE(cancel_version,0),budget_frozen,deadline FROM tasks WHERE task_id=$1`, id).Scan(&d.ID, &d.Root, &d.Parent, &d.Status, &d.Agent, &d.AssigneeDomain, &d.Version, &d.Epoch, &d.CancelVersion, &d.Frozen, &d.Deadline)
	if err != nil {
		return d, err
	}
	rows, err := tx.Query(ctx, `SELECT effect_id,task_id,command_id,target_ref,capability,state,params_digest,authority_digest,COALESCE(result_ref,''),epoch FROM effects WHERE task_id=$1 ORDER BY effect_id`, id)
	if err != nil {
		return d, err
	}
	d.Effects, err = pgx.CollectRows(rows, pgx.RowToStructByPos[EffectDetails])
	if err != nil {
		return d, err
	}
	rows, err = tx.Query(ctx, `SELECT receipt_id,kind,issuer_domain_id,task_id,COALESCE(command_id,''),COALESCE(effect_id,''),COALESCE(verifier,''),COALESCE(policy_version,''),COALESCE(evidence_manifest_ref,''),COALESCE(result_ref,''),issued_at FROM receipts WHERE task_id=$1 ORDER BY issued_at,receipt_id`, id)
	if err != nil {
		return d, err
	}
	d.Receipts, err = pgx.CollectRows(rows, pgx.RowToStructByPos[ReceiptDetails])
	if err != nil {
		return d, err
	}
	rows, err = tx.Query(ctx, `SELECT a.account_id,a.dimension,a.budget_limit::text,a.settled::text,COALESCE((SELECT sum(amount) FROM budget_reservations r WHERE r.account_id=a.account_id AND r.state='HELD'),0)::text,a.delegated::text FROM budget_accounts a WHERE task_id=$1 ORDER BY dimension,account_id`, id)
	if err != nil {
		return d, err
	}
	d.Budgets, err = pgx.CollectRows(rows, pgx.RowToStructByPos[BudgetDetails])
	if err != nil {
		return d, err
	}
	return d, commit(ctx, tx)
}

type ReservationDetails struct{ ID, Account, State, Amount string }

func (s *Store) Reservation(ctx context.Context, c Caller, taskID, id string) (ReservationDetails, error) {
	var d ReservationDetails
	tx, err := s.begin(ctx)
	if err != nil {
		return d, err
	}
	defer tx.Rollback(ctx)
	if _, err = s.lockTask(ctx, tx, c, taskID); err != nil {
		return d, err
	}
	err = tx.QueryRow(ctx, `SELECT reservation_id,account_id,state,amount::text FROM budget_reservations WHERE reservation_id=$1 AND holder_task_id=$2`, id, taskID).Scan(&d.ID, &d.Account, &d.State, &d.Amount)
	if err != nil {
		return d, err
	}
	return d, commit(ctx, tx)
}
