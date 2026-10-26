// Package mockbiz is a faultlab business system, not a production payment API.
// Its independent transactions make external commit observable across adapter kills.
package mockbiz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/services/execution"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ddl = `CREATE SCHEMA IF NOT EXISTS mockbiz;
CREATE TABLE IF NOT EXISTS mockbiz.calls(seq BIGSERIAL PRIMARY KEY,namespace TEXT NOT NULL,effect_id TEXT NOT NULL,attempt_id TEXT NOT NULL,params_digest TEXT NOT NULL,usage JSONB NOT NULL);
CREATE TABLE IF NOT EXISTS mockbiz.query_modes(namespace TEXT NOT NULL,effect_id TEXT NOT NULL,mode TEXT NOT NULL,PRIMARY KEY(namespace,effect_id));
CREATE TABLE IF NOT EXISTS mockbiz.tombstones(namespace TEXT NOT NULL,attempt_id TEXT NOT NULL,PRIMARY KEY(namespace,attempt_id));
CREATE TABLE IF NOT EXISTS mockbiz.key_tombstones(namespace TEXT NOT NULL,effect_id TEXT NOT NULL,params_digest TEXT NOT NULL,PRIMARY KEY(namespace,effect_id));`

func Install(ctx context.Context, db *pgxpool.Pool) error { _, err := db.Exec(ctx, ddl); return err }

type Server struct {
	DB                    *pgxpool.Pool
	Namespace, Capability string
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || (r.URL.Path != "/send" && r.URL.Path != "/query") {
		http.NotFound(w, r)
		return
	}
	var req execution.Request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	o, err := s.call(r.Context(), req, r.URL.Path == "/send")
	if err != nil {
		http.Error(w, "business failure", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(o)
}
func (s *Server) call(ctx context.Context, r execution.Request, send bool) (execution.Observation, error) {
	o := execution.Observation{Status: execution.Unknown, EffectID: r.EffectID, AttemptID: r.AttemptID, ParamsDigest: r.ParamsDigest, Evidence: "mock:" + s.Namespace + ":" + r.EffectID}
	if s.DB == nil || s.Namespace == "" || r.EffectID == "" {
		return o, execution.ErrInvalid
	}
	if !send && s.Capability == "opaque" {
		return o, nil
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return o, err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "mockbiz:"+s.Namespace+":"+r.EffectID)
	if err != nil {
		return o, err
	}
	var seq int64
	var pd string
	var usage []byte
	found := tx.QueryRow(ctx, `SELECT seq,params_digest,usage FROM mockbiz.calls WHERE namespace=$1 AND effect_id=$2 ORDER BY seq LIMIT 1`, s.Namespace, r.EffectID).Scan(&seq, &pd, &usage)
	if found != nil && found != pgx.ErrNoRows {
		return o, found
	}
	if found == nil && (!send || s.Capability == "idempotent-key") {
		if pd != r.ParamsDigest {
			return o, execution.ErrInvalid
		}
		o.Status = execution.Committed
		o.Evidence = fmt.Sprintf("mock:%s:receipt:%d", s.Namespace, seq)
		if err = json.Unmarshal(usage, &o.Usage); err != nil {
			return o, err
		}
		return o, tx.Commit(ctx)
	}
	if !send {
		if s.Capability == "opaque" {
			return o, tx.Commit(ctx)
		}
		mode := execution.Absent
		err = tx.QueryRow(ctx, `SELECT mode FROM mockbiz.query_modes WHERE namespace=$1 AND effect_id=$2`, s.Namespace, r.EffectID).Scan(&mode)
		if err != nil && err != pgx.ErrNoRows {
			return o, err
		}
		if mode == execution.Final || mode == "NOT_EXECUTED_FINAL_KEY" {
			if r.AttemptID == "" {
				return o, execution.ErrInvalid
			}
			_, err = tx.Exec(ctx, `INSERT INTO mockbiz.tombstones(namespace,attempt_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, s.Namespace, r.AttemptID)
			if err != nil {
				return o, err
			}
			o.FinalScope = "attempt"
			if mode == "NOT_EXECUTED_FINAL_KEY" {
				_, err = tx.Exec(ctx, `INSERT INTO mockbiz.key_tombstones(namespace,effect_id,params_digest) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, s.Namespace, r.EffectID, r.ParamsDigest)
				if err != nil {
					return o, err
				}
				o.FinalScope = "effect"
				mode = execution.Final
			}
			o.Usage = map[string]string{"money": "0", "tokens": "0", "calls": "0", "messages": "0"}
		}
		o.Status = mode
		return o, tx.Commit(ctx)
	}
	if r.AttemptID == "" {
		return o, execution.ErrInvalid
	}
	digest, err := canonical.Digest(r.Parameters)
	if err != nil || digest != r.ParamsDigest {
		return o, execution.ErrInvalid
	}
	var tombstone bool
	var keyTombstone bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mockbiz.key_tombstones WHERE namespace=$1 AND effect_id=$2)`, s.Namespace, r.EffectID).Scan(&keyTombstone)
	if err != nil {
		return o, err
	}
	if keyTombstone {
		o.Status = execution.Final
		o.FinalScope = "effect"
		o.Usage = map[string]string{"money": "0", "tokens": "0", "calls": "0", "messages": "0"}
		return o, tx.Commit(ctx)
	}
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mockbiz.tombstones WHERE namespace=$1 AND attempt_id=$2)`, s.Namespace, r.AttemptID).Scan(&tombstone)
	if err != nil {
		return o, err
	}
	if tombstone {
		o.Status = execution.Final
		o.FinalScope = "attempt"
		o.Usage = map[string]string{"money": "0", "tokens": "0", "calls": "0", "messages": "0"}
		return o, tx.Commit(ctx)
	}
	if s.Capability == "idempotent-key" {
		if r.KeyExpiresAt == nil {
			return o, execution.ErrInvalid
		}
		var live bool
		if err = tx.QueryRow(ctx, `SELECT $1::timestamptz>clock_timestamp()`, r.KeyExpiresAt).Scan(&live); err != nil {
			return o, err
		}
		if !live {
			return o, tx.Commit(ctx)
		}
	}
	// Queryable and opaque deliberately do NOT deduplicate Sends. If the adapter
	// repeats one, the ledger will expose two business effects rather than hide it.
	o.Usage = map[string]string{"money": "2", "tokens": "2", "calls": "2", "messages": "2"}
	usage, _ = json.Marshal(o.Usage)
	err = tx.QueryRow(ctx, `INSERT INTO mockbiz.calls(namespace,effect_id,attempt_id,params_digest,usage) VALUES($1,$2,$3,$4,$5::jsonb) RETURNING seq`, s.Namespace, r.EffectID, r.AttemptID, r.ParamsDigest, string(usage)).Scan(&seq)
	if err != nil {
		return o, err
	}
	o.Status = execution.Committed
	o.Evidence = fmt.Sprintf("mock:%s:receipt:%d", s.Namespace, seq)
	return o, tx.Commit(ctx)
}
