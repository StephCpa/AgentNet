package execution

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"time"

	"github.com/agentnet/agentnet/services/tcs"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalid = errors.New("invalid execution configuration or evidence")
	ErrBusy    = errors.New("execution is leased or not due")
	ErrFenced  = errors.New("execution claim lost or send authority invalid")
)

const (
	Committed = "COMMITTED"
	Pending   = "PENDING"
	Absent    = "ABSENT"
	Final     = "NOT_EXECUTED_FINAL"
	Unknown   = "UNKNOWN"
)

type Request struct {
	EffectID     string          `json:"effect_id"`
	AttemptID    string          `json:"attempt_id"`
	ParamsDigest string          `json:"params_digest"`
	Parameters   json.RawMessage `json:"parameters,omitempty"`
	KeyExpiresAt *time.Time      `json:"key_expires_at,omitempty"`
}
type Observation struct {
	Status       string            `json:"status"`
	EffectID     string            `json:"effect_id"`
	AttemptID    string            `json:"attempt_id"`
	ParamsDigest string            `json:"params_digest"`
	Evidence     string            `json:"evidence"`
	Usage        map[string]string `json:"usage,omitempty"`
	FinalScope   string            `json:"final_scope,omitempty"` // attempt or effect; effect fences every request for this key
}

// Targets are trusted deployment bindings. Responses must come from the verified
// target, never a worker-supplied endpoint. Final proves this exact attempt cannot
// later execute (including delayed sends); ABSENT provides no such guarantee.
type Target interface {
	Send(context.Context, Request) (Observation, error)
	Query(context.Context, Request) (Observation, error)
}
type Binding struct {
	Capability string
	Target     Target
}
type Resolver interface {
	Resolve(context.Context, tcs.Envelope) (tcs.Effect, error)
}
type Config struct {
	Domain, ResultKeyID   string
	ResultKey             ed25519.PrivateKey
	Targets               map[string]Binding
	Payloads              PayloadSource
	Resolver              Resolver
	Lease, RecoveryWindow time.Duration
	Backoff               []time.Duration
}
type Engine struct {
	db  *pgxpool.Pool
	cfg Config
}

func New(db *pgxpool.Pool, c Config) (*Engine, error) {
	if db == nil || c.Domain == "" || c.ResultKeyID == "" || len(c.ResultKey) != ed25519.PrivateKeySize || c.Payloads == nil || c.Resolver == nil || len(c.Targets) == 0 {
		return nil, ErrInvalid
	}
	if c.Lease == 0 {
		c.Lease = 30 * time.Second
	}
	if c.RecoveryWindow == 0 {
		c.RecoveryWindow = 72 * time.Hour
	}
	if c.Lease < time.Millisecond || c.RecoveryWindow < time.Millisecond {
		return nil, ErrInvalid
	}
	if len(c.Backoff) == 0 {
		c.Backoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour}
	}
	c.Backoff = append([]time.Duration(nil), c.Backoff...)
	for _, d := range c.Backoff {
		if d < time.Millisecond {
			return nil, ErrInvalid
		}
	}
	c.ResultKey = append(ed25519.PrivateKey(nil), c.ResultKey...)
	bindings := map[string]Binding{}
	for name, b := range c.Targets {
		if name == "" || b.Target == nil || (b.Capability != "opaque" && b.Capability != "queryable" && b.Capability != "idempotent-key") {
			return nil, ErrInvalid
		}
		bindings[name] = b
	}
	c.Targets = bindings
	return &Engine{db, c}, nil
}

type job struct {
	EffectID, TaskID, Target, Capability, Digest, State, Token, Attempt string
	Epoch                                                               int64
	Sends, Reconciles, Retries                                          int
	Created                                                             time.Time
	KeyUntil                                                            *time.Time
	Saved                                                               *tcs.Envelope
}
