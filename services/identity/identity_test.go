package identity_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentnet/agentnet/internal/authority"
	"github.com/agentnet/agentnet/services/identity"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

type authorization struct{ Nonce, Challenge, Redirect string }
type issuer struct {
	server         *httptest.Server
	key            *rsa.PrivateKey
	mu             sync.Mutex
	codes          map[string]authorization
	mode, redirect string
}

func tokenString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
func (p *issuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		json.NewEncoder(w).Encode(map[string]any{"issuer": p.server.URL, "authorization_endpoint": p.server.URL + "/authorize", "token_endpoint": p.server.URL + "/token", "jwks_uri": p.server.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}, "code_challenge_methods_supported": []string{"S256"}})
	case "/keys":
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "issuer", Algorithm: "RS256", Use: "sig"}}})
	case "/authorize":
		q := r.URL.Query()
		if q.Get("response_type") != "code" || q.Get("client_id") != "client" || q.Get("redirect_uri") != p.redirect || q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" || q.Get("scope") != "openid" {
			http.Error(w, "invalid authorization request", 400)
			return
		}
		code := tokenString()
		p.mu.Lock()
		p.codes[code] = authorization{q.Get("nonce"), q.Get("code_challenge"), q.Get("redirect_uri")}
		p.mu.Unlock()
		http.Redirect(w, r, p.redirect+"?"+url.Values{"code": {code}, "state": {q.Get("state")}}.Encode(), 303)
	case "/token":
		r.ParseForm()
		client, secret, _ := r.BasicAuth()
		if client != "client" || secret != "fixture-secret" {
			http.Error(w, "invalid client", 401)
			return
		}
		p.mu.Lock()
		a, ok := p.codes[r.Form.Get("code")]
		delete(p.codes, r.Form.Get("code"))
		p.mu.Unlock()
		hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || r.Form.Get("grant_type") != "authorization_code" || a.Redirect != r.Form.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(hash[:]) != a.Challenge {
			http.Error(w, "invalid code or verifier", 400)
			return
		}
		now := time.Now().Unix()
		claims := map[string]any{"iss": p.server.URL, "sub": "alice", "aud": "client", "iat": now, "exp": now + 3500, "nonce": a.Nonce, "email": "alice@example.test"}
		switch p.mode {
		case "nonce":
			claims["nonce"] = "wrong"
		case "issuer":
			claims["iss"] = "https://different.test"
		case "audience":
			claims["aud"] = "different"
		case "expired":
			claims["exp"] = now - 1
		case "future-issued":
			claims["iat"] = now + 60
		case "long-lived":
			claims["exp"] = now + 7200
		case "azp":
			claims["aud"] = []string{"client", "another"}
			claims["azp"] = "another"
		case "at-hash":
			claims["at_hash"] = "wrong"
		case "unmapped":
			claims["sub"] = "mallory"
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithHeader("kid", "issuer"))
		data, _ := json.Marshal(claims)
		object, _ := signer.Sign(data)
		raw, _ := object.CompactSerialize()
		if p.mode == "signature" {
			parts := strings.Split(raw, ".")
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			sig[0] ^= 1
			parts[2] = base64.RawURLEncoding.EncodeToString(sig)
			raw = strings.Join(parts, ".")
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access-token", "token_type": "Bearer", "expires_in": 3500, "id_token": raw})
	default:
		http.NotFound(w, r)
	}
}

