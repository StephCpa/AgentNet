package identity_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agentnet/agentnet/internal/pki"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	"github.com/agentnet/agentnet/services/ics"
	"github.com/agentnet/agentnet/services/identity"
	rpcservice "github.com/agentnet/agentnet/services/rpc"
	"github.com/agentnet/agentnet/services/tcs"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

func enableCredentials(f *fixture) tls.Certificate {
	f.t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "workload test CA"}, IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		f.t.Fatal(err)
	}
	ca := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	issuer, err := pki.New(ca, "test.agentnet", time.Hour)
	if err != nil {
		f.t.Fatal(err)
	}
	f.cfg.CredentialIssuer = issuer
	f.server.Close()
	f.server = httptest.NewUnstartedServer(nil)
	f.cfg.PublicURL = "https://" + f.server.Listener.Addr().String()
	f.provider.redirect = f.cfg.PublicURL + "/callback"
	service, err := identity.New(f.ctx, f.db, f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	f.server.Config.Handler = service
	f.server.StartTLS()
	f.t.Cleanup(f.server.Close)
	f.browser.Transport = f.server.Client().Transport
	return ca
}
func csr(t *testing.T) (string, []byte) {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	raw, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "admin"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: raw})), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
func (f *fixture) credentialForm(csrf, request string) url.Values {
	return url.Values{"action": {"enroll"}, "csrf": {csrf}, "agent": {f.agent}, "csr": {request}, "request_id": {tokenString()}}
}
func (f *fixture) credentialPost(form url.Values, want int) (http.Header, []byte) {
	f.t.Helper()
	req, _ := http.NewRequestWithContext(f.ctx, "POST", f.server.URL+"/instances", strings.NewReader(form.Encode()))
	req.Header.Set("Origin", f.server.URL)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.browser.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		f.t.Fatalf("credential status %d want %d: %s", resp.StatusCode, want, body)
	}
	return resp.Header, body
}
func network(f *fixture, ca tls.Certificate) func([]byte, []byte) tcsv1.TaskControlClient {
	f.t.Helper()
	roots := x509.NewCertPool()
	leaf, _ := x509.ParseCertificate(ca.Certificate[0])
	roots.AddCert(leaf)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	keys := map[string]ed25519.PublicKey{"fixture": pub}
	ops := map[string]tcs.Operation{"bank": {Action: "pay", Resource: "account:test", Capability: "queryable", Purpose: "tool_call", Dimensions: []string{"money"}, Sensitivity: "sensitive", DataClass: "internal"}}
	store, err := tcs.New(f.db, tcs.Config{Domain: f.domain, CommandKeys: keys, RootKeys: keys, DecisionKeys: keys, ResultKeys: keys, Operations: ops})
	if err != nil {
		f.t.Fatal(err)
	}
	policy, err := ics.New(f.db, ics.Config{Domain: f.domain, KeyID: "fixture", Key: key, CommandKeys: keys, Operations: ops})
	if err != nil {
		f.t.Fatal(err)
	}
	server, err := rpcservice.New(f.db, store, policy, rpcservice.Config{Domain: f.domain, TrustDomain: "test.agentnet", Certificate: f.server.TLS.Certificates[0], ClientCAs: roots, Operations: ops})
	if err != nil {
		f.t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.t.Fatal(err)
	}
	go server.Serve(listener)
	f.t.Cleanup(server.Stop)
	serverRoots := x509.NewCertPool()
	serverRoots.AddCert(f.server.Certificate())
	return func(cert, key []byte) tcsv1.TaskControlClient {
		f.t.Helper()
		pair, err := tls.X509KeyPair(cert, key)
		if err != nil {
			f.t.Fatal(err)
		}
		conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: serverRoots, Certificates: []tls.Certificate{pair}})))
		if err != nil {
			f.t.Fatal(err)
		}
		f.t.Cleanup(func() { conn.Close() })
		return tcsv1.NewTaskControlClient(conn)
	}
}
func assertAuthenticated(t *testing.T, f *fixture, client tcsv1.TaskControlClient, want codes.Code) {
	t.Helper()
	_, err := client.GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: "nonexistent"})
	if status.Code(err) != want {
		t.Fatalf("authentication status want %v got %v", want, err)
	}
}
func TestIdentityCredentialLifecycleAndMTLS(t *testing.T) {
	f := setup(t)
	ca := enableCredentials(f)
	csrf := f.login()
	request, key := csr(t)
	form := f.credentialForm(csrf, request)
	h, cert := f.credentialPost(form, 200)
	id := h.Get("X-AgentNet-Instance")
	if !strings.HasPrefix(id, "ins_") || h.Get("X-AgentNet-Credential-Version") != "1" {
		t.Fatal(h)
	}
	replay, same := f.credentialPost(form, 200)
	if !bytes.Equal(cert, same) || replay.Get("X-AgentNet-Existing") != "true" {
		t.Fatal("issuance retry changed certificate")
	}
	if f.count(`SELECT count(*) FROM instance_roles WHERE instance_id=$1`, id) != 0 {
		t.Fatal("enrollment created privileged role")
	}
	connect := network(f, ca)
	old := connect(cert, key)
	assertAuthenticated(t, f, old, codes.PermissionDenied)
	_, err := old.ReportEffectResult(f.ctx, &tcsv1.ReportEffectResultRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatal("worker gained adapter role", err)
	}
	request2, key2 := csr(t)
	rotate := f.credentialForm(csrf, request2)
	rotate.Set("action", "rotate")
	rotate.Set("instance", id)
	rotate.Set("version", "1")
	h, cert2 := f.credentialPost(rotate, 200)
	if h.Get("X-AgentNet-Instance") != id || h.Get("X-AgentNet-Credential-Version") != "2" {
		t.Fatal("rotation changed instance or not version", h)
	}
	assertAuthenticated(t, f, old, codes.Unauthenticated)
	current := connect(cert2, key2)
	assertAuthenticated(t, f, current, codes.PermissionDenied)
	f.credentialPost(form, 409)
	if f.count(`SELECT count(*) FROM instance_credentials WHERE instance_id=$1 AND retired_at IS NOT NULL`, id) != 1 {
		t.Fatal("old credential not retired")
	}
	f.credentialPost(url.Values{"csrf": {csrf}, "action": {"revoke"}, "instance": {id}, "version": {"2"}}, 303)
	assertAuthenticated(t, f, current, codes.Unauthenticated)
	f.credentialPost(rotate, 409)
	if f.count(`SELECT count(*) FROM instance_credentials WHERE instance_id=$1 AND retired_at IS NOT NULL`, id) != 2 {
		t.Fatal("revocation did not retire active cert")
	}
}
func TestIdentityCredentialReplayAndKeyIsolation(t *testing.T) {
	f := setup(t)
	enableCredentials(f)
	csrf := f.login()
	request, _ := csr(t)
	form := f.credentialForm(csrf, request)
	h, _ := f.credentialPost(form, 200)
	id := h.Get("X-AgentNet-Instance")
	changed, _ := csr(t)
	form.Set("csr", changed)
	f.credentialPost(form, 409)
	form = f.credentialForm(csrf, request)
	f.credentialPost(form, 409)
	form.Set("action", "rotate")
	form.Set("instance", id)
	form.Set("version", "1")
	f.credentialPost(form, 409)
	if f.count(`SELECT count(*) FROM instance_credentials WHERE instance_id=$1`, id) != 1 || f.count(`SELECT credential_version::int FROM agent_instances WHERE instance_id=$1`, id) != 1 {
		t.Fatal("failed key reuse altered credential")
	}
	form = f.credentialForm(csrf, changed)
	form.Set("agent", "foreign")
	f.credentialPost(form, 403)
	form.Set("agent", f.agent)
	form.Set("role", "adapter")
	f.credentialPost(form, 403)
	form.Del("role")
	form.Set("csrf", "wrong")
	f.credentialPost(form, 403)
}
func TestIdentityCredentialAuditRollback(t *testing.T) {
	f := setup(t)
	enableCredentials(f)
	csrf := f.login()
	request, _ := csr(t)
	name := "fail_credential_" + strings.ToLower(strings.ReplaceAll(f.domain[len("domain-"):], "-", ""))
	name = strings.ReplaceAll(name, "_", "")
	if len(name) > 55 {
		name = name[:55]
	}
	// Constraint is domain-scoped and removed before the fixture shuts down.
	f.exec(`ALTER TABLE audit_log ADD CONSTRAINT ` + name + ` CHECK(NOT(domain_id='` + f.domain + `' AND action='instance.enroll')) NOT VALID`)
	defer f.exec(`ALTER TABLE audit_log DROP CONSTRAINT ` + name)
	f.credentialPost(f.credentialForm(csrf, request), 503)
	if f.count(`SELECT count(*) FROM agent_instances i JOIN agents a USING(agent_id) WHERE a.domain_id=$1`, f.domain) != 1 || f.count(`SELECT count(*) FROM credential_requests WHERE domain_id=$1`, f.domain) != 0 {
		t.Fatal("credential survived failed audit")
	}
}
