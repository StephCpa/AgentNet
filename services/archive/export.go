package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/parquet-go/parquet-go"
)

func hashFile(f *os.File) (string, int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), size, err
}
func (s *Service) temp() (*os.File, error) { return os.CreateTemp(s.cfg.WorkDir, "anet-archive-*") }
func removeTemp(f *os.File)                { name := f.Name(); _ = f.Close(); _ = os.Remove(name) }
func sourceSQL(j Job) (string, error) {
	child := pgx.Identifier{"public", j.Partition}.Sanitize()
	switch j.Parent {
	case "task_events":
		return `SELECT e.recorded_at,e.seq,COALESCE(t.domain_id,''),e.task_id,to_jsonb(e)::text FROM ` + child + ` e LEFT JOIN tasks t USING(task_id)`, nil
	case "audit_log":
		return `SELECT e.at,e.seq,e.domain_id,COALESCE(e.task_id,''),to_jsonb(e)::text FROM ` + child + ` e`, nil
	default:
		return "", errors.New("invalid archive parent")
	}
}
func sourceTime(j Job) string {
	if j.Parent == "task_events" {
		return "e.recorded_at"
	}
	return "e.at"
}
func sourceExists(ctx context.Context, tx interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, j Job) error {
	var oid int64
	if err := tx.QueryRow(ctx, "SELECT to_regclass($1)::oid::bigint", pgx.Identifier{"public", j.Partition}.Sanitize()).Scan(&oid); err != nil {
		return err
	}
	if oid != j.OID {
		return errors.New("archive source relation changed")
	}
	return nil
}

const chunkColumns = `ordinal,object_key,object_version,digest,size_bytes,row_count,last_at,last_seq`

func scanChunk(row pgx.Row) (Chunk, error) {
	var c Chunk
	err := row.Scan(&c.Ordinal, &c.Key, &c.Version, &c.Digest, &c.Size, &c.Rows, &c.LastAt, &c.LastSeq)
	c.LastAt = c.LastAt.UTC()
	return c, err
}
func loadChunks(ctx context.Context, c interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, id string) ([]Chunk, error) {
	rows, err := c.Query(ctx, "SELECT "+chunkColumns+" FROM archive_chunks WHERE job_id=$1 ORDER BY ordinal", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	chunks := []Chunk{}
	for rows.Next() {
		v, err := scanChunk(rows)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, v)
	}
	return chunks, rows.Err()
}

