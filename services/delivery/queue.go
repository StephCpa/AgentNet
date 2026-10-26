// Package delivery implements durable, at-least-once notification delivery.
// Publishers must durably deduplicate acceptance. They must NOT retry raw business
// operations: EFFECT_ADMITTED is a notification, not authority to repeat an effect.
package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrLeaseLost = errors.New("delivery lease lost")
	ErrConflict  = errors.New("delivery identity bound to different content")
	ErrInvalid   = errors.New("invalid delivery configuration or message")
)

type Message struct {
	SourceDomain string          `json:"source_domain"`
	Destination  string          `json:"destination"`
	Key          string          `json:"dedup_key"`
	TaskID       string          `json:"task_id"`
	Type         string          `json:"event_type"`
	Payload      json.RawMessage `json:"payload"`
}
type Claim struct {
	Message  Message
	Sequence int64
	Token    string
	Attempt  int
	Until    time.Time
}
type Config struct {
	Domain, Destination                      string
	Lease, BaseDelay, MaxDelay, PollInterval time.Duration
	MaxAttempts                              int
}
type Queue struct {
	db  *pgxpool.Pool
	cfg Config
}

func New(db *pgxpool.Pool, c Config) (*Queue, error) {
	if db == nil || c.Domain == "" || c.Destination == "" {
		return nil, ErrInvalid
	}
	if c.Lease == 0 {
		c.Lease = 30 * time.Second
	}
	if c.BaseDelay == 0 {
		c.BaseDelay = time.Second
	}
	if c.MaxDelay == 0 {
		c.MaxDelay = 5 * time.Minute
	}
	if c.PollInterval == 0 {
		c.PollInterval = 250 * time.Millisecond
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 16
	}
	if c.Lease < time.Millisecond || c.BaseDelay < time.Millisecond || c.MaxDelay < c.BaseDelay || c.PollInterval < time.Millisecond || c.MaxAttempts < 1 || c.MaxAttempts > 1000 {
		return nil, ErrInvalid
	}
	return &Queue{db, c}, nil
}
func (q *Queue) delay(attempt int) time.Duration {
	d := q.cfg.BaseDelay
	for i := 1; i < attempt; i++ {
		if d > q.cfg.MaxDelay/2 {
			return q.cfg.MaxDelay
		}
		d *= 2
	}
	return d
}

