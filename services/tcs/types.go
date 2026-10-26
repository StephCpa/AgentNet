// Package tcs implements the single-domain PostgreSQL transaction kernel.
// Caller identities must come from an authenticated transport, never request JSON.
package tcs

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	decimal "github.com/agentnet/agentnet/internal/amount"
	"github.com/agentnet/agentnet/internal/authority"
	"github.com/agentnet/agentnet/internal/signed"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Code string

func (c Code) Error() string { return string(c) }

const (
	Denied             Code = "ANET-AUT-DENIED"
	Invalid            Code = "ANET-CMD-INVALID"
	Expired            Code = "ANET-CMD-EXPIRED"
	Conflict           Code = "ANET-CMD-PARAMS-CONFLICT"
	EffectConflict     Code = "ANET-EFF-PARAMS-CONFLICT"
	Terminal           Code = "ANET-TSK-TERMINAL"
	Deadline           Code = "ANET-TSK-DEADLINE"
	VersionConflict    Code = "ANET-TSK-VERSION-CONFLICT"
	Fenced             Code = "ANET-EFF-FENCED"
	LeaseInvalid       Code = "ANET-EFF-LEASE-INVALID"
	BudgetExhausted    Code = "ANET-BUD-EXHAUSTED"
	ReservationInvalid Code = "ANET-BUD-RESERVATION-INVALID"
	DecisionMismatch   Code = "ANET-AUT-DECISION-MISMATCH"
	BudgetViolation    Code = "ANET-BUD-VIOLATION-RECORDED"
	ResultConflict     Code = "ANET-EFF-RESULT-CONFLICT"
)

// Caller is supplied by a trusted transport after authentication. The kernel
// also checks live principal/instance revocation and task ownership in the DB.
type Caller struct {
	Principal, Agent, Instance string
	// CredentialHash is supplied only by authenticated transport. It is never
	// accepted from signed/body identity and lets lock-wait rechecks fence rotation.
	CredentialHash string `json:"-"`
}

func (c Caller) SameIdentity(other Caller) bool {
	return c.Principal == other.Principal && c.Agent == other.Agent && c.Instance == other.Instance
}

type Operation struct {
	Action, Resource, Capability, Purpose string
	Dimensions                            []string
	TargetTTL                             time.Duration // required for idempotent-key targets
	Sensitivity, DataClass                string        // required by authenticated ICS/transport configuration
}
type Config struct {
	Domain                               string
	CommandKeys                          map[string]ed25519.PublicKey
	DecisionKeys                         map[string]ed25519.PublicKey
	ResultKeys                           map[string]ed25519.PublicKey
	RootKeys                             map[string]ed25519.PublicKey
	Operations                           map[string]Operation // target_ref -> server-controlled policy
	LeaseTTL, LeaseGrace, DecisionMaxAge time.Duration
}
type Store struct {
	db  *pgxpool.Pool
	cfg Config
}