type fixture struct {
	t                             *testing.T
	ctx                           context.Context
	db                            *pgxpool.Pool
	cfg                           identity.Config
	provider                      *issuer
	server                        *httptest.Server
	browser                       *http.Client
	domain, user, agent, instance string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	if os.Getenv("ANET_TEST_DSN") == "" {
		t.Skip("requires isolated schema gate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	db, err := pgxpool.New(ctx, os.Getenv("ANET_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	id := tokenString()
	f := &fixture{t: t, ctx: ctx, db: db, domain: "domain-" + id, user: "user-" + id, agent: "agent-" + id, instance: "instance-" + id}
	f.exec(`INSERT INTO domains(domain_id,name,gateway_endpoint,trust_anchor) VALUES($1,'fixture','https://unused.test','{}')`, f.domain)
	f.exec(`INSERT INTO principals(principal_id,kind,domain_id) VALUES($1,'user',$2)`, f.user, f.domain)
	f.exec(`INSERT INTO agents(agent_id,domain_id,owner_principal_id,handle,card_digest) VALUES($1,$2,$3,'Research <Agent>','fixture')`, f.agent, f.domain, f.user)
	f.exec(`INSERT INTO agent_instances(instance_id,agent_id,cred_hash) VALUES($1,$2,'fixture')`, f.instance, f.agent)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &issuer{key: key, codes: map[string]authorization{}}
	p.server = httptest.NewUnstartedServer(p)
	p.server.StartTLS()
	t.Cleanup(p.server.Close)
	f.provider = p
	f.server = httptest.NewUnstartedServer(nil)
	f.cfg = identity.Config{Domain: f.domain, Issuer: p.server.URL, ClientID: "client", ClientSecret: "fixture-secret", PublicURL: "https://" + f.server.Listener.Addr().String(), HTTPClient: p.server.Client(), Profiles: map[string]identity.Profile{"research": {Name: "Research", PolicyVersion: "policy/1", MaxHours: 24, TaskLimits: map[string]string{"money": "10", "tokens": "100", "calls": "20", "messages": "20"}, Rights: authority.Rights{Actions: []string{"pay"}, Resources: []string{"account:test"}, DataClasses: []string{"internal"}, Spatial: authority.Spatial{Domains: []string{f.domain}, EgressOnlyViaGateway: true}}}}}
	p.redirect = f.cfg.PublicURL + "/callback"
	f.exec(`INSERT INTO oidc_subjects(domain_id,issuer,subject,principal_id) VALUES($1,$2,'alice',$3)`, f.domain, p.server.URL, f.user)
	service, err := identity.New(ctx, db, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.server.Config.Handler = service
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	jar, _ := cookiejar.New(nil)
	f.browser = &http.Client{Transport: f.server.Client().Transport, Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return f
}
func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}
func (f *fixture) get(raw string, want int) (http.Header, string) {
	f.t.Helper()
	resp, err := f.browser.Get(raw)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		f.t.Fatalf("status %d want %d: %s", resp.StatusCode, want, body)
	}
	return resp.Header, string(body)
}
func (f *fixture) begin() string {
	f.t.Helper()
	h, _ := f.get(f.server.URL+"/login", 303)
	h, _ = f.get(h.Get("Location"), 303)
	return h.Get("Location")
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (f *fixture) login() string {
	f.t.Helper()
	h, _ := f.get(f.begin(), 303)
	if !strings.Contains(h.Get("Set-Cookie"), "Secure") || !strings.Contains(h.Get("Set-Cookie"), "HttpOnly") {
		f.t.Fatal("missing secure session cookie")
	}
	_, body := f.get(f.server.URL+"/", 200)
	matches := csrfRE.FindStringSubmatch(body)
	if len(matches) != 2 {
		f.t.Fatal("no CSRF token", body)
	}
	return matches[1]
}
func (f *fixture) form(csrf string) url.Values {
	return url.Values{"action": {"create"}, "csrf": {csrf}, "profile": {"research"}, "agent": {f.agent}, "instance": {f.instance}, "hours": {"2"}, "money": {"5"}, "tokens": {"50"}, "calls": {"10"}, "messages": {"10"}}
}
func (f *fixture) post(path string, form url.Values, origin string, want int) http.Header {
	f.t.Helper()
	req, _ := http.NewRequest("POST", f.server.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	resp, err := f.browser.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		f.t.Fatalf("POST %s status %d want %d: %s", path, resp.StatusCode, want, body)
	}
	return resp.Header
}

func TestIdentityLoginConsentAndLogout(t *testing.T) {
	f := setup(t)
	csrf := f.login()
	_, body := f.get(f.server.URL+"/", 200)
	if strings.Contains(body, "Research <Agent>") || !strings.Contains(body, "Research &lt;Agent&gt;") {
		t.Fatal("unsafe agent label")
	}
	h := f.post("/consents", f.form(csrf), f.server.URL, 303)
	id := h.Get("X-AgentNet-Consent")
	if id == "" {
		t.Fatal("missing consent")
	}
	if f.count(`SELECT count(*) FROM root_consents WHERE consent_id=$1 AND subject_principal_id=$2 AND actor_instance_id=$3 AND task_limits->>'money'='5' AND rights->'actions' ? 'pay'`, id, f.user, f.instance) != 1 {
		t.Fatal("consent binding mismatch")
	}
	f.post("/consents", url.Values{"csrf": {csrf}, "action": {"revoke"}, "consent": {id}, "version": {"1"}}, f.server.URL, 303)
	if f.count(`SELECT count(*) FROM root_consents WHERE consent_id=$1 AND version=2 AND revoked_at IS NOT NULL`, id) != 1 {
		t.Fatal("revoke not versioned")
	}
	f.post("/logout", url.Values{"csrf": {csrf}}, f.server.URL, 303)
	f.post("/consents", f.form(csrf), f.server.URL, 403)
	if f.count(`SELECT count(*) FROM user_sessions WHERE domain_id=$1 AND revoked_at IS NULL`, f.domain) != 0 {
		t.Fatal("logout left live session")
	}
	if f.count(`SELECT count(*) FROM audit_log WHERE domain_id=$1 AND action IN ('identity.login','consent.create','consent.revoke','identity.logout') AND decision='allow'`, f.domain) != 4 {
		t.Fatal("missing successful audit")
	}
}
func TestIdentityIDTokenClaimsAndSignatures(t *testing.T) {
	for _, mode := range []string{"nonce", "issuer", "audience", "expired", "future-issued", "long-lived", "azp", "at-hash", "signature", "unmapped"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			f.provider.mode = mode
			want := 401
			if mode == "unmapped" {
				want = 403
			}
			f.get(f.begin(), want)
			if f.count(`SELECT count(*) FROM user_sessions WHERE domain_id=$1`, f.domain) != 0 {
				t.Fatal("bad identity created session")
			}
		})
	}
}
func TestIdentityCallbackReplayAndBrowserBinding(t *testing.T) {
	f := setup(t)
	callback := f.begin()
	origin, _ := url.Parse(f.server.URL)
	loginCookies := f.browser.Jar.Cookies(origin)
	jar := f.browser.Jar
	f.browser.Jar, _ = cookiejar.New(nil)
	f.get(callback, 400)
	f.browser.Jar = jar
	f.get(callback, 303)
	// Restore the old browser transaction cookie: the database consumption,
	// rather than cookie clearing alone, must prevent replay.
	f.browser.Jar.SetCookies(origin, loginCookies)
	f.get(callback, 401)
	if f.count(`SELECT count(*) FROM user_sessions WHERE domain_id=$1`, f.domain) != 1 {
		t.Fatal("callback created duplicate sessions")
	}
}
func TestIdentityConsentCSRFAndOwnership(t *testing.T) {
	f := setup(t)
	csrf := f.login()
	for _, mode := range []string{"origin", "csrf", "agent", "instance", "budget", "hours", "profile", "unknown", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			form := f.form(csrf)
			origin := f.server.URL
			want := 400
			switch mode {
			case "origin":
				origin = "https://attacker.test"
				want = 403
			case "csrf":
				form.Set("csrf", "bad")
				want = 403
			case "agent":
				form.Set("agent", "not-owned")
				want = 403
			case "instance":
				form.Set("instance", "not-owned")
				want = 403
			case "budget":
				form.Set("money", "11")
			case "hours":
				form.Set("hours", "25")
			case "profile":
				form.Set("profile", "admin")
			case "unknown":
				form.Set("subject", "admin")
				want = 403
			case "duplicate":
				form.Add("money", "1")
				want = 403
			}
			f.post("/consents", form, origin, want)
		})
	}
	if f.count(`SELECT count(*) FROM root_consents WHERE domain_id=$1`, f.domain) != 0 {
		t.Fatal("invalid consent persisted")
	}
	if f.count(`SELECT count(*) FROM audit_log WHERE domain_id=$1 AND decision='deny'`, f.domain) != 9 {
		t.Fatal("denials not audited")
	}
}
func TestIdentitySessionRevocationAndRemapping(t *testing.T) {
	for _, mode := range []string{"principal", "session", "expiry", "mapping"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			csrf := f.login()
			switch mode {
			case "principal":
				f.exec(`UPDATE principals SET revoked_at=clock_timestamp() WHERE principal_id=$1`, f.user)
			case "session":
				f.exec(`UPDATE user_sessions SET revoked_at=clock_timestamp() WHERE domain_id=$1`, f.domain)
			case "expiry":
				f.exec(`UPDATE user_sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE domain_id=$1`, f.domain)
			case "mapping":
				f.exec(`DELETE FROM oidc_subjects WHERE domain_id=$1`, f.domain)
			}
			f.post("/consents", f.form(csrf), f.server.URL, 403)
			if f.count(`SELECT count(*) FROM root_consents WHERE domain_id=$1`, f.domain) != 0 {
				t.Fatal("dead session authorized")
			}
		})
	}
}