func (s *Service) export(ctx context.Context, c *pgxpool.Conn, j Job) error {
	if err := sourceExists(ctx, c, j); err != nil {
		return err
	}
	previous, err := scanChunk(c.QueryRow(ctx, "SELECT "+chunkColumns+" FROM archive_chunks WHERE job_id=$1 ORDER BY ordinal DESC LIMIT 1", j.ID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if previous.Ordinal == 0 {
		previous.LastAt = j.Start.Add(-time.Microsecond)
		previous.LastSeq = -1
	}
	query, err := sourceSQL(j)
	if err != nil {
		return err
	}
	at := sourceTime(j)
	readTx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer readTx.Rollback(ctx)
	if _, err = readTx.Exec(ctx, "SET LOCAL timezone='UTC'"); err != nil {
		return err
	}
	rows, err := readTx.Query(ctx, query+" WHERE ("+at+",e.seq)>($1,$2) ORDER BY "+at+",e.seq LIMIT $3", previous.LastAt, previous.LastSeq, s.cfg.ChunkRows)
	if err != nil {
		return err
	}
	defer rows.Close()
	f, err := s.temp()
	if err != nil {
		return err
	}
	defer removeTemp(f)
	w := parquet.NewGenericWriter[Record](f, parquet.MaxRowsPerRowGroup(512))
	ch := Chunk{Ordinal: previous.Ordinal + 1}
	var rawBytes int64
	index := map[[2]string]bool{}
	for rows.Next() {
		var r Record
		var at time.Time
		if err = rows.Scan(&at, &r.Seq, &r.Domain, &r.Task, &r.JSON); err != nil {
			return err
		}
		r.AtMicros = at.UnixMicro()
		r.Table = j.Parent
		size := int64(len(r.JSON) + len(r.Task) + len(r.Domain) + 64)
		if rawBytes+size > s.cfg.MaxChunkBytes {
			if ch.Rows == 0 {
				return errors.New("archive row exceeds configured chunk byte limit; source retained")
			}
			break
		}
		if _, err = w.Write([]Record{r}); err != nil {
			return err
		}
		rawBytes += size
		ch.Rows++
		ch.LastAt = at.UTC()
		ch.LastSeq = r.Seq
		index[[2]string{r.Domain, r.Task}] = true
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	if err = readTx.Commit(ctx); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	if ch.Rows == 0 {
		return s.publishManifest(ctx, c, j)
	}
	ch.Digest, ch.Size, err = hashFile(f)
	if err != nil {
		return err
	}
	ch.Key = fmt.Sprintf("%s/chunk-%08d-%s.parquet", j.ID, ch.Ordinal, ch.Digest[len("sha256:"):])
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	ch.Version, err = s.objects.Put(ctx, ch.Key, f, ch.Size)
	if err != nil {
		return err
	}
	ch.Version, err = s.verifyChunk(ctx, ch)
	if err != nil {
		return err
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO archive_chunks(job_id,ordinal,object_key,object_version,digest,size_bytes,row_count,last_at,last_seq) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, j.ID, ch.Ordinal, ch.Key, ch.Version, ch.Digest, ch.Size, ch.Rows, ch.LastAt, ch.LastSeq)
	if err != nil {
		return err
	}
	for pair := range index {
		if _, err = tx.Exec(ctx, "INSERT INTO archive_task_index(job_id,ordinal,domain_id,task_id) VALUES($1,$2,$3,$4)", j.ID, ch.Ordinal, pair[0], pair[1]); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Service) fetch(ctx context.Context, key, version, digest string, size int64) (*os.File, string, error) {
	o, err := s.objects.Open(ctx, key, version)
	if err != nil {
		return nil, "", err
	}
	defer o.Body.Close()
	f, err := s.temp()
	if err != nil {
		return nil, "", err
	}
	n, err := io.Copy(f, io.LimitReader(o.Body, size+1))
	if err != nil {
		removeTemp(f)
		return nil, "", err
	}
	got, _, err := hashFile(f)
	if err != nil || n != size || got != digest {
		removeTemp(f)
		return nil, "", errors.New("archive object digest/size mismatch; source retained")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		removeTemp(f)
		return nil, "", err
	}
	return f, o.Version, nil
}
func (s *Service) verifyChunk(ctx context.Context, ch Chunk) (string, error) {
	f, version, err := s.fetch(ctx, ch.Key, ch.Version, ch.Digest, ch.Size)
	if err != nil {
		return "", err
	}
	defer removeTemp(f)
	r := parquet.NewGenericReader[Record](f)
	defer r.Close()
	buf := make([]Record, 64)
	var count int64
	var last Record
	for {
		n, err := r.Read(buf)
		if n > 0 {
			count += int64(n)
			last = buf[n-1]
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
	}
	if count != ch.Rows || last.AtMicros != ch.LastAt.UnixMicro() || last.Seq != ch.LastSeq {
		return "", errors.New("archive Parquet rows do not match checkpoint")
	}
	return version, nil
}

func (s *Service) publishManifest(ctx context.Context, c *pgxpool.Conn, j Job) error {
	chunks, err := loadChunks(ctx, c, j.ID)
	if err != nil {
		return err
	}
	m := Manifest{Format: "anet.partition-archive/1", Job: j, Chunks: chunks}
	for _, ch := range chunks {
		m.Rows += ch.Rows
	}
	f, err := s.temp()
	if err != nil {
		return err
	}
	defer removeTemp(f)
	if err = json.NewEncoder(f).Encode(m); err != nil {
		return err
	}
	digest, size, err := hashFile(f)
	if err != nil {
		return err
	}
	key := j.ID + "/manifest-" + digest[len("sha256:"):] + ".json"
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	version, err := s.objects.Put(ctx, key, f, size)
	if err != nil {
		return err
	}
	remote, version, err := s.fetch(ctx, key, version, digest, size)
	if err != nil {
		return err
	}
	removeTemp(remote)
	_, err = c.Exec(ctx, "UPDATE archive_jobs SET state='EXPORTED',manifest_key=$2,manifest_digest=$3,manifest_version=$4 WHERE job_id=$1 AND state='DETACHED'", j.ID, key, digest, version)
	return err
}

func (s *Service) finish(ctx context.Context, c *pgxpool.Conn, j Job) error {
	chunks, err := loadChunks(ctx, c, j.ID)
	if err != nil {
		return err
	}
	m := Manifest{Format: "anet.partition-archive/1", Job: j, Chunks: chunks}
	for _, ch := range chunks {
		m.Rows += ch.Rows
	}
	// Reconstruct the exact immutable manifest without relying on untrusted object metadata.
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	h := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(h[:]) != j.ManifestDigest {
		return errors.New("archive manifest catalog mismatch")
	}
	f, _, err := s.fetch(ctx, j.ManifestKey, j.ManifestVersion, j.ManifestDigest, int64(len(raw)))
	if err != nil {
		return err
	}
	removeTemp(f)
	for _, ch := range chunks {
		if _, err = s.verifyChunk(ctx, ch); err != nil {
			return err
		}
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state string
	if err = tx.QueryRow(ctx, "SELECT state FROM archive_jobs WHERE job_id=$1 FOR UPDATE", j.ID).Scan(&state); err != nil {
		return err
	}
	if state != "EXPORTED" {
		return errors.New("archive state changed")
	}
	child := pgx.Identifier{"public", j.Partition}.Sanitize()
	if _, err = tx.Exec(ctx, "LOCK TABLE "+child+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return err
	}
	if err = sourceExists(ctx, tx, j); err != nil {
		return err
	}
	var count int64
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM "+child).Scan(&count); err != nil {
		return err
	}
	if count != m.Rows {
		return errors.New("archive source row count mismatch; source retained")
	}
	if _, err = tx.Exec(ctx, "DROP TABLE "+child); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE archive_jobs SET state='COMPLETE',completed_at=clock_timestamp() WHERE job_id=$1", j.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
