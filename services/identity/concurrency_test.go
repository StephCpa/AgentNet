package identity_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestIdentitySessionExpiryAfterAgentLock(t *testing.T) {
	f := setup(t)
	csrf := f.login()
	expires := time.Now().UTC().Add(time.Second)
	f.exec(`UPDATE user_sessions SET expires_at=$2 WHERE domain_id=$1`, f.domain, expires)
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, `SELECT agent_id FROM agents WHERE agent_id=$1 FOR UPDATE`, f.agent); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(f.ctx, "POST", f.server.URL+"/consents", strings.NewReader(f.form(csrf).Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", f.server.URL)
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		resp, e := f.browser.Do(req)
		if e != nil {
			done <- result{err: e}
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		done <- result{status: resp.StatusCode}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.count(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'SELECT owner_principal_id FROM agents WHERE agent_id=%'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("request never waited on agent lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(time.Until(expires) + 30*time.Millisecond)
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.status != 401 {
		t.Fatal(got)
	}
	if f.count(`SELECT count(*) FROM root_consents WHERE domain_id=$1`, f.domain) != 0 {
		t.Fatal("expired session committed consent after lock wait")
	}
}

func TestIdentityCrossOwnerAndConsentVersion(t *testing.T) {
	f := setup(t)
	csrf := f.login()
	other := "other-" + f.user
	agent := "other-" + f.agent
	f.exec(`INSERT INTO principals(principal_id,kind,domain_id) VALUES($1,'user',$2)`, other, f.domain)
	f.exec(`INSERT INTO agents(agent_id,domain_id,owner_principal_id,handle,card_digest) VALUES($1,$2,$3,'other','fixture')`, agent, f.domain, other)
	form := f.form(csrf)
	form.Set("agent", agent)
	form.Set("instance", "")
	f.post("/consents", form, f.server.URL, 403)
	h := f.post("/consents", f.form(csrf), f.server.URL, 303)
	id := h.Get("X-AgentNet-Consent")
	f.exec(`INSERT INTO root_consents(consent_id,domain_id,subject_principal_id,actor_agent_id,rights,task_limits,not_before,expires_at,policy_version) SELECT 'other-'||consent_id,domain_id,$2,$3,rights,task_limits,not_before,expires_at,policy_version FROM root_consents WHERE consent_id=$1`, id, other, agent)
	f.post("/consents", url.Values{"action": {"revoke"}, "csrf": {csrf}, "consent": {"other-" + id}, "version": {"1"}}, f.server.URL, 409)
	if f.count(`SELECT count(*) FROM root_consents WHERE consent_id=$1 AND revoked_at IS NULL AND version=1`, "other-"+id) != 1 {
		t.Fatal("foreign consent changed")
	}
	revoke := url.Values{"action": {"revoke"}, "csrf": {csrf}, "consent": {id}, "version": {"1"}}
	f.post("/consents", revoke, f.server.URL, 303)
	f.post("/consents", revoke, f.server.URL, 409)
	if f.count(`SELECT count(*) FROM root_consents WHERE consent_id=$1 AND version=2`, id) != 1 {
		t.Fatal("stale revoke changed version")
	}
}
