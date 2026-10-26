package archive

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maintenanceLock int64 = 7416217834219
const jobColumns = `job_id::text,parent_table,partition_name,partition_oid::bigint,range_start,range_end,store_id,state,COALESCE(manifest_key,''),COALESCE(manifest_digest,''),manifest_version`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Parent, &j.Partition, &j.OID, &j.Start, &j.End, &j.Store, &j.State, &j.ManifestKey, &j.ManifestDigest, &j.ManifestVersion)
	j.Start, j.End = j.Start.UTC(), j.End.UTC()
	return j, err
}
func (s *Service) lock(ctx context.Context) (*pgxpool.Conn, error) {
	c, err := s.db.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var locked bool
	if err = c.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", maintenanceLock).Scan(&locked); err != nil || !locked {
		c.Release()
		if err != nil {
			return nil, err
		}
		return nil, ErrBusy
	}
	return c, nil
}
func unlock(c *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Never return a connection with a surviving session lock to the pool.
	if _, err := c.Exec(ctx, "SELECT pg_advisory_unlock($1)", maintenanceLock); err != nil {
		_ = c.Conn().Close(ctx)
	}
	c.Release()
}

// Step persists one partition transition/chunk. Session locking excludes another
// archiver during network I/O and is released by PostgreSQL on process death.
func (s *Service) Step(ctx context.Context) (bool, error) {
	c, err := s.lock(ctx)
	if err != nil {
		return false, err
	}
	defer unlock(c)
	j, err := scanJob(c.QueryRow(ctx, "SELECT "+jobColumns+" FROM archive_jobs WHERE state<>'COMPLETE' ORDER BY detached_at,job_id LIMIT 1"))
	if errors.Is(err, pgx.ErrNoRows) {
		return s.detach(ctx, c)
	}
	if err != nil {
		return false, err
	}
	if j.Store != s.objects.ID() {
		return false, errors.New("unfinished archive belongs to a different object store")
	}
	if j.State == "DETACHED" {
		err = s.export(ctx, c, j)
	} else {
		err = s.finish(ctx, c, j)
	}
	return true, err
}

const candidates = `SELECT child.relname,child.oid::bigint,parent.relname,bounds[1]::timestamptz,bounds[2]::timestamptz
 FROM pg_inherits i JOIN pg_class child ON child.oid=i.inhrelid JOIN pg_class parent ON parent.oid=i.inhparent
 JOIN pg_namespace n ON n.oid=child.relnamespace JOIN pg_namespace p ON p.oid=parent.relnamespace
 CROSS JOIN LATERAL regexp_match(pg_get_expr(child.relpartbound,child.oid), '^FOR VALUES FROM \(''([^'']+)''\) TO \(''([^'']+)''\)$') bounds
 WHERE n.nspname='public' AND p.nspname='public' AND parent.relname IN ('task_events','audit_log')
 AND bounds[2]::timestamptz <= clock_timestamp()-CASE parent.relname WHEN 'task_events' THEN interval '180 days' ELSE interval '365 days' END
 ORDER BY bounds[2]::timestamptz,parent.relname LIMIT 1`

func (s *Service) detach(ctx context.Context, c *pgxpool.Conn) (bool, error) {
	tx, err := c.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SET LOCAL timezone='UTC'"); err != nil {
		return false, err
	}
	var name, parent string
	var oid int64
	var start, end time.Time
	err = tx.QueryRow(ctx, candidates).Scan(&name, &oid, &parent, &start, &end)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	start, end = start.UTC(), end.UTC()
	if name != parent+"_"+start.Format("2006_01") || start.Day() != 1 || start.Hour() != 0 || start.Minute() != 0 || start.Second() != 0 || start.Nanosecond() != 0 || !start.AddDate(0, 1, 0).Equal(end) {
		return false, errors.New("unsupported partition bounds/name; source retained")
	}
	parentSQL := pgx.Identifier{"public", parent}.Sanitize()
	childSQL := pgx.Identifier{"public", name}.Sanitize()
	if _, err = tx.Exec(ctx, "LOCK TABLE ONLY "+parentSQL+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, "LOCK TABLE "+childSQL+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return false, err
	}
	var same bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid WHERE i.inhparent=$1::regclass AND c.oid=$2::oid AND c.relname=$3)`, parentSQL, oid, name).Scan(&same)
	if err != nil {
		return false, err
	}
	if !same {
		return false, errors.New("partition changed during maintenance lock wait")
	}
	for _, q := range []string{
		"CREATE TRIGGER archive_frozen_rows BEFORE INSERT OR UPDATE OR DELETE ON " + childSQL + " FOR EACH ROW EXECUTE FUNCTION anet_archive_frozen()",
		"CREATE TRIGGER archive_frozen_truncate BEFORE TRUNCATE ON " + childSQL + " FOR EACH STATEMENT EXECUTE FUNCTION anet_archive_frozen()",
		"ALTER TABLE " + parentSQL + " DETACH PARTITION " + childSQL,
	} {
		if _, err = tx.Exec(ctx, q); err != nil {
			return false, err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO archive_jobs(parent_table,partition_name,partition_oid,range_start,range_end,store_id,state) VALUES($1,$2,$3,$4,$5,$6,'DETACHED')`, parent, name, oid, start, end, s.objects.ID())
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Cleanup never deletes pending/dead delivery, delivery inboxes, gateway bindings,
// effects, receipts, business records or artifact data. It uses server time only.
func (s *Service) Cleanup(ctx context.Context) (int64, error) {
	c, err := s.lock(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock(c)
	tx, err := c.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var total int64
	for _, q := range []string{
		`WITH expired AS (SELECT command_id,audience_domain_id FROM command_inbox WHERE expires_at<clock_timestamp() AND not_after<clock_timestamp() AND received_at<clock_timestamp()-interval '168 hours' ORDER BY expires_at LIMIT 1000 FOR UPDATE SKIP LOCKED)
 DELETE FROM command_inbox i USING expired x WHERE i.command_id=x.command_id AND i.audience_domain_id=x.audience_domain_id`,
		`WITH expired AS (SELECT o.outbox_seq FROM outbox o WHERE o.status='DONE' AND o.dispatched_at<clock_timestamp()-interval '168 hours' AND NOT EXISTS(SELECT 1 FROM delivery_dead_letters d WHERE d.outbox_seq=o.outbox_seq) ORDER BY o.dispatched_at LIMIT 1000 FOR UPDATE SKIP LOCKED)
 DELETE FROM outbox o USING expired x WHERE o.outbox_seq=x.outbox_seq`,
	} {
		tag, e := tx.Exec(ctx, q)
		if e != nil {
			return 0, e
		}
		total += tag.RowsAffected()
	}
	return total, tx.Commit(ctx)
}
func (s *Service) RunOnce(ctx context.Context) error {
	// Existing month-boundary creation is deliberately run in UTC.
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SET LOCAL timezone='UTC'; SELECT anet_ensure_partitions()"); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	// Drain bounded transactions before archival: a storage outage must not
	// starve independent safe expiry cleanup.
	for {
		deleted, err := s.Cleanup(ctx)
		if err != nil {
			return err
		}
		if deleted == 0 {
			break
		}
	}
	for {
		worked, err := s.Step(ctx)
		if err != nil {
			return fmt.Errorf("archive step: %w", err)
		}
		if !worked {
			return nil
		}
	}
}
