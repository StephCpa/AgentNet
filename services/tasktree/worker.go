// Package tasktree applies local cancellation intents transactionally, one task
// per work item. It never infers business outcomes or releases budget holds.
package tasktree

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Worker struct {
	db     *pgxpool.Pool
	domain string
}

func New(db *pgxpool.Pool, domain string) (*Worker, error) {
	if db == nil || domain == "" {
		return nil, errors.New("tasktree requires database and domain")
	}
	return &Worker{db, domain}, nil
}
func (w *Worker) Once(ctx context.Context) (bool, error) {
	tx, err := w.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var task, parent, origin string
	err = tx.QueryRow(ctx, `SELECT task_id,parent_task_id,origin_task_id FROM task_cancellations WHERE domain_id=$1 AND state='PENDING' ORDER BY created_at,task_id FOR UPDATE SKIP LOCKED LIMIT 1`, w.domain).Scan(&task, &parent, &origin)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var state, actualParent string
	var version int64
	err = tx.QueryRow(ctx, `SELECT status,COALESCE(parent_task_id,''),COALESCE(cancel_version,0) FROM tasks WHERE task_id=$1 AND domain_id=$2 AND peer_task_ref IS NULL AND (assignee_domain_id IS NULL OR assignee_domain_id=$2) FOR UPDATE`, task, w.domain).Scan(&state, &actualParent, &version)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return true, err
	}
	valid := err == nil && actualParent == parent
	if valid {
		// A queue row is not authority. Prove the current ancestry reaches a durable
		// cancelled origin. Bound recursion and reject cycles/foreign-domain links.
		err = tx.QueryRow(ctx, `WITH RECURSIVE path AS (
 SELECT task_id,parent_task_id,status,ARRAY[task_id] ids,0 depth FROM tasks WHERE task_id=$1 AND domain_id=$3
 UNION ALL SELECT t.task_id,t.parent_task_id,t.status,p.ids||t.task_id,p.depth+1 FROM path p JOIN tasks t ON t.task_id=p.parent_task_id
 WHERE t.domain_id=$3 AND p.depth<64 AND NOT t.task_id=ANY(p.ids))
 SELECT EXISTS(SELECT 1 FROM path WHERE task_id=$2 AND status='CANCELLED')`, task, origin, w.domain).Scan(&valid)
		if err != nil {
			return true, err
		}
	}
	if !valid {
		if _, err = tx.Exec(ctx, `UPDATE task_cancellations SET state='INVALID',completed_at=clock_timestamp() WHERE task_id=$1`, task); err != nil {
			return true, err
		}
		return true, tx.Commit(ctx)
	}
	if state != "CANCELLED" && state != "SUCCEEDED" && state != "FAILED" {
		err = tx.QueryRow(ctx, `UPDATE tasks SET status='CANCELLED',cancelled_at=clock_timestamp(),cancel_version=COALESCE(cancel_version,0)+1,task_version=task_version+1 WHERE task_id=$1 RETURNING cancel_version`, task).Scan(&version)
		if err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO task_events(task_id,root_task_id,event_type,payload) SELECT task_id,root_task_id,'CANCELLED',jsonb_build_object('cancel_version',$2::bigint,'origin_task_id',$3::text) FROM tasks WHERE task_id=$1`, task, version, origin); err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO outbox(task_id,event_type,payload,dedup_key) VALUES($1,'CANCELLED',jsonb_build_object('cancel_version',$2::bigint,'origin_task_id',$3::text),'cancel:'||$1::text)`, task, version, origin); err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO audit_log(domain_id,task_id,action,decision) VALUES($1,$2,'task.cancel_propagated','allow')`, w.domain, task); err != nil {
			return true, err
		}
	}
	// Even a completed intermediary can have descendants awaiting cancellation.
	if _, err = tx.Exec(ctx, `INSERT INTO task_cancellations(task_id,parent_task_id,origin_task_id,domain_id) SELECT task_id,parent_task_id,$2,$3 FROM tasks WHERE parent_task_id=$1 AND domain_id=$3 AND peer_task_ref IS NULL AND (assignee_domain_id IS NULL OR assignee_domain_id=$3) ON CONFLICT(task_id) DO UPDATE SET domain_id=EXCLUDED.domain_id,parent_task_id=EXCLUDED.parent_task_id,origin_task_id=EXCLUDED.origin_task_id,state='PENDING',completed_at=NULL WHERE task_cancellations.state='INVALID' `, task, origin, w.domain); err != nil {
		return true, err
	}
	if _, err = tx.Exec(ctx, `UPDATE task_cancellations SET state='DONE',completed_at=clock_timestamp() WHERE task_id=$1`, task); err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}
func (w *Worker) Run(ctx context.Context) error {
	for {
		work, err := w.Once(ctx)
		if err != nil {
			return err
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
