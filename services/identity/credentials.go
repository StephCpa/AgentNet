package identity

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/internal/pki"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func (s *Service) instances(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		s.fail(w, r, 503, "database")
		return
	}
	defer tx.Rollback(r.Context())
	r = withTransaction(r, tx)
	principal, session, err := s.form(w, r, tx, "action", "agent", "instance", "version", "csr", "request_id")
	if err != nil {
		s.fail(w, r, 403, "instance authentication or CSRF")
		return
	}
	action, agent, instance := r.PostForm.Get("action"), r.PostForm.Get("agent"), r.PostForm.Get("instance")
	if action != "enroll" && action != "rotate" && action != "revoke" {
		s.fail(w, r, 400, "invalid instance action")
		return
	}
	var expected int64
	if action != "enroll" {
		expected, err = strconv.ParseInt(r.PostForm.Get("version"), 10, 64)
		if err != nil || expected < 1 || instance == "" {
			s.fail(w, r, 400, "instance version required")
			return
		}
	}
	var certificate []byte
	var version int64
	existing := false
	if action == "revoke" {
		var owned string
		err = tx.QueryRow(r.Context(), `SELECT i.instance_id FROM agent_instances i JOIN agents a ON a.agent_id=i.agent_id WHERE i.instance_id=$1 AND a.domain_id=$2 AND a.owner_principal_id=$3 AND i.credential_version=$4 AND i.revoked_at IS NULL FOR UPDATE OF i`, instance, s.cfg.Domain, principal, expected).Scan(&owned)
		if err != nil {
			s.fail(w, r, 409, "instance ownership or version mismatch")
			return
		}
		_, err = tx.Exec(r.Context(), `UPDATE agent_instances SET revoked_at=clock_timestamp(),credential_version=credential_version+1 WHERE instance_id=$1`, instance)
		if err != nil {
			s.fail(w, r, 503, "instance revocation")
			return
		}
		version = expected + 1
	} else {
		if s.cfg.CredentialIssuer == nil {
			s.fail(w, r, 503, "credential issuer not configured")
			return
		}
		requestID := r.PostForm.Get("request_id")
		if !requestIDRE.MatchString(requestID) {
			s.fail(w, r, 400, "issuance request id required")
			return
		}
		csr, err := pki.ParseCSR([]byte(r.PostForm.Get("csr")))
		if err != nil {
			s.fail(w, r, 400, "invalid CSR proof or key")
			return
		}
		if action == "enroll" && (instance != "" || r.PostForm.Get("version") != "") {
			s.fail(w, r, 400, "instance ID is server assigned")
			return
		}
		payload, _ := json.Marshal(struct {
			Action, Agent, Instance, CSR string
			Version                      int64
		}{action, agent, instance, pki.Fingerprint(csr.Raw), expected})
		bound, err := canonical.Digest(payload)
		if err != nil {
			s.fail(w, r, 400, "invalid issuance descriptor")
			return
		}
		if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "anet.credential:"+s.cfg.Domain+":"+principal+":"+requestID); err != nil {
			s.fail(w, r, 503, "issuance lock")
			return
		}
		var stored string
		var active bool
		err = tx.QueryRow(r.Context(), `SELECT r.payload_digest,c.instance_id,c.version,c.certificate_pem,c.retired_at IS NULL AND c.expires_at>clock_timestamp() AND i.revoked_at IS NULL AND i.cred_hash=c.fingerprint AND a.owner_principal_id=$2 AND a.status='active'
   FROM credential_requests r JOIN instance_credentials c USING(fingerprint) JOIN agent_instances i ON i.instance_id=c.instance_id JOIN agents a ON a.agent_id=i.agent_id WHERE r.domain_id=$1 AND r.owner_principal_id=$2 AND r.request_id=$3`, s.cfg.Domain, principal, requestID).Scan(&stored, &instance, &version, &certificate, &active)
		if err == nil {
			if stored != bound || !active {
				s.fail(w, r, 409, "issuance replay conflict or retired credential")
				return
			}
			existing = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			s.fail(w, r, 503, "issuance lookup")
			return
		} else {
			var owned string
			err = tx.QueryRow(r.Context(), `SELECT agent_id FROM agents WHERE agent_id=$1 AND domain_id=$2 AND owner_principal_id=$3 AND status='active' FOR SHARE`, agent, s.cfg.Domain, principal).Scan(&owned)
			if err != nil {
				s.fail(w, r, 403, "agent not owned")
				return
			}
			if action == "rotate" {
				var current int64
				err = tx.QueryRow(r.Context(), `SELECT credential_version FROM agent_instances WHERE instance_id=$1 AND agent_id=$2 AND revoked_at IS NULL FOR UPDATE`, instance, agent).Scan(&current)
				if err != nil || current != expected {
					s.fail(w, r, 409, "instance rotation version mismatch")
					return
				}
				version = current + 1
			} else {
				if err = tx.QueryRow(r.Context(), `SELECT 'ins_'||gen_random_uuid()::text`).Scan(&instance); err != nil {
					s.fail(w, r, 503, "instance identifier")
					return
				}
				version = 1
			}
			var now time.Time
			if err = tx.QueryRow(r.Context(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
				s.fail(w, r, 503, "database clock")
				return
			}
			issued, err := s.cfg.CredentialIssuer.Issue(csr, instance, now)
			if err != nil {
				s.fail(w, r, 400, "certificate issuance rejected")
				return
			}
			if action == "enroll" {
				_, err = tx.Exec(r.Context(), `INSERT INTO agent_instances(instance_id,agent_id,cred_hash,spiffe_id) VALUES($1,$2,$3,$4)`, instance, agent, issued.Fingerprint, issued.URI)
			} else {
				_, err = tx.Exec(r.Context(), `UPDATE agent_instances SET cred_hash=$2,spiffe_id=$3,credential_version=$4 WHERE instance_id=$1`, instance, issued.Fingerprint, issued.URI, version)
			}
			if err == nil {
				_, err = tx.Exec(r.Context(), `INSERT INTO instance_credentials(fingerprint,instance_id,version,public_key_hash,certificate_der,certificate_pem,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, issued.Fingerprint, instance, version, issued.PublicKeyHash, issued.DER, issued.PEM, issued.Expires)
			}
			if err != nil {
				var conflict *pgconn.PgError
				if errors.As(err, &conflict) && conflict.Code == "23505" {
					s.fail(w, r, 409, "credential key already enrolled")
				} else {
					s.fail(w, r, 503, "credential persistence")
				}
				return
			}
			_, err = tx.Exec(r.Context(), `INSERT INTO credential_requests(domain_id,owner_principal_id,request_id,payload_digest,fingerprint) VALUES($1,$2,$3,$4,$5)`, s.cfg.Domain, principal, requestID, bound, issued.Fingerprint)
			if err != nil {
				s.fail(w, r, 503, "issuance idempotency persistence")
				return
			}
			certificate = issued.PEM
		}
	}
	var live bool
	err = tx.QueryRow(r.Context(), `SELECT expires_at>clock_timestamp() FROM user_sessions WHERE session_hash=$1`, digest(session)).Scan(&live)
	if err != nil || !live {
		s.fail(w, r, 401, "session expired during credential operation")
		return
	}
	if err = s.audit(r.Context(), tx, principal, "instance."+action, "allow", fmt.Sprintf("%s:%d", instance, version)); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.fail(w, r, 503, "credential transaction commit")
		return
	}
	if action == "revoke" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-v%d.pem"`, instance, version))
	w.Header().Set("X-AgentNet-Instance", instance)
	w.Header().Set("X-AgentNet-Credential-Version", strconv.FormatInt(version, 10))
	w.Header().Set("X-AgentNet-Existing", strconv.FormatBool(existing))
	_, _ = w.Write(certificate)
}
