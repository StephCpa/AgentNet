package identity_test

import (
	"bufio"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentnet/agentnet/services/identity"
)

func TestIdentityDaemonProcess(t *testing.T) {
	f := setup(t)
	dir := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	certificate := f.server.TLS.Certificates[0]
	private, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	write("server.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}))
	write("server.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
	write("issuer-ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.provider.server.Certificate().Raw}))
	write("client-secret", []byte("fixture-secret"))
	origin, _ := url.Parse(f.cfg.PublicURL)
	cfg := identity.FileConfig{Domain: f.domain, Listen: origin.Host, PublicURL: f.cfg.PublicURL, Issuer: f.cfg.Issuer, ClientID: "client", ClientSecretFile: "client-secret", IssuerCAFile: "issuer-ca.pem", CertificateFile: "server.pem", PrivateKeyFile: "server.key", Profiles: f.cfg.Profiles}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	write("config.json", raw)
	binary := filepath.Join(dir, "identityd.exe")
	build := exec.CommandContext(f.ctx, "go", "build", "-o", binary, "../../cmd/identityd")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	// Replace the in-process listener with the shipped binary at the registered
	// callback origin. The IdP remains an independent HTTPS server.
	f.server.Close()
	child := exec.CommandContext(f.ctx, binary, "-config", filepath.Join(dir, "config.json"))
	child.Env = append(os.Environ(), "ANET_DATABASE_URL="+os.Getenv("ANET_TEST_DSN"))
	pipe, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = child.Stdout
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "LISTEN ") {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("identityd exited before listening")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("identityd startup timed out")
	}
	csrf := f.login()
	h := f.post("/consents", f.form(csrf), f.server.URL, 303)
	if f.count(`SELECT count(*) FROM root_consents WHERE consent_id=$1 AND subject_principal_id=$2`, h.Get("X-AgentNet-Consent"), f.user) != 1 {
		t.Fatal("daemon failed to create consent")
	}
}
