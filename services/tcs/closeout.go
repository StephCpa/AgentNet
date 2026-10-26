package tcs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const CloseoutPending Code = "ANET-BUD-SUBTASK-NOT-READY"

type DimensionCloseout struct {
	Dimension string `json:"dimension"`
	Allocated string `json:"allocated"`
	Settled   string `json:"settled"`
	Returned  string `json:"returned"`
}
type SubtaskCloseout struct {
	Child      string              `json:"child_task_id"`
	Parent     string              `json:"parent_task_id"`
	At         time.Time           `json:"closed_at"`
	Existing   bool                `json:"existing"`
	Dimensions []DimensionCloseout `json:"dimensions"`
}
type closeAccount struct {
	ID, Task, Parent, Dimension, Limit, Settled, Delegated string
	Closed                                                 bool
}

// CloseSubtaskBudget rolls up authoritative costs exactly once. Neither a
// worker's claimed usage nor cancellation alone establishes unused budget.
func (s *Store) CloseSubtaskBudget(ctx context.Context, c Caller, child string) (SubtaskCloseout, error) {
	var out SubtaskCloseout
	if child == "" {
		return out, Invalid
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var parent string
	err = tx.QueryRow(ctx, "SELECT parent_task_id FROM task_delegations WHERE child_task_id=$1", child).Scan(&parent)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, Denied
	}
	if err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT task_id FROM tasks WHERE task_id=ANY($1) AND domain_id=$2 ORDER BY task_id FOR UPDATE`, []string{parent, child}, s.cfg.Domain)
	if err != nil {
		return out, err
	}
	tasks, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return out, err
	}
	if len(tasks) != 2 {
		return out, Denied
	}
	t, err := s.lockTask(ctx, tx, c, child)
	if err != nil {
		return out, err
	}
	var actualParent, command string
	var prior []byte
	err = tx.QueryRow(ctx, `SELECT parent_task_id,command_id,close_result FROM task_delegations WHERE child_task_id=$1 FOR UPDATE`, child).Scan(&actualParent, &command, &prior)
	if err != nil {
		return out, err
	}
	if actualParent != parent {
		return out, Invalid
	}
	var linked bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks c JOIN tasks p ON p.task_id=c.parent_task_id WHERE c.task_id=$1 AND p.task_id=$2 AND c.root_task_id=p.root_task_id AND c.owner_principal_id=p.owner_principal_id)`, child, parent).Scan(&linked); err != nil {
		return out, err
	}
	if !linked {
		return out, Invalid
	}

	if prior != nil {
		if json.Unmarshal(prior, &out) != nil {
			return out, Invalid
		}
		out.Existing = true
		return out, commit(ctx, tx)
	}
	if t.Frozen || (t.Status != "CANCELLED" && t.Status != "FAILED" && t.Status != "SUCCEEDED") {
		return out, CloseoutPending
	}
	var ready bool
	err = tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM effects WHERE task_id=$1 AND state NOT IN ('COMMITTED','REJECTED'))
 AND NOT EXISTS(SELECT 1 FROM tasks t LEFT JOIN task_delegations d ON d.child_task_id=t.task_id WHERE t.parent_task_id=$1 AND d.closed_at IS NULL)`, child).Scan(&ready)
	if err != nil {
		return out, err
	}
	if !ready {
		return out, CloseoutPending
	}
	rows, err = tx.Query(ctx, `SELECT account_id,task_id,COALESCE(parent_account_id,''),dimension,budget_limit::text,settled::text,delegated::text,closed_at IS NOT NULL FROM budget_accounts WHERE task_id=ANY($1) ORDER BY account_id FOR UPDATE`, []string{parent, child})
	if err != nil {
		return out, err
	}
	accounts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[closeAccount])
	if err != nil {
		return out, err
	}
	if len(accounts) != 8 {
		return out, Invalid
	}
	byID := map[string]closeAccount{}
	children := map[string]closeAccount{}
	for _, a := range accounts {
		byID[a.ID] = a
		if a.Closed {
			return out, CloseoutPending
		}
		if a.Task == child {
			children[a.Dimension] = a
		}
	}
	if len(children) != 4 {
		return out, Invalid
	}
	err = tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM budget_reservations r JOIN budget_accounts a USING(account_id) WHERE (r.holder_task_id=$1 OR a.task_id=$1) AND r.state='HELD') AND NOT EXISTS(SELECT 1 FROM budget_accounts WHERE task_id=$1 AND delegated<>0)`, child).Scan(&ready)
	if err != nil {
		return out, err
	}
	if !ready {
		return out, CloseoutPending
	}
	out = SubtaskCloseout{Child: child, Parent: parent}
	out.At, err = currentTime(ctx, tx)
	if err != nil {
		return out, err
	}
	for _, dim := range []string{"calls", "messages", "money", "tokens"} {
		a, ok := children[dim]
		if !ok {
			return out, Invalid
		}
		p, ok := byID[a.Parent]
		if !ok || p.Task != parent || p.Dimension != dim {
			return out, Invalid
		}
		// The persistent allocation and consumed reservation must agree with B.
		var valid bool
		err = tx.QueryRow(ctx, `SELECT ($1::numeric=0 AND NOT EXISTS(SELECT 1 FROM delegation_allocations WHERE child_account_id=$2)) OR
 EXISTS(SELECT 1 FROM delegation_allocations d JOIN budget_reservations r ON r.reservation_id=d.reservation_id
 WHERE d.child_account_id=$2 AND d.parent_account_id=$3 AND d.child_task_id=$4 AND d.amount=$1::numeric
 AND r.account_id=$3 AND r.holder_task_id=$5 AND r.amount=d.amount AND r.state='RELEASED' AND r.release_reason='delegated' AND r.command_id=$6)
`, a.Limit, a.ID, p.ID, child, parent, command).Scan(&valid)
		if err != nil {
			return out, err
		}
		if !valid {
			return out, Invalid
		}
		var returned string
		err = tx.QueryRow(ctx, `SELECT ($1::numeric-$2::numeric)::text,$3::numeric >= $1::numeric`, a.Limit, a.Settled, p.Delegated).Scan(&returned, &valid)
		if err != nil {
			return out, err
		}
		if !valid {
			return out, Invalid
		}
		if _, err = tx.Exec(ctx, `UPDATE budget_accounts SET delegated=delegated-$2::numeric,settled=settled+$3::numeric WHERE account_id=$1`, p.ID, a.Limit, a.Settled); err != nil {
			return out, err
		}
		if _, err = tx.Exec(ctx, `UPDATE budget_accounts SET budget_limit=settled,closed_at=$2 WHERE account_id=$1`, a.ID, out.At); err != nil {
			return out, err
		}
		out.Dimensions = append(out.Dimensions, DimensionCloseout{dim, a.Limit, a.Settled, returned})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `UPDATE task_delegations SET closed_at=$2,close_result=$3::jsonb WHERE child_task_id=$1`, child, out.At, string(raw)); err != nil {
		return out, err
	}
	if err = event(ctx, tx, child, "SUBTASK_BUDGET_CLOSED", out); err != nil {
		return out, err
	}
	if err = event(ctx, tx, parent, "CHILD_BUDGET_SETTLED", out); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox(task_id,event_type,payload,dedup_key) VALUES($1,'CHILD_BUDGET_SETTLED',$2::jsonb,'closeout:'||$3::text)`, parent, string(raw), child); err != nil {
		return out, err
	}
	if err = audit(ctx, tx, s.cfg.Domain, child, "", "budget.child_closed", out); err != nil {
		return out, err
	}
	if err = s.identity(ctx, tx, c); err != nil {
		return SubtaskCloseout{}, err
	}
	if err = commit(ctx, tx); err != nil {
		return SubtaskCloseout{}, err
	}
	return out, nil
}
