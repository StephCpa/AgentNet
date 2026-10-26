package delivery

import (
	"context"
	"encoding/json"

	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Inbox is a local PostgreSQL notification receiver. Domain and destination are
// trusted constructor inputs, NOT authorization derived from an incoming message.
// A future network receiver must authenticate the sender before using this API.
type Inbox struct {
	db                  *pgxpool.Pool
	domain, destination string
}

func NewInbox(db *pgxpool.Pool, domain, destination string) (*Inbox, error) {
	if db == nil || domain == "" || destination == "" {
		return nil, ErrInvalid
	}
	return &Inbox{db, domain, destination}, nil
}
func (i *Inbox) Publish(ctx context.Context, m Message) error {
	if m.SourceDomain != i.domain || m.Destination != i.destination || m.Key == "" || m.TaskID == "" || m.Type == "" || len(m.Payload) > 1<<20 {
		return ErrInvalid
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ErrInvalid
	}
	digest, err := canonical.Digest(b)
	if err != nil {
		return ErrInvalid
	}
	tx, err := i.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Verify that this is a notification about a task in the configured domain.
	var exists bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE task_id=$1 AND domain_id=$2)`, m.TaskID, i.domain).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return ErrInvalid
	}
	_, err = tx.Exec(ctx, `INSERT INTO delivery_inbox(source_domain,destination,dedup_key,task_id,event_type,payload,message_digest)
 VALUES($1,$2,$3,$4,$5,$6::jsonb,$7) ON CONFLICT(source_domain,destination,dedup_key) DO NOTHING`, i.domain, i.destination, m.Key, m.TaskID, m.Type, string(m.Payload), digest)
	if err != nil {
		return err
	}
	var prior string
	err = tx.QueryRow(ctx, `SELECT message_digest FROM delivery_inbox WHERE source_domain=$1 AND destination=$2 AND dedup_key=$3`, i.domain, i.destination, m.Key).Scan(&prior)
	if err != nil {
		return err
	}
	if prior != digest {
		return ErrConflict
	}
	if m.Type == "CANCELLED" {
		// Notifications are not authority: derive cancellation from the live local
		// task, then durably enqueue its actual children in the inbox transaction.
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE task_id=$1 AND domain_id=$2 AND status='CANCELLED' AND cancel_version IS NOT NULL)`, m.TaskID, i.domain).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrInvalid
		}
		_, err = tx.Exec(ctx, `INSERT INTO task_cancellations(task_id,parent_task_id,origin_task_id,domain_id)
 SELECT task_id,parent_task_id,$1,$2 FROM tasks WHERE parent_task_id=$1 AND domain_id=$2 AND peer_task_ref IS NULL AND (assignee_domain_id IS NULL OR assignee_domain_id=$2) ON CONFLICT(task_id) DO UPDATE SET domain_id=EXCLUDED.domain_id,parent_task_id=EXCLUDED.parent_task_id,origin_task_id=EXCLUDED.origin_task_id,state='PENDING',completed_at=NULL WHERE task_cancellations.state='INVALID' `, m.TaskID, i.domain)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