// Claim locks only an outbox row, skips other dispatchers, and commits before I/O.
// An expired attempt is counted even if the process died before sending anything.
// dead=true means an exhausted row and its durable alert were committed.
func (q *Queue) Claim(ctx context.Context) (c *Claim, dead bool, err error) {
	tx, err := q.db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	c = &Claim{}
	err = tx.QueryRow(ctx, `SELECT o.outbox_seq,o.task_id,o.event_type,o.payload,o.dedup_key,o.attempts
 FROM outbox o JOIN tasks t ON t.task_id=o.task_id
 WHERE t.domain_id=$1 AND o.destination=$2 AND
 ((o.status='PENDING' AND o.next_attempt_at<=clock_timestamp()) OR
  (o.status='IN_FLIGHT' AND o.lease_until<=clock_timestamp()))
 ORDER BY o.next_attempt_at,o.outbox_seq FOR UPDATE OF o SKIP LOCKED LIMIT 1`, q.cfg.Domain, q.cfg.Destination).
		Scan(&c.Sequence, &c.Message.TaskID, &c.Message.Type, &c.Message.Payload, &c.Message.Key, &c.Attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if c.Attempt >= q.cfg.MaxAttempts {
		if err = q.dead(ctx, tx, c.Sequence, c.Attempt, "attempts_exhausted"); err != nil {
			return nil, false, err
		}
		return nil, true, tx.Commit(ctx)
	}
	c.Message.SourceDomain = q.cfg.Domain
	c.Message.Destination = q.cfg.Destination
	err = tx.QueryRow(ctx, `UPDATE outbox SET status='IN_FLIGHT',attempts=attempts+1,
 lease_token=gen_random_uuid(),lease_until=clock_timestamp()+$2::bigint*interval '1 microsecond'
 WHERE outbox_seq=$1 RETURNING lease_token::text,lease_until,attempts`, c.Sequence, q.cfg.Lease.Microseconds()).Scan(&c.Token, &c.Until, &c.Attempt)
	if err != nil {
		return nil, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return c, false, nil
}
func (q *Queue) dead(ctx context.Context, tx pgx.Tx, seq int64, attempts int, reason string) error {
	_, err := tx.Exec(ctx, `UPDATE outbox SET status='DEAD',lease_token=NULL,lease_until=NULL,last_failure=$2 WHERE outbox_seq=$1`, seq, reason)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO delivery_dead_letters(outbox_seq,source_domain,destination,attempts,reason)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT(outbox_seq) DO NOTHING`, seq, q.cfg.Domain, q.cfg.Destination, attempts, reason)
	return err
}

// Finish fences both stale tokens and expired leases. A sender may still be in
// flight after its lease expires; only receiver deduplication prevents duplicates.
func (q *Queue) Finish(ctx context.Context, c Claim, publishErr error) error {
	tx, err := q.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var attempts int
	err = tx.QueryRow(ctx, `SELECT o.attempts FROM outbox o JOIN tasks t ON t.task_id=o.task_id
 WHERE o.outbox_seq=$1 AND o.status='IN_FLIGHT' AND o.lease_token::text=$2
 AND o.lease_until>clock_timestamp() AND t.domain_id=$3 AND o.destination=$4
 FOR UPDATE OF o`, c.Sequence, c.Token, q.cfg.Domain, q.cfg.Destination).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	// A lock wait may outlive the lease; inspect time again after acquiring it.
	var live bool
	if err = tx.QueryRow(ctx, `SELECT lease_until>clock_timestamp() FROM outbox WHERE outbox_seq=$1`, c.Sequence).Scan(&live); err != nil {
		return err
	}
	if !live {
		return ErrLeaseLost
	}
	if publishErr == nil {
		_, err = tx.Exec(ctx, `UPDATE outbox SET status='DONE',dispatched_at=clock_timestamp(),lease_token=NULL,lease_until=NULL,last_failure=NULL WHERE outbox_seq=$1`, c.Sequence)
	} else if attempts >= q.cfg.MaxAttempts || errors.Is(publishErr, ErrConflict) {
		reason := "attempts_exhausted"
		if errors.Is(publishErr, ErrConflict) {
			reason = "identity_conflict"
		}
		err = q.dead(ctx, tx, c.Sequence, attempts, reason)
	} else {
		_, err = tx.Exec(ctx, `UPDATE outbox SET status='PENDING',lease_token=NULL,lease_until=NULL,
 next_attempt_at=clock_timestamp()+$2::bigint*interval '1 microsecond',last_failure='publisher_error' WHERE outbox_seq=$1`, c.Sequence, q.delay(attempts).Microseconds())
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Publisher acknowledges only durable acceptance by a trusted receiver. It must
// honor cancellation. Its destination/authentication is deployment configuration.
type Publisher interface {
	Publish(context.Context, Message) error
}

func (q *Queue) Once(ctx context.Context, p Publisher) (bool, error) {
	if p == nil {
		return false, ErrInvalid
	}
	c, dead, err := q.Claim(ctx)
	if err != nil {
		return false, err
	}
	if c == nil {
		return dead, nil
	}
	// Local timeout bounds I/O; database time remains authoritative for fencing.
	sendCtx, cancel := context.WithTimeout(ctx, q.cfg.Lease)
	defer cancel()
	sendErr := p.Publish(sendCtx, c.Message)
	if err = q.Finish(ctx, *c, sendErr); err != nil {
		return true, err
	}
	return true, nil
}
func (q *Queue) Run(ctx context.Context, p Publisher) error {
	if p == nil {
		return ErrInvalid
	}
	for {
		work, err := q.Once(ctx, p)
		if err != nil && !errors.Is(err, ErrLeaseLost) {
			return fmt.Errorf("outbox: %w", err)
		}
		if work && err == nil {
			continue
		}
		timer := time.NewTimer(q.cfg.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
