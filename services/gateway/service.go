// Package gateway binds authenticated SDK intents to durable signed commands.
package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"time"

	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/internal/signed"
	"github.com/agentnet/agentnet/services/ics"
	"github.com/agentnet/agentnet/services/tcs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Intent struct {
	Kind            string            `json:"kind"`
	ID              string            `json:"command_id"`
	ContractID      string            `json:"contract_id"`
	ContractVersion int               `json:"contract_version"`
	Contract        json.RawMessage   `json:"contract"`
	Limits          map[string]string `json:"limits"`
	Root            *tcs.RootRequest  `json:"root"`
}
type Config struct {
	Domain, KeyID string
	Key           ed25519.PrivateKey
	CommandTTL    time.Duration
}
type Service struct {
	db     *pgxpool.Pool
	store  *tcs.Store
	policy *ics.Service
	cfg    Config
}

func New(db *pgxpool.Pool, store *tcs.Store, policy *ics.Service, c Config) (*Service, error) {
	if db == nil || store == nil || policy == nil || c.Domain == "" || c.KeyID == "" || len(c.Key) != ed25519.PrivateKeySize {
		return nil, tcs.Invalid
	}
	if c.CommandTTL == 0 {
		c.CommandTTL = 5 * time.Minute
	}
	if c.CommandTTL < time.Second || c.CommandTTL > 5*time.Minute {
		return nil, tcs.Invalid
	}
	c.Key = append(ed25519.PrivateKey(nil), c.Key...)
	return &Service{db, store, policy, c}, nil
}

func (s *Service) Submit(ctx context.Context, c tcs.Caller, raw []byte) (tcs.Admission, error) {
	command, permit, err := s.bind(ctx, c, raw)
	if err != nil {
		return tcs.Admission{}, err
	}
	// A stored permit preserves recovery of a previously committed T1 after its
	// reply was lost, even if consent was subsequently revoked. TCS still checks
	// current identity, exact command digest and the original command expiry.
	if permit.KeyID != "" {
		out, e := s.store.SubmitAuthorized(ctx, c, command, permit)
		if e == nil || !errors.Is(e, tcs.Denied) {
			return out, e
		}
		// No ACCEPTED match; an expired/stale permit must be reauthorized by ICS.
	}
	permit, err = s.policy.IssueRoot(ctx, c, command)
	if err != nil {
		return tcs.Admission{}, err
	}
	encoded, err := json.Marshal(permit)
	if err != nil {
		return tcs.Admission{}, err
	}
	// Persist before T1. A crash between these steps can retry the same command;
	// a permit alone does not create a task or a budget account.
	_, err = s.db.Exec(ctx, `UPDATE gateway_commands SET root_permit=$3::jsonb WHERE domain_id=$1 AND command_id=$2`, s.cfg.Domain, commandID(command), string(encoded))
	if err != nil {
		return tcs.Admission{}, err
	}
	return s.store.SubmitAuthorized(ctx, c, command, permit)
}
func commandID(e tcs.Envelope) string {
	var sub tcs.Submission
	_ = json.Unmarshal(e.Payload, &sub) // produced by bind or read from the trusted store
	return sub.Command.ID
}
func (s *Service) bind(ctx context.Context, c tcs.Caller, raw []byte) (tcs.Envelope, tcs.Envelope, error) {
	var zero tcs.Envelope
	if len(raw) == 0 || len(raw) > 1<<20 || c.CredentialHash == "" || c.CredentialExpiresAt == nil {
		return zero, zero, tcs.Invalid
	}
	canonicalRaw, err := canonical.Bytes(raw)
	if err != nil {
		return zero, zero, tcs.Invalid
	}
	var request Intent
	decoder := json.NewDecoder(bytes.NewReader(canonicalRaw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || request.Kind != "anet.intent/1" || request.ID == "" || len(request.ID) > 256 || request.ContractID == "" || len(request.ContractID) > 256 || request.ContractVersion < 1 || request.Root == nil {
		return zero, zero, tcs.Invalid
	}
	digest, err := canonical.Digest(canonicalRaw)
	if err != nil {
		return zero, zero, tcs.Invalid
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return zero, zero, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "anet.gateway:"+s.cfg.Domain+":"+request.ID); err != nil {
		return zero, zero, err
	}
	var live bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM principals p JOIN agents a ON a.owner_principal_id=p.principal_id JOIN agent_instances i ON i.agent_id=a.agent_id WHERE p.principal_id=$1 AND a.agent_id=$2 AND i.instance_id=$3 AND p.domain_id=$4 AND a.domain_id=$4 AND p.revoked_at IS NULL AND i.revoked_at IS NULL AND a.status='active' AND i.cred_hash=$5 AND clock_timestamp()<$6)`, c.Principal, c.Agent, c.Instance, s.cfg.Domain, c.CredentialHash, c.CredentialExpiresAt).Scan(&live)
	if err != nil {
		return zero, zero, err
	}
	if !live {
		return zero, zero, tcs.Denied
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return zero, zero, err
	}
	var oldDigest, principal, agent, instance string
	var encoded, root []byte
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT intent_digest,principal_id,agent_id,instance_id,signed_command,root_permit,not_after FROM gateway_commands WHERE domain_id=$1 AND command_id=$2`, s.cfg.Domain, request.ID).Scan(&oldDigest, &principal, &agent, &instance, &encoded, &root, &expires)
	if err == nil {
		if principal != c.Principal || agent != c.Agent || instance != c.Instance || oldDigest != digest {
			return zero, zero, tcs.Conflict
		}
		if !now.Before(expires) {
			return zero, zero, tcs.Expired
		}
		var command, permit tcs.Envelope
		if err = json.Unmarshal(encoded, &command); err != nil {
			return zero, zero, err
		}
		if len(root) > 0 {
			if err = json.Unmarshal(root, &permit); err != nil {
				return zero, zero, err
			}
		}
		return command, permit, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return zero, zero, err
	}
	cmd := tcs.Command{ID: request.ID, Audience: s.cfg.Domain, NotAfter: now.UTC().Add(s.cfg.CommandTTL), ContractID: request.ContractID, ContractVersion: request.ContractVersion, Contract: request.Contract, Limits: request.Limits, Root: request.Root}
	command, err := signed.Encode(s.cfg.KeyID, s.cfg.Key, tcs.Submission{Kind: "anet.command/1", Caller: c, Command: cmd})
	if err != nil {
		return zero, zero, err
	}
	encoded, err = json.Marshal(command)
	if err != nil {
		return zero, zero, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO gateway_commands(domain_id,command_id,principal_id,agent_id,instance_id,intent_digest,signed_command,not_after,created_at) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9)`, s.cfg.Domain, request.ID, c.Principal, c.Agent, c.Instance, digest, string(encoded), cmd.NotAfter, now)
	if err != nil {
		return zero, zero, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log(domain_id,principal_id,agent_instance_id,command_id,action,decision,detail_digest) VALUES($1,$2,$3,$4,'gw.command_bound','allow',$5)`, s.cfg.Domain, c.Principal, c.Instance, request.ID, digest)
	if err != nil {
		return zero, zero, err
	}
	return command, zero, tx.Commit(ctx)
}