func New(db *pgxpool.Pool, c Config) (*Store, error) {
	if db == nil || c.Domain == "" || len(c.CommandKeys) == 0 || len(c.DecisionKeys) == 0 || len(c.ResultKeys) == 0 || len(c.Operations) == 0 {
		return nil, Invalid
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 30 * time.Second
	}
	if c.LeaseGrace < 0 {
		return nil, Invalid
	}
	if c.LeaseGrace == 0 {
		c.LeaseGrace = 15 * time.Second
	}
	if c.DecisionMaxAge <= 0 {
		c.DecisionMaxAge = 60 * time.Second
	}
	copyKeys := func(in map[string]ed25519.PublicKey) (map[string]ed25519.PublicKey, error) {
		out := map[string]ed25519.PublicKey{}
		for k, v := range in {
			if len(v) != ed25519.PublicKeySize {
				return nil, Invalid
			}
			out[k] = append(ed25519.PublicKey(nil), v...)
		}
		return out, nil
	}
	var err error
	c.CommandKeys, err = copyKeys(c.CommandKeys)
	if err != nil {
		return nil, err
	}
	c.DecisionKeys, err = copyKeys(c.DecisionKeys)
	if err != nil {
		return nil, err
	}
	c.ResultKeys, err = copyKeys(c.ResultKeys)
	if err != nil {
		return nil, err
	}
	c.RootKeys, err = copyKeys(c.RootKeys)
	if err != nil {
		return nil, err
	}
	ops := map[string]Operation{}
	for target, op := range c.Operations {
		if target == "" || op.Action == "" || op.Resource == "" || op.Purpose != "tool_call" || len(op.Dimensions) == 0 {
			return nil, Invalid
		}
		if op.Capability != "opaque" && op.Capability != "queryable" && op.Capability != "idempotent-key" {
			return nil, Invalid
		}
		if op.TargetTTL < 0 || (op.Capability == "idempotent-key" && op.TargetTTL < time.Millisecond) {
			return nil, Invalid
		}
		seen := map[string]bool{}
		for _, d := range op.Dimensions {
			if !dimension(d) || seen[d] {
				return nil, Invalid
			}
			seen[d] = true
		}
		op.Dimensions = append([]string(nil), op.Dimensions...)
		ops[target] = op
	}
	c.Operations = ops
	return &Store{db: db, cfg: c}, nil
}

type Command struct {
	ID              string            `json:"command_id"`
	Audience        string            `json:"audience"`
	NotAfter        time.Time         `json:"not_after"`
	ContractID      string            `json:"contract_id"`
	ContractVersion int               `json:"contract_version"`
	Contract        json.RawMessage   `json:"contract"`
	Limits          map[string]string `json:"limits"`
	Root            *RootRequest      `json:"root,omitempty"`
}
type Submission struct {
	Kind    string  `json:"kind"`
	Caller  Caller  `json:"caller"`
	Command Command `json:"command"`
}
type Admission struct {
	TaskID, ReceiptID string
	Existing          bool
	RootGrantID       string
}
type Reservation struct{ ID, Dimension, Amount, State string }
type Lease struct {
	ID             string
	Epoch, Version int64
	ExpiresAt      time.Time
}
type Proposal struct {
	EffectID, TaskID, CommandID, Target, ParamsDigest, AuthorityDigest, LeaseID, Sensitivity string
	Epoch, ExpectedVersion                                                                   int64
	Reservations                                                                             map[string]string // dimension -> reservation ID
	UpperBounds                                                                              map[string]string
	DataClass                                                                                string
}
type Effect struct {
	ID, State string
	Existing  bool
}
type Envelope = signed.Envelope
type Decision struct {
	Kind            string    `json:"kind"`
	ID              string    `json:"decision_id"`
	Allow           bool      `json:"allow"`
	At              time.Time `json:"at"`
	ExpiresAt       time.Time `json:"expires_at"`
	Subject         string    `json:"subject"`
	Agent           string    `json:"actor_agent"`
	Instance        string    `json:"actor_instance"`
	Grant           string    `json:"grant_id"`
	Task            string    `json:"task_id"`
	Target          string    `json:"target"`
	Action          string    `json:"action"`
	Resource        string    `json:"resource"`
	ParamsDigest    string    `json:"params_digest"`
	AuthorityDigest string    `json:"authority_digest"`
	Audience        string    `json:"audience"`
	Sensitivity     string    `json:"sensitivity"`
	DataClass       string    `json:"data_class,omitempty"`
	PolicyVersion   string    `json:"policy_version,omitempty"`
}

