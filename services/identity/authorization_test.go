package identity_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agentnet/agentnet/internal/signed"
	"github.com/agentnet/agentnet/services/ics"
	"github.com/agentnet/agentnet/services/tcs"
)

func TestIdentityConsentDrivesRootAcceptance(t *testing.T) {
	for _, withdraw := range []bool{false, true} {
		name := "approved"
		if withdraw {
			name = "withdrawn-after-permit"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			csrf := f.login()
			h := f.post("/consents", f.form(csrf), f.server.URL, 303)
			consent := h.Get("X-AgentNet-Consent")
			pub, key, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			keys := map[string]ed25519.PublicKey{"fixture": pub}
			ops := map[string]tcs.Operation{"bank": {Action: "pay", Resource: "account:test", Capability: "queryable", Purpose: "tool_call", Dimensions: []string{"money", "tokens", "calls", "messages"}, Sensitivity: "sensitive", DataClass: "internal"}}
			store, err := tcs.New(f.db, tcs.Config{Domain: f.domain, CommandKeys: keys, RootKeys: keys, DecisionKeys: keys, ResultKeys: keys, Operations: ops})
			if err != nil {
				t.Fatal(err)
			}
			policy, err := ics.New(f.db, ics.Config{Domain: f.domain, KeyID: "fixture", Key: key, CommandKeys: keys, Operations: ops})
			if err != nil {
				t.Fatal(err)
			}
			actor := tcs.Caller{Principal: f.user, Agent: f.agent, Instance: f.instance}
			contract, _ := json.Marshal(map[string]any{"deadline": time.Now().UTC().Add(time.Hour), "input_refs": []any{map[string]any{"version": 1, "digest": "sha256:" + strings.Repeat("a", 64)}}, "output_schema": map[string]string{"type": "object"}, "acceptance_policy": map[string]any{"verifier": "fixture", "checks": []string{"schema"}, "on_fail": "report"}})
			cmd := tcs.Command{ID: "command-" + f.domain, Audience: f.domain, NotAfter: time.Now().UTC().Add(time.Hour), ContractID: "contract-" + f.domain, ContractVersion: 1, Contract: contract, Limits: map[string]string{"money": "5", "tokens": "50", "calls": "10", "messages": "10"}, Root: &tcs.RootRequest{ConsentID: consent, Rights: f.cfg.Profiles["research"].Rights}}
			command, err := signed.Encode("fixture", key, tcs.Submission{Kind: "anet.command/1", Caller: actor, Command: cmd})
			if err != nil {
				t.Fatal(err)
			}
			permit, err := policy.IssueRoot(f.ctx, actor, command)
			if err != nil {
				t.Fatal(err)
			}
			if withdraw {
				f.post("/consents", url.Values{"csrf": {csrf}, "action": {"revoke"}, "consent": {consent}, "version": {"1"}}, f.server.URL, 303)
			}
			accepted, err := store.SubmitAuthorized(f.ctx, actor, command, permit)
			if withdraw {
				if !errors.Is(err, tcs.Denied) || f.count(`SELECT count(*) FROM tasks WHERE domain_id=$1`, f.domain) != 0 {
					t.Fatal("withdrawn consent created task", err)
				}
			} else if err != nil || accepted.RootGrantID == "" || f.count(`SELECT count(*) FROM grants WHERE grant_id=$1 AND consent_id=$2 AND consent_version=1`, accepted.RootGrantID, consent) != 1 {
				t.Fatal("approved consent did not create root", err)
			}
		})
	}
}

func TestIdentityCodeInjectionAndStateExpiry(t *testing.T) {
	for _, mode := range []string{"code-injection", "state-expiry", "state-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			callback := f.begin()
			u, _ := url.Parse(callback)
			q := u.Query()
			switch mode {
			case "code-injection":
				// The injected code was issued for a different PKCE transaction.
				f.provider.mu.Lock()
				f.provider.codes["injected"] = authorization{Nonce: "different", Challenge: "different", Redirect: f.provider.redirect}
				f.provider.mu.Unlock()
				q.Set("code", "injected")
			case "state-expiry":
				f.exec(`UPDATE oidc_logins SET expires_at=clock_timestamp()-interval '1 second' WHERE domain_id=$1`, f.domain)
			case "state-mismatch":
				q.Set("state", tokenString())
			}
			u.RawQuery = q.Encode()
			f.get(u.String(), 401)
			if f.count(`SELECT count(*) FROM user_sessions WHERE domain_id=$1`, f.domain) != 0 {
				t.Fatal("invalid callback created session")
			}
		})
	}
}
