package archive

import (
	"context"
	"errors"
	"io"
	"sort"
	"time"

	"github.com/parquet-go/parquet-go"
)

// ReadTask is an operator lookup, not an authorization boundary. It includes hot,
// detached and cold rows without a detach/drop visibility gap. Callers exposing
// it to users must authenticate and authorize the domain/task themselves.
func (s *Service) ReadTask(ctx context.Context, domain, task string, limit int) ([]Record, error) {
	if domain == "" || task == "" || limit < 1 || limit > 100000 {
		return nil, errors.New("invalid archive lookup")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SET LOCAL timezone='UTC'"); err != nil {
		return nil, err
	}
	var exists bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM tasks WHERE task_id=$1 AND domain_id=$2)", task, domain).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("task outside requested domain")
	}
	if _, err = tx.Exec(ctx, "LOCK TABLE ONLY public.audit_log, ONLY public.task_events IN ACCESS SHARE MODE"); err != nil {
		return nil, err
	}
	result := []Record{}
	read := func(j Job) error {
		q, err := sourceSQL(j)
		if err != nil {
			return err
		}
		domainColumn := "e.domain_id"
		if j.Parent == "task_events" {
			domainColumn = "t.domain_id"
		}
		rows, err := tx.Query(ctx, q+" WHERE "+domainColumn+"=$1 AND e.task_id=$2 LIMIT $3", domain, task, limit+1-len(result))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Record
			var at time.Time
			if err = rows.Scan(&at, &r.Seq, &r.Domain, &r.Task, &r.JSON); err != nil {
				return err
			}
			r.AtMicros = at.UnixMicro()
			r.Table = j.Parent
			result = append(result, r)
		}
		if len(result) > limit {
			return errors.New("archive lookup exceeds record limit")
		}
		return rows.Err()
	}
	for _, parent := range []string{"audit_log", "task_events"} {
		if err = read(Job{Parent: parent, Partition: parent}); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, "SELECT "+jobColumns+" FROM archive_jobs WHERE state<>'COMPLETE' ORDER BY job_id FOR SHARE")
	if err != nil {
		return nil, err
	}
	jobs := []Job{}
	for rows.Next() {
		j, e := scanJob(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if err = read(j); err != nil {
			return nil, err
		}
	}
	rows, err = tx.Query(ctx, `SELECT c.ordinal,c.object_key,c.object_version,c.digest,c.size_bytes,c.row_count,c.last_at,c.last_seq,j.store_id FROM archive_task_index i JOIN archive_chunks c USING(job_id,ordinal) JOIN archive_jobs j USING(job_id) WHERE i.domain_id=$1 AND i.task_id=$2 AND j.state='COMPLETE' ORDER BY j.job_id,c.ordinal`, domain, task)
	if err != nil {
		return nil, err
	}
	chunks := []Chunk{}
	for rows.Next() {
		var c Chunk
		var store string
		if err = rows.Scan(&c.Ordinal, &c.Key, &c.Version, &c.Digest, &c.Size, &c.Rows, &c.LastAt, &c.LastSeq, &store); err != nil {
			rows.Close()
			return nil, err
		}
		if store != s.objects.ID() {
			rows.Close()
			return nil, errors.New("archive lookup requires a different object store")
		}
		chunks = append(chunks, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	for _, ch := range chunks {
		f, _, err := s.fetch(ctx, ch.Key, ch.Version, ch.Digest, ch.Size)
		if err != nil {
			return nil, err
		}
		r := parquet.NewGenericReader[Record](f)
		buf := make([]Record, 64)
		for {
			n, e := r.Read(buf)
			for _, record := range buf[:n] {
				if record.Domain == domain && record.Task == task {
					result = append(result, record)
				}
			}
			if len(result) > limit {
				e = errors.New("archive lookup exceeds record limit")
			}
			if e != nil {
				r.Close()
				removeTemp(f)
				if !errors.Is(e, io.EOF) {
					return nil, e
				}
				break
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].AtMicros == result[j].AtMicros {
			if result[i].Seq == result[j].Seq {
				return result[i].Table < result[j].Table
			}
			return result[i].Seq < result[j].Seq
		}
		return result[i].AtMicros < result[j].AtMicros
	})
	return result, nil
}
