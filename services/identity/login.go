package identity

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

const sessionCookie = "__Host-anet-session"
const loginCookie = "__Host-anet-login"

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	state, browser, nonce, verifier := random(), random(), random(), oauth2.GenerateVerifier()
	// Expired credentials have no recovery use. Cleanup stays within this domain.
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		s.fail(w, r, 503, "database")
		return
	}
	defer tx.Rollback(r.Context())
	r = withTransaction(r, tx)
	_, err = tx.Exec(r.Context(), `DELETE FROM oidc_logins WHERE domain_id=$1 AND expires_at<=clock_timestamp()`, s.cfg.Domain)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO oidc_logins(state_hash,domain_id,issuer,browser_hash,nonce,pkce_verifier,expires_at) VALUES($1,$2,$3,$4,$5,$6,clock_timestamp()+interval '5 minutes')`, digest(state), s.cfg.Domain, s.cfg.Issuer, digest(browser), nonce, verifier)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		s.fail(w, r, 503, "login storage")
		return
	}
	cookie(w, loginCookie, browser, 300)
	http.Redirect(w, r, s.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusSeeOther)
}
func one(r *http.Request, key string) string {
	values := r.URL.Query()[key]
	if len(values) != 1 {
		return ""
	}
	return values[0]
}
func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	state, code := one(r, "state"), one(r, "code")
	browser, err := r.Cookie(loginCookie)
	if err != nil || len(state) != 43 || code == "" || len(code) > 4096 || len(browser.Value) != 43 || r.URL.Query().Get("error") != "" {
		s.fail(w, r, 400, "invalid callback")
		return
	}
	var nonce, verifier string
	// Atomically consume the transaction before network I/O. A second callback
	// cannot redeem it; a failure requires a fresh login, not token re-use.
	err = s.db.QueryRow(r.Context(), `DELETE FROM oidc_logins WHERE state_hash=$1 AND domain_id=$2 AND issuer=$3 AND browser_hash=$4 AND expires_at>clock_timestamp() RETURNING nonce,pkce_verifier`, digest(state), s.cfg.Domain, s.cfg.Issuer, digest(browser.Value)).Scan(&nonce, &verifier)
	if err != nil {
		s.fail(w, r, 401, "state or browser mismatch")
		return
	}
	cookie(w, loginCookie, "", -1)
	ctx := oidc.ClientContext(r.Context(), s.client)
	token, err := s.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		s.fail(w, r, 401, "code exchange")
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || len(raw) > 64<<10 {
		s.fail(w, r, 401, "missing ID token")
		return
	}
	id, err := s.verifier.Verify(ctx, raw)
	if err != nil {
		s.fail(w, r, 401, "ID token verification")
		return
	}
	now := time.Now()
	var claims struct {
		AZP string `json:"azp"`
	}
	if id.Claims(&claims) != nil || id.Issuer != s.cfg.Issuer || id.Subject == "" || len(id.Subject) > 1024 || subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(nonce)) != 1 || id.IssuedAt.IsZero() || id.IssuedAt.After(now.Add(500*time.Millisecond)) || id.IssuedAt.Before(now.Add(-time.Hour)) || !id.Expiry.After(now.Add(500*time.Millisecond)) || id.Expiry.Sub(id.IssuedAt) > time.Hour || (len(id.Audience) > 1 && claims.AZP != s.cfg.ClientID) || (claims.AZP != "" && claims.AZP != s.cfg.ClientID) {
		s.fail(w, r, 401, "ID token claims")
		return
	}
	if id.AccessTokenHash != "" && id.VerifyAccessToken(token.AccessToken) != nil {
		s.fail(w, r, 401, "access token binding")
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.fail(w, r, 503, "database")
		return
	}
	defer tx.Rollback(ctx)
	r = withTransaction(r, tx)
	var principal string
	err = tx.QueryRow(ctx, `SELECT p.principal_id FROM oidc_subjects o JOIN principals p ON p.principal_id=o.principal_id WHERE o.domain_id=$1 AND o.issuer=$2 AND o.subject=$3 AND p.domain_id=$1 AND p.kind='user' AND p.revoked_at IS NULL FOR SHARE OF p,o`, s.cfg.Domain, s.cfg.Issuer, id.Subject).Scan(&principal)
	if errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, r, 403, "unmapped or revoked user")
		return
	}
	if err != nil {
		s.fail(w, r, 503, "mapping lookup")
		return
	}
	session := random()
	if old, e := r.Cookie(sessionCookie); e == nil {
		_, err = tx.Exec(ctx, `UPDATE user_sessions SET revoked_at=clock_timestamp() WHERE session_hash=$1 AND domain_id=$2`, digest(old.Value), s.cfg.Domain)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO user_sessions(session_hash,domain_id,issuer,subject,principal_id,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, digest(session), s.cfg.Domain, s.cfg.Issuer, id.Subject, principal, id.Expiry.Add(-500*time.Millisecond))
	}
	if err == nil {
		err = s.audit(ctx, tx, principal, "identity.login", "allow", principal)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		s.fail(w, r, 503, "session commit")
		return
	}
	cookie(w, sessionCookie, session, int(time.Until(id.Expiry.Add(-500*time.Millisecond)).Seconds()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
