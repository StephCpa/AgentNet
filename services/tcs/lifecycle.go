package tcs

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

type Snapshot struct {
	ID, Status              string
	Version, Epoch          int64
	Cancelled, BudgetFrozen bool
}

func (s *Store) GetTask(ctx context.Context, c Caller, taskID string) (Snapshot, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback(ctx)
	t, err := s.lockTask(ctx, tx, c, taskID)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{t.ID, t.Status, t.Version, t.Epoch, t.Cancelled, t.Frozen}, commit(ctx, tx)
}

func (s *Store) RenewLease(ctx context.Context, c Caller, taskID, leaseID string) (Lease, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Lease{}, err
	}
	defer tx.Rollback(ctx)
	t, err := s.lockTask(ctx, tx, c, taskID)
	if err != nil {
		return Lease{}, err
	}
	now, err := currentTime(ctx, tx)
	if err != nil {
		return Lease{}, err
	}
	if err = active(t, now); err != nil {
		return Lease{}, err
	}
	out := Lease{ID: leaseID, Epoch: t.Epoch, Version: t.Version}
	err = tx.QueryRow(ctx, `UPDATE leases SET expires_at=$5 WHERE lease_id=$1 AND task_id=$2 AND worker_instance_id=$3 AND epoch=$4 AND superseded_at IS NULL AND expires_at>=clock_timestamp() RETURNING expires_at`, leaseID, taskID, c.Instance, t.Epoch, now.Add(s.cfg.LeaseTTL)).Scan(&out.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Lease{}, LeaseInvalid
		}
		return Lease{}, err
	}
	if err = commit(ctx, tx); err != nil {
		return Lease{}, err
	}
	return out, nil
}

// ReleaseUnbound returns an unused reservation. A bound hold can only be settled
// by Resolve with signed terminal evidence, even after cancellation or soft expiry.
func (s *Store) ReleaseUnbound(ctx context.Context, c Caller, taskID, reservationID string) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = s.lockTask(ctx, tx, c, taskID); err != nil {
		return err
	}
	rows, err := lockReservations(ctx, tx, []string{reservationID})
	if err != nil {
		return err
	}
	h := rows[0]
	if h.Task != taskID || h.Effect != "" || h.Attempt != "" {
		return ReservationInvalid
	}
	if h.State == "RELEASED" {
		return commit(ctx, tx)
	}
	if h.State != "HELD" {
		return ReservationInvalid
	}
	_, err = tx.Exec(ctx, `UPDATE budget_reservations SET state='RELEASED',released_at=clock_timestamp(),release_reason='unbound' WHERE reservation_id=$1`, reservationID)
	if err != nil {
		return err
	}
	return commit(ctx, tx)
}
