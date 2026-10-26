package identity_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func rawCredential(f *fixture, form url.Values) (int, error) {
	req, err := http.NewRequestWithContext(f.ctx, "POST", f.server.URL+"/instances", strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Origin", f.server.URL)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.browser.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
func TestIdentityConcurrentCredentialRotation(t *testing.T) {
	f := setup(t)
	enableCredentials(f)
	csrf := f.login()
	request, _ := csr(t)
	h, _ := f.credentialPost(f.credentialForm(csrf, request), 200)
	id := h.Get("X-AgentNet-Instance")
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 2)
	for n := 0; n < 2; n++ {
		request, _ := csr(t)
		form := f.credentialForm(csrf, request)
		form.Set("action", "rotate")
		form.Set("instance", id)
		form.Set("version", "1")
		go func() { status, err := rawCredential(f, form); done <- result{status, err} }()
	}
	passed, conflicts := 0, 0
	for n := 0; n < 2; n++ {
		r := <-done
		if r.err != nil {
			t.Fatal(r.err)
		}
		switch r.status {
		case 200:
			passed++
		case 409:
			conflicts++
		default:
			t.Fatal(r.status)
		}
	}
	if passed != 1 || conflicts != 1 || f.count(`SELECT count(*) FROM instance_credentials WHERE instance_id=$1`, id) != 2 || f.count(`SELECT count(*) FROM instance_credentials WHERE instance_id=$1 AND retired_at IS NULL`, id) != 1 {
		t.Fatal("concurrent rotation created multiple live credentials")
	}
}
func TestIdentityCredentialExpiryAfterLock(t *testing.T) {
	f := setup(t)
	enableCredentials(f)
	csrf := f.login()
	request, _ := csr(t)
	form := f.credentialForm(csrf, request)
	expires := time.Now().Add(time.Second)
	f.exec(`UPDATE user_sessions SET expires_at=$2 WHERE domain_id=$1`, f.domain, expires)
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, `SELECT agent_id FROM agents WHERE agent_id=$1 FOR UPDATE`, f.agent); err != nil {
		t.Fatal(err)
	}
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() { status, err := rawCredential(f, form); done <- result{status, err} }()
	deadline := time.Now().Add(5 * time.Second)
	for f.count(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'SELECT agent_id FROM agents WHERE agent_id=%'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("enrollment did not wait for agent lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(time.Until(expires) + 30*time.Millisecond)
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.status != 401 {
		t.Fatal(r)
	}
	if f.count(`SELECT count(*) FROM credential_requests WHERE domain_id=$1`, f.domain) != 0 || f.count(`SELECT count(*) FROM agent_instances WHERE agent_id=$1`, f.agent) != 1 {
		t.Fatal("expired session registered a credential")
	}
}
