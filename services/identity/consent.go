package identity

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/agentnet/agentnet/internal/amount"
	"github.com/jackc/pgx/v5"
)

func (s *Service) session(r *http.Request, tx pgx.Tx) (string, string, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || len(c.Value) != 43 {
		return "", "", errors.New("session required")
	}
	var principal string
	err = tx.QueryRow(r.Context(), `SELECT p.principal_id FROM user_sessions u JOIN principals p ON p.principal_id=u.principal_id JOIN oidc_subjects o ON o.domain_id=u.domain_id AND o.issuer=u.issuer AND o.subject=u.subject AND o.principal_id=u.principal_id WHERE u.session_hash=$1 AND u.domain_id=$2 AND u.issuer=$3 AND u.revoked_at IS NULL AND u.expires_at>clock_timestamp() AND p.domain_id=$2 AND p.kind='user' AND p.revoked_at IS NULL FOR SHARE OF u,p,o`, digest(c.Value), s.cfg.Domain, s.cfg.Issuer).Scan(&principal)
	return principal, c.Value, err
}
func (s *Service) form(w http.ResponseWriter, r *http.Request, tx pgx.Tx, allowed ...string) (string, string, error) {
	if r.Header.Get("Origin") != s.cfg.PublicURL || strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/x-www-form-urlencoded" {
		return "", "", errors.New("same-origin form required")
	}
	if err := r.ParseForm(); err != nil {
		return "", "", err
	}
	valid := map[string]bool{"csrf": true}
	for _, key := range allowed {
		valid[key] = true
	}
	for key, values := range r.PostForm {
		if !valid[key] || len(values) != 1 {
			return "", "", errors.New("unknown or repeated form field")
		}
	}
	principal, session, err := s.session(r, tx)
	if err != nil {
		return "", "", err
	}
	if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(csrf(session))) != 1 {
		return "", "", errors.New("CSRF mismatch")
	}
	return principal, session, nil
}
func (s *Service) consent(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		s.fail(w, r, 503, "database")
		return
	}
	defer tx.Rollback(r.Context())
	r = withTransaction(r, tx)
	principal, session, err := s.form(w, r, tx, "action", "profile", "agent", "instance", "hours", "money", "tokens", "calls", "messages", "consent", "version")
	if err != nil {
		s.fail(w, r, 403, "consent authentication or CSRF")
		return
	}
	action := r.PostForm.Get("action")
	var id string
	if action == "revoke" {
		version, e := strconv.ParseInt(r.PostForm.Get("version"), 10, 64)
		if e != nil || version < 1 {
			s.fail(w, r, 400, "version required")
			return
		}
		id = r.PostForm.Get("consent")
		tag, e := tx.Exec(r.Context(), `UPDATE root_consents SET revoked_at=clock_timestamp(),version=version+1 WHERE consent_id=$1 AND domain_id=$2 AND subject_principal_id=$3 AND version=$4 AND revoked_at IS NULL`, id, s.cfg.Domain, principal, version)
		if e != nil {
			s.fail(w, r, 503, "revoke failed")
			return
		}
		if tag.RowsAffected() != 1 {
			s.fail(w, r, 409, "consent version or ownership mismatch")
			return
		}
	} else if action == "create" {
		profile, ok := s.cfg.Profiles[r.PostForm.Get("profile")]
		hours, e := strconv.Atoi(r.PostForm.Get("hours"))
		if !ok || e != nil || hours < 1 || hours > profile.MaxHours {
			s.fail(w, r, 400, "invalid profile or duration")
			return
		}
		agent, instance := r.PostForm.Get("agent"), r.PostForm.Get("instance")
		var owner string
		err = tx.QueryRow(r.Context(), `SELECT owner_principal_id FROM agents WHERE agent_id=$1 AND domain_id=$2 AND owner_principal_id=$3 AND status='active' FOR SHARE`, agent, s.cfg.Domain, principal).Scan(&owner)
		if err != nil {
			s.fail(w, r, 403, "agent not owned")
			return
		}
		if instance != "" {
			var present string
			err = tx.QueryRow(r.Context(), `SELECT instance_id FROM agent_instances WHERE instance_id=$1 AND agent_id=$2 AND revoked_at IS NULL FOR SHARE`, instance, agent).Scan(&present)
			if err != nil {
				s.fail(w, r, 403, "instance not owned")
				return
			}
		}
		limits := map[string]string{}
		for _, dim := range dimensions {
			value := r.PostForm.Get(dim)
			if !amount.Valid(value) {
				s.fail(w, r, 400, "invalid amount")
				return
			}
			var within bool
			err = tx.QueryRow(r.Context(), `SELECT $1::numeric <= $2::numeric`, value, profile.TaskLimits[dim]).Scan(&within)
			if err != nil || !within {
				s.fail(w, r, 400, "profile budget exceeded")
				return
			}
			limits[dim] = value
		}
		rights, _ := json.Marshal(profile.Rights)
		lb, _ := json.Marshal(limits)
		err = tx.QueryRow(r.Context(), `INSERT INTO root_consents(consent_id,domain_id,subject_principal_id,actor_agent_id,actor_instance_id,rights,task_limits,not_before,expires_at,policy_version) VALUES('cns_'||gen_random_uuid()::text,$1,$2,$3,NULLIF($4,''),$5::jsonb,$6::jsonb,clock_timestamp(),clock_timestamp()+$7::int*interval '1 hour',$8) RETURNING consent_id`, s.cfg.Domain, principal, agent, instance, string(rights), string(lb), hours, profile.PolicyVersion).Scan(&id)
		if err != nil {
			s.fail(w, r, 503, "consent insert")
			return
		}
	} else {
		s.fail(w, r, 400, "unknown consent action")
		return
	}
	// Locks can outlast the session. Recheck time immediately before commit.
	var live bool
	err = tx.QueryRow(r.Context(), `SELECT expires_at>clock_timestamp() FROM user_sessions WHERE session_hash=$1`, digest(session)).Scan(&live)
	if err != nil || !live {
		s.fail(w, r, 401, "session expired during operation")
		return
	}
	if err = s.audit(r.Context(), tx, principal, "consent."+action, "allow", id); err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.fail(w, r, 503, "consent commit")
		return
	}
	w.Header().Set("X-AgentNet-Consent", id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		s.fail(w, r, 503, "database")
		return
	}
	defer tx.Rollback(r.Context())
	r = withTransaction(r, tx)
	principal, session, err := s.form(w, r, tx)
	if err != nil {
		s.fail(w, r, 403, "logout authentication or CSRF")
		return
	}
	_, err = tx.Exec(r.Context(), `UPDATE user_sessions SET revoked_at=clock_timestamp() WHERE session_hash=$1`, digest(session))
	if err == nil {
		err = s.audit(r.Context(), tx, principal, "identity.logout", "allow", principal)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.fail(w, r, 503, "logout commit")
		return
	}
	cookie(w, sessionCookie, "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
