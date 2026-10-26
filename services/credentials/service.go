// Package credentials implements two-phase, instance-authenticated renewal.
package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/internal/pki"
	"github.com/agentnet/agentnet/services/tcs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"regexp"
	"time"
)

var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

type Service struct {
	db     *pgxpool.Pool
	domain string
	issuer *pki.Issuer
}
type Status struct {
	Instance, Fingerprint string
	Version               int64
	Expires               time.Time
}
type Prepared struct {
	Instance       string
	Version        int64
	PEM            []byte
	ActivateBefore time.Time
}

func New(db *pgxpool.Pool, domain string, issuer *pki.Issuer) (*Service, error) {
	if db == nil || domain == "" || issuer == nil {
		return nil, tcs.Invalid
	}
	return &Service{db, domain, issuer}, nil
}
func translate(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return tcs.Denied
	}
	var p *pgconn.PgError
	if errors.As(err, &p) && p.Code == "23505" {
		return tcs.Conflict
	}
	return err
}

// Instance lock comes before renewal rows, shared with owner-driven rotation.
func (s *Service) lock(ctx context.Context, tx pgx.Tx, c tcs.Caller) (Status, error) {
	var out Status
	if c.CredentialHash == "" || c.CredentialExpiresAt == nil {
		return out, tcs.Denied
	}
	err := tx.QueryRow(ctx, `SELECT i.instance_id,i.credential_version,i.cred_hash FROM agent_instances i JOIN agents a ON a.agent_id=i.agent_id JOIN principals p ON p.principal_id=a.owner_principal_id WHERE i.instance_id=$1 AND a.agent_id=$2 AND p.principal_id=$3 AND a.domain_id=$4 AND p.domain_id=$4 AND a.status='active' AND p.revoked_at IS NULL AND i.revoked_at IS NULL FOR UPDATE OF i`, c.Instance, c.Agent, c.Principal, s.domain).Scan(&out.Instance, &out.Version, &out.Fingerprint)
	return out, translate(err)
}
func (s *Service) fresh(ctx context.Context, tx pgx.Tx, c tcs.Caller) error {
	var live bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agents a JOIN principals p ON p.principal_id=a.owner_principal_id WHERE a.agent_id=$1 AND p.principal_id=$2 AND a.domain_id=$3 AND p.domain_id=$3 AND a.status='active' AND p.revoked_at IS NULL AND clock_timestamp()<$4)`, c.Agent, c.Principal, s.domain, c.CredentialExpiresAt).Scan(&live)
	if err != nil {
		return err
	}
	if !live {
		return tcs.Denied
	}
	return nil
}
func (s *Service) audit(ctx context.Context, tx pgx.Tx, c tcs.Caller, action, detail string) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_log(domain_id,principal_id,agent_instance_id,action,decision,detail_digest) VALUES($1,$2,$3,$4,'allow',$5)`, s.domain, c.Principal, c.Instance, action, detail)
	return err
}
func (s *Service) GetStatus(ctx context.Context, c tcs.Caller) (Status, error) {
	var out Status
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	out, err = s.lock(ctx, tx, c)
	if err != nil {
		return out, err
	}
	if out.Fingerprint != c.CredentialHash {
		return Status{}, tcs.Denied
	}
	if err = s.fresh(ctx, tx, c); err != nil {
		return Status{}, err
	}
	err = tx.QueryRow(ctx, `SELECT expires_at FROM instance_credentials WHERE instance_id=$1 AND fingerprint=$2 AND retired_at IS NULL AND expires_at>clock_timestamp()`, c.Instance, c.CredentialHash).Scan(&out.Expires)
	if err != nil {
		return Status{}, translate(err)
	}
	return out, tx.Commit(ctx)
}
func (s *Service) Prepare(ctx context.Context, c tcs.Caller, requestID string, expected int64, raw []byte) (Prepared, error) {
	var out Prepared
	if !requestIDRE.MatchString(requestID) || expected < 1 {
		return out, tcs.Invalid
	}
	csr, err := pki.ParseCSR(raw)
	if err != nil {
		return out, tcs.Invalid
	}
	descriptor, _ := json.Marshal(struct {
		Request string
		Version int64
		CSR     string
	}{requestID, expected, pki.Fingerprint(csr.Raw)})
	digest, err := canonical.Digest(descriptor)
	if err != nil {
		return out, tcs.Invalid
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	current, err := s.lock(ctx, tx, c)
	if err != nil {
		return out, err
	}
	if current.Fingerprint != c.CredentialHash {
		return out, tcs.Denied
	}
	if current.Version != expected {
		return out, tcs.VersionConflict
	}
	if err = s.fresh(ctx, tx, c); err != nil {
		return out, err
	}
	var oldKey string
	err = tx.QueryRow(ctx, `SELECT public_key_hash FROM instance_credentials WHERE instance_id=$1 AND fingerprint=$2 AND retired_at IS NULL AND expires_at>clock_timestamp()`, c.Instance, c.CredentialHash).Scan(&oldKey)
	if err != nil {
		return out, translate(err)
	}
	if oldKey == pki.Fingerprint(csr.RawSubjectPublicKeyInfo) {
		return out, tcs.Conflict
	}
	var stored, state string
	var before time.Time
	err = tx.QueryRow(ctx, `SELECT payload_digest,status,certificate_pem,prepare_expires_at FROM credential_renewals WHERE instance_id=$1 AND request_id=$2`, c.Instance, requestID).Scan(&stored, &state, &out.PEM, &before)
	if err == nil {
		var live bool
		if e := tx.QueryRow(ctx, `SELECT clock_timestamp()<$1`, before).Scan(&live); e != nil {
			return out, e
		}
		if stored != digest || state != "pending" || !live {
			return Prepared{}, tcs.Conflict
		}
		out.Instance = c.Instance
		out.Version = expected + 1
		out.ActivateBefore = before
		return out, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	_, err = tx.Exec(ctx, `UPDATE credential_renewals SET status=CASE WHEN base_version<>$2 OR base_fingerprint<>$3 THEN 'superseded' ELSE 'expired' END WHERE instance_id=$1 AND status='pending' AND (base_version<>$2 OR base_fingerprint<>$3 OR prepare_expires_at<=clock_timestamp())`, c.Instance, current.Version, current.Fingerprint)
	if err != nil {
		return out, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return out, err
	}
	issued, err := s.issuer.Issue(csr, c.Instance, now)
	if err != nil {
		return out, tcs.Invalid
	}
	before = now.Add(5 * time.Minute)
	if before.After(issued.Expires) {
		before = issued.Expires
	}
	_, err = tx.Exec(ctx, `INSERT INTO credential_renewals(instance_id,request_id,base_version,base_fingerprint,payload_digest,fingerprint,public_key_hash,certificate_der,certificate_pem,certificate_expires_at,prepare_expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, c.Instance, requestID, expected, current.Fingerprint, digest, issued.Fingerprint, issued.PublicKeyHash, issued.DER, issued.PEM, issued.Expires, before)
	if err != nil {
		return Prepared{}, translate(err)
	}
	// A key-claim lock can wait behind another issuer; recheck auth after it.
	if err = s.fresh(ctx, tx, c); err != nil {
		return Prepared{}, err
	}
	var usable bool
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()<$1`, before).Scan(&usable); err != nil {
		return Prepared{}, err
	}
	if !usable {
		return Prepared{}, tcs.Expired
	}
	if err = s.audit(ctx, tx, c, "credential.renewal_prepared", digest); err != nil {
		return Prepared{}, err
	}
	out = Prepared{c.Instance, expected + 1, issued.PEM, before}
	return out, tx.Commit(ctx)
}
func (s *Service) Activate(ctx context.Context, c tcs.Caller, requestID string) (Status, error) {
	var out Status
	if !requestIDRE.MatchString(requestID) {
		return out, tcs.Invalid
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	current, err := s.lock(ctx, tx, c)
	if err != nil {
		return out, err
	}
	if err = s.fresh(ctx, tx, c); err != nil {
		return out, err
	}
	var base int64
	var old, fp, key, state, digest string
	var der, cert []byte
	var expires, until time.Time
	err = tx.QueryRow(ctx, `SELECT base_version,base_fingerprint,fingerprint,public_key_hash,status,payload_digest,certificate_der,certificate_pem,certificate_expires_at,prepare_expires_at FROM credential_renewals WHERE instance_id=$1 AND request_id=$2 FOR UPDATE`, c.Instance, requestID).Scan(&base, &old, &fp, &key, &state, &digest, &der, &cert, &expires, &until)
	if err != nil {
		return out, translate(err)
	}
	if fp != c.CredentialHash {
		return out, tcs.Denied
	}
	out = Status{c.Instance, fp, base + 1, expires}
	if state == "active" && current.Version == base+1 && current.Fingerprint == fp {
		return out, tx.Commit(ctx)
	}
	var live bool
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()<$1 AND clock_timestamp()<$2`, until, expires).Scan(&live); err != nil {
		return Status{}, err
	}
	if state != "pending" || current.Version != base || current.Fingerprint != old || !live {
		return Status{}, tcs.Denied
	}
	_, err = tx.Exec(ctx, `INSERT INTO instance_credentials(fingerprint,instance_id,version,public_key_hash,certificate_der,certificate_pem,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, fp, c.Instance, base+1, key, der, cert, expires)
	if err != nil {
		return Status{}, translate(err)
	}
	if err = s.fresh(ctx, tx, c); err != nil {
		return Status{}, err
	}
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()<$1`, until).Scan(&live); err != nil {
		return Status{}, err
	}
	if !live {
		return Status{}, tcs.Expired
	}
	_, err = tx.Exec(ctx, `UPDATE agent_instances SET cred_hash=$2,credential_version=$3 WHERE instance_id=$1`, c.Instance, fp, base+1)
	if err != nil {
		return Status{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE credential_renewals SET status='active',activated_at=clock_timestamp() WHERE instance_id=$1 AND request_id=$2`, c.Instance, requestID)
	if err != nil {
		return Status{}, err
	}
	if err = s.audit(ctx, tx, c, "credential.renewal_activated", digest); err != nil {
		return Status{}, err
	}
	return out, tx.Commit(ctx)
}

func (s *Service) Abandon(ctx context.Context, c tcs.Caller, requestID string, expected int64) (Status, error) {
	var out Status
	if !requestIDRE.MatchString(requestID) || expected < 1 {
		return out, tcs.Invalid
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	out, err = s.lock(ctx, tx, c)
	if err != nil {
		return out, err
	}
	if out.Fingerprint != c.CredentialHash || out.Version != expected {
		return Status{}, tcs.Denied
	}
	if err = s.fresh(ctx, tx, c); err != nil {
		return Status{}, err
	}
	err = tx.QueryRow(ctx, `SELECT expires_at FROM instance_credentials WHERE instance_id=$1 AND fingerprint=$2 AND retired_at IS NULL`, c.Instance, c.CredentialHash).Scan(&out.Expires)
	if err != nil {
		return Status{}, translate(err)
	}
	var state string
	var base int64
	err = tx.QueryRow(ctx, `SELECT status,base_version FROM credential_renewals WHERE instance_id=$1 AND request_id=$2 FOR UPDATE`, c.Instance, requestID).Scan(&state, &base)
	if err == nil {
		if state == "active" || base != expected {
			return Status{}, tcs.Denied
		}
		if _, err = tx.Exec(ctx, `UPDATE credential_renewals SET status='superseded' WHERE instance_id=$1 AND request_id=$2`, c.Instance, requestID); err != nil {
			return Status{}, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Status{}, err
	}
	if err = s.fresh(ctx, tx, c); err != nil {
		return Status{}, err
	}
	detail, _ := canonical.Digest([]byte(`{"request_id":"` + requestID + `"}`))
	if err = s.audit(ctx, tx, c, "credential.renewal_abandoned", detail); err != nil {
		return Status{}, err
	}
	return out, tx.Commit(ctx)
}