// Result is signed by a trusted adapter/reconciler. Worker self-report is not
// sufficient to establish a terminal business fact or to release UNKNOWN holds.
type Result struct {
	Kind         string            `json:"kind"`
	Task         string            `json:"task_id"`
	Effect       string            `json:"effect_id"`
	ParamsDigest string            `json:"params_digest"`
	Audience     string            `json:"audience"`
	Outcome      string            `json:"outcome"`
	Usage        map[string]string `json:"usage"`
	UsageSource  string            `json:"usage_source"`
	Evidence     string            `json:"evidence"`
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func amount(a string, positive bool) bool { return decimal.Valid(a) && (!positive || a != "0") }
func dimension(d string) bool {
	return d == "money" || d == "tokens" || d == "calls" || d == "messages"
}
func id(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	ms := time.Now().UnixMilli()
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	b[6] = (b[6] & 15) | 0x70
	b[8] = (b[8] & 63) | 0x80
	return prefix + "_" + hex.EncodeToString(b[:])
}
func verify(e Envelope, keys map[string]ed25519.PublicKey, out any) error {
	if err := signed.Decode(e, keys, out); err != nil {
		return Denied
	}
	return nil
}

type RootRequest struct {
	ConsentID string           `json:"consent_id"`
	Rights    authority.Rights `json:"rights"`
}
type RootPermit struct {
	Kind           string           `json:"kind"`
	GrantID        string           `json:"grant_id"`
	CommandDigest  string           `json:"command_digest"`
	ConsentID      string           `json:"consent_id"`
	ConsentVersion int64            `json:"consent_version"`
	Caller         Caller           `json:"caller"`
	Audience       string           `json:"audience"`
	IssuedAt       time.Time        `json:"issued_at"`
	ExpiresAt      time.Time        `json:"expires_at"`
	GrantNotBefore time.Time        `json:"grant_not_before"`
	GrantExpiresAt time.Time        `json:"grant_expires_at"`
	PolicyVersion  string           `json:"policy_version"`
	Rights         authority.Rights `json:"rights"`
}

type task struct {
	ID, Status        string
	Version, Epoch    int64
	Cancelled, Frozen bool
	Deadline          time.Time
}

func (s *Store) begin(ctx context.Context) (pgx.Tx, error) {
	return s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
}
func (s *Store) identity(ctx context.Context, tx pgx.Tx, c Caller) error {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM principals p JOIN agents a ON a.owner_principal_id=p.principal_id JOIN agent_instances i ON i.agent_id=a.agent_id
 WHERE p.principal_id=$1 AND a.agent_id=$2 AND i.instance_id=$3 AND p.domain_id=$4 AND a.domain_id=$4 AND p.revoked_at IS NULL AND i.revoked_at IS NULL AND a.status='active' AND ($5='' OR i.cred_hash=$5))`, c.Principal, c.Agent, c.Instance, s.cfg.Domain, c.CredentialHash).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return Denied
	}
	return nil
}
func (s *Store) lockTask(ctx context.Context, tx pgx.Tx, c Caller, taskID string) (task, error) {
	var t task
	t.ID = taskID
	err := tx.QueryRow(ctx, `SELECT status,task_version,current_epoch,cancel_version IS NOT NULL,budget_frozen,deadline FROM tasks WHERE task_id=$1 AND owner_principal_id=$2 AND domain_id=$3 FOR UPDATE`, taskID, c.Principal, s.cfg.Domain).Scan(&t.Status, &t.Version, &t.Epoch, &t.Cancelled, &t.Frozen, &t.Deadline)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, Denied
	}
	if err != nil {
		return t, err
	}
	return t, s.identity(ctx, tx, c)
}
func currentTime(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now)
	return now, err
}
func active(t task, now time.Time) error {
	if t.Cancelled || t.Status == "SUCCEEDED" || t.Status == "FAILED" || t.Status == "CANCELLED" {
		return Terminal
	}
	if now.After(t.Deadline) {
		return Deadline
	}
	if t.Frozen {
		return BudgetViolation
	}
	return nil
}
func event(ctx context.Context, tx pgx.Tx, taskID, kind string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO task_events(task_id,root_task_id,event_type,payload) SELECT task_id,root_task_id,$2,$3::jsonb FROM tasks WHERE task_id=$1`, taskID, kind, string(b))
	return err
}
func audit(ctx context.Context, tx pgx.Tx, domain, taskID, effectID, action string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log(domain_id,task_id,effect_id,action,decision,cost) VALUES($1,$2,$3,$4,'record',$5::jsonb)`, domain, taskID, effectID, action, string(b))
	return err
}
func commit(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
