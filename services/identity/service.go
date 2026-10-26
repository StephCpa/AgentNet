// Package identity implements OIDC browser login and owner-controlled consent.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/agentnet/agentnet/internal/amount"
	"github.com/agentnet/agentnet/internal/authority"
	"github.com/agentnet/agentnet/internal/pki"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
)

type Profile struct {
	Name          string            `json:"name"`
	PolicyVersion string            `json:"policy_version"`
	Rights        authority.Rights  `json:"rights"`
	TaskLimits    map[string]string `json:"task_limits"`
	MaxHours      int               `json:"max_hours"`
}
type Config struct {
	Domain, Issuer, ClientID, ClientSecret, PublicURL string
	Profiles                                          map[string]Profile
	HTTPClient                                        *http.Client
	CredentialIssuer                                  *pki.Issuer
}
type Service struct {
	db       *pgxpool.Pool
	cfg      Config
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
	handler  http.Handler
}
type transactionKey struct{}

func withTransaction(r *http.Request, tx pgx.Tx) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), transactionKey{}, tx))
}

var dimensions = []string{"money", "tokens", "calls", "messages"}

func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == ""
}
func New(ctx context.Context, db *pgxpool.Pool, c Config) (*Service, error) {
	if db == nil || c.Domain == "" || c.ClientID == "" || !httpsURL(c.Issuer) || !httpsURL(c.PublicURL) || len(c.Profiles) == 0 {
		return nil, fmt.Errorf("invalid identity configuration")
	}
	public, _ := url.Parse(c.PublicURL)
	if public.Path != "" || public.RawQuery != "" {
		return nil, fmt.Errorf("public_url must be an HTTPS origin without a trailing slash")
	}
	profiles := map[string]Profile{}
	for id, p := range c.Profiles {
		if id == "" || p.Name == "" || p.PolicyVersion == "" || !authority.Valid(p.Rights, c.Domain) || p.MaxHours < 1 || p.MaxHours > 24*30 || len(p.TaskLimits) != 4 {
			return nil, fmt.Errorf("invalid consent profile")
		}
		limits := map[string]string{}
		for _, dim := range dimensions {
			if !amount.Valid(p.TaskLimits[dim]) {
				return nil, fmt.Errorf("invalid profile ceiling")
			}
			limits[dim] = p.TaskLimits[dim]
		}
		p.TaskLimits = limits
		p.Rights.DelegationScope = append([]string(nil), p.Rights.DelegationScope...)
		p.Rights.Actions = append([]string(nil), p.Rights.Actions...)
		p.Rights.Resources = append([]string(nil), p.Rights.Resources...)
		p.Rights.DataClasses = append([]string(nil), p.Rights.DataClasses...)
		p.Rights.Spatial.Domains = append([]string(nil), p.Rights.Spatial.Domains...)
		profiles[id] = p
	}
	c.Profiles = profiles
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if c.HTTPClient != nil {
		client.Transport = c.HTTPClient.Transport
	}
	ctx = oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(ctx, c.Issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery failed")
	}
	var metadata struct {
		JWKS string   `json:"jwks_uri"`
		PKCE []string `json:"code_challenge_methods_supported"`
	}
	if provider.Claims(&metadata) != nil || !httpsURL(provider.Endpoint().AuthURL) || !httpsURL(provider.Endpoint().TokenURL) || !httpsURL(metadata.JWKS) {
		return nil, fmt.Errorf("OIDC endpoints must use HTTPS")
	}
	// Requiring advertised S256 makes a misconfigured provider fail at startup.
	if !authority.Contains(metadata.PKCE, "S256") {
		return nil, fmt.Errorf("OIDC provider must advertise PKCE S256")
	}
	s := &Service{db: db, cfg: c, client: client, oauth: oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, Endpoint: provider.Endpoint(), RedirectURL: c.PublicURL + "/callback", Scopes: []string{oidc.ScopeOpenID}}, verifier: provider.Verifier(&oidc.Config{ClientID: c.ClientID, SupportedSigningAlgs: []string{"RS256", "ES256", "EdDSA"}})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.login)
	mux.HandleFunc("GET /callback", s.callback)
	mux.HandleFunc("GET /", s.page)
	mux.HandleFunc("POST /consents", s.consent)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("POST /instances", s.instances)
	s.handler = mux
	return s, nil
}
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	u, _ := url.Parse(s.cfg.PublicURL)
	if r.TLS == nil || r.Host != u.Host {
		http.Error(w, "HTTPS origin required", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	s.handler.ServeHTTP(w, r)
}
func random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func digest(v string) string { b := sha256.Sum256([]byte(v)); return hex.EncodeToString(b[:]) }
func csrf(v string) string   { return digest("anet.browser.csrf:" + v) }
func cookie(w http.ResponseWriter, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: age})
}
func (s *Service) audit(ctx context.Context, tx pgx.Tx, principal, action, decision, detail string) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_log(domain_id,principal_id,action,decision,detail_digest) VALUES($1,NULLIF($2,''),$3,$4,$5)`, s.cfg.Domain, principal, action, decision, "sha256:"+digest(detail))
	return err
}

// Denied control requests record only a fixed reason digest, never raw tokens,
// login codes, cookies, CSRs, body content or upstream error text.
func (s *Service) fail(w http.ResponseWriter, r *http.Request, status int, reason string) {
	if tx, ok := r.Context().Value(transactionKey{}).(pgx.Tx); ok {
		_ = tx.Rollback(r.Context())
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	_, err := s.db.Exec(ctx, `INSERT INTO audit_log(domain_id,action,decision,detail_digest) VALUES($1,'identity.request','deny',$2)`, s.cfg.Domain, "sha256:"+digest(reason))
	if err != nil {
		status = http.StatusServiceUnavailable
	}
	http.Error(w, http.StatusText(status), status)
}
