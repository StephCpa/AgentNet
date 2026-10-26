// Package local implements the compiled record.put/v1 local transaction handler.
package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/internal/localtx"
	"github.com/jackc/pgx/v5"
)

type Records struct{}

func (Records) Validate(action, resource string, dimensions []string) error {
	if action != "record.put" || !strings.HasPrefix(resource, "records:") || len(resource) <= len("records:") || len(dimensions) != 4 {
		return localtx.Invalid
	}
	seen := map[string]bool{}
	for _, d := range dimensions {
		seen[d] = true
	}
	for _, d := range []string{"money", "tokens", "calls", "messages"} {
		if !seen[d] {
			return localtx.Invalid
		}
	}
	return nil
}

func (Records) Execute(ctx context.Context, db localtx.DB, r localtx.Request) (localtx.Result, error) {
	var p struct {
		Key             string          `json:"key"`
		ExpectedVersion *int64          `json:"expected_version"`
		Value           json.RawMessage `json:"value"`
	}
	d := json.NewDecoder(bytes.NewReader(r.Parameters))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || p.Key == "" || len(p.Key) > 256 || strings.ContainsRune(p.Key, 0) || p.ExpectedVersion == nil || *p.ExpectedVersion < 0 || *p.ExpectedVersion >= 9007199254740991 || len(p.Value) == 0 {
		return localtx.Result{}, localtx.Invalid
	}
	// Serialize both absent and existing records. JSON tuple framing avoids
	// ambiguous concatenation; hash collisions only cause extra serialization.
	lockKey, _ := json.Marshal([]string{"anet.record.put/v1", r.Domain, r.Resource, p.Key})
	if _, err := db.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", string(lockKey)); err != nil {
		return localtx.Result{}, err
	}
	var version int64
	err := db.QueryRow(ctx, "SELECT version FROM local_records WHERE domain_id=$1 AND resource=$2 AND record_key=$3 FOR UPDATE", r.Domain, r.Resource, p.Key).Scan(&version)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return localtx.Result{}, err
	}
	result := localtx.Result{Outcome: "REJECTED", Usage: map[string]string{"money": "0", "tokens": "0", "calls": "1", "messages": "0"}}
	if version != *p.ExpectedVersion {
		result.Payload, _ = json.Marshal(map[string]any{"reason": "version_conflict", "current_version": version})
		return result, nil
	}
	_, err = db.Exec(ctx, `INSERT INTO local_records(domain_id,resource,record_key,version,value,last_effect_id) VALUES($1,$2,$3,1,$4::jsonb,$5)
 ON CONFLICT(domain_id,resource,record_key) DO UPDATE SET version=local_records.version+1,value=EXCLUDED.value,last_effect_id=EXCLUDED.last_effect_id`, r.Domain, r.Resource, p.Key, string(p.Value), r.EffectID)
	if err != nil {
		return localtx.Result{}, err
	}
	digest, err := canonical.Digest(p.Value)
	if err != nil {
		return localtx.Result{}, err
	}
	result.Outcome = "COMMITTED"
	result.Payload, _ = json.Marshal(map[string]any{"key": p.Key, "version": version + 1, "value_digest": digest})
	return result, nil
}
