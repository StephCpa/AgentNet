package rpc_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/agentnet/agentnet/internal/credentialclient"
	"github.com/agentnet/agentnet/internal/pki"
	credentialsv1 "github.com/agentnet/agentnet/proto/gen/anet/credentials/v1"
	icsv1 "github.com/agentnet/agentnet/proto/gen/anet/ics/v1"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	service "github.com/agentnet/agentnet/services/rpc"
	"google.golang.org/grpc/codes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Retry only Windows' refusal to create a process while its image has a sharing
// lock. A successful Start is never retried, regardless of its eventual result.
func startRenewalProcess(t *testing.T, ctx context.Context, binary string, output *bytes.Buffer, args ...string) *exec.Cmd {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	attempts := 0
	for {
		child := exec.CommandContext(ctx, binary, args...)
		if output != nil {
			child.Stdout = output
			child.Stderr = output
		}
		err := child.Start()
		if err == nil {
			if attempts > 0 {
				t.Logf("Windows image sharing lock cleared after %d failed Start attempts; no process was created in those attempts", attempts)
			}
			return child
		}
		if child.Process != nil || runtime.GOOS != "windows" || !errors.Is(err, syscall.Errno(32)) || time.Now().After(until) {
			t.Fatal(err)
		}
		attempts++
		time.Sleep(25 * time.Millisecond)
	}
}

func TestRPCRenewalClientProcessRecovery(t *testing.T) {
	if os.Getenv("ANET_TEST_DSN") == "" {
		t.Skip("requires PostgreSQL")
	}
	binary := filepath.Join(t.TempDir(), "instancerenew.exe")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/instancerenew").CombinedOutput(); err != nil {
		t.Fatalf("build %v %s", err, out)
	}
	t.Cleanup(func() {
		until := time.Now().Add(5 * time.Second)
		for {
			err := os.Remove(binary)
			if err == nil || errors.Is(err, os.ErrNotExist) {
				return
			}
			if runtime.GOOS != "windows" || !errors.Is(err, syscall.Errno(32)) || time.Now().After(until) {
				t.Errorf("remove exited renewal client: %v", err)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	})
	for _, phase := range []string{"kill_before_activation", "fail_publication_after_activation", "watch_due"} {
		t.Run(phase, func(t *testing.T) {
			f := setup(t)
			old, _, bundle := enableRenewal(t, f)
			dir := t.TempDir()
			write := func(path string, b []byte) {
				t.Helper()
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := credentialclient.Config{Endpoint: f.address, ServerName: "localhost", ServerCAFile: filepath.Join(dir, "ca.pem"), BundleFile: filepath.Join(dir, "current.pem"), StateDir: filepath.Join(dir, "state")}
			write(cfg.ServerCAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.ca.Raw}))
			write(cfg.BundleFile, bundle)
			raw, _ := json.Marshal(cfg)
			configPath := filepath.Join(dir, "config.json")
			write(configPath, raw)
			if phase == "watch_due" {
				issuer, err := pki.New(tls.Certificate{Certificate: [][]byte{f.ca.Raw}, PrivateKey: f.caKey}, "test.agentnet", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				csr, key := renewalCSR(t)
				parsed, _ := pki.ParseCSR(csr)
				short, err := issuer.Issue(parsed, f.caller.Instance, time.Now().Add(-42*time.Second))
				if err != nil {
					t.Fatal(err)
				}
				f.exec(`UPDATE agent_instances SET cred_hash=$2,credential_version=2 WHERE instance_id=$1`, f.caller.Instance, short.Fingerprint)
				f.exec(`INSERT INTO instance_credentials(fingerprint,instance_id,version,public_key_hash,certificate_der,certificate_pem,expires_at) VALUES($1,$2,2,$3,$4,$5,$6)`, short.Fingerprint, f.caller.Instance, short.PublicKeyHash, short.DER, short.PEM, short.Expires)
				write(cfg.BundleFile, append(short.PEM, key...))
				watch := startRenewalProcess(t, f.ctx, binary, nil, "-config", configPath, "-watch")
				t.Cleanup(func() { _ = watch.Process.Kill(); _ = watch.Wait() })
				deadline := time.Now().Add(10 * time.Second)
				for {
					b, err := os.ReadFile(cfg.BundleFile)
					if err != nil {
						t.Fatal(err)
					}
					current := renewalPair(t, b, b)
					if service.Fingerprint(current.Leaf) != short.Fingerprint {
						state, err := credentialsv1.NewInstanceCredentialsClient(f.connect(current)).GetStatus(f.ctx, &credentialsv1.GetStatusRequest{})
						if err != nil || state.GetVersion() != 3 {
							t.Fatal("watch published wrong generation", err)
						}
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("watch did not renew an expiring certificate")
					}
					time.Sleep(20 * time.Millisecond)
				}
				return
			}
			name := "hold_" + strings.ReplaceAll(f.domain, "-", "")
			f.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$ BEGIN IF NEW.instance_id='%s' AND NEW.version=2 THEN PERFORM pg_advisory_xact_lock(hashtextextended('%s',0)); END IF; RETURN NEW; END $$ LANGUAGE plpgsql`, name, f.caller.Instance, name))
			f.exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON instance_credentials FOR EACH ROW EXECUTE FUNCTION %s()`, name, name))
			t.Cleanup(func() {
				f.exec(`DROP TRIGGER ` + name + ` ON instance_credentials`)
				f.exec(`DROP FUNCTION ` + name + `()`)
			})
			tx, err := f.db.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(f.ctx)
			if _, err = tx.Exec(f.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, name); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			child := startRenewalProcess(t, f.ctx, binary, &output, "-config", configPath)
			done := make(chan error, 1)
			go func() { done <- child.Wait(); close(done) }()
			t.Cleanup(func() { _ = child.Process.Kill(); <-done })
			waitForLock(t, f, "INSERT INTO instance_credentials%")
			journalPath := filepath.Join(cfg.StateDir, "pending.json")
			journalBytes, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				RequestID        string
				Key, Certificate []byte
			}
			if err = json.Unmarshal(journalBytes, &saved); err != nil || len(saved.Certificate) == 0 || len(saved.Key) == 0 {
				t.Fatal("recovery key/certificate not persisted")
			}
			if phase == "kill_before_activation" {
				if err = child.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if <-done == nil {
					t.Fatal("child was not killed")
				}
				if err = tx.Commit(f.ctx); err != nil {
					t.Fatal(err)
				}
				s, err := old.GetStatus(f.ctx, &credentialsv1.GetStatusRequest{})
				if err != nil || s.GetVersion() != 1 {
					t.Fatal("uncommitted activation lost old key", err)
				}
			} else {
				// Hold activation until the full replacement journal is durable, then make
				// local publication fail while allowing the server transaction to commit.
				if err = os.Rename(cfg.BundleFile, cfg.BundleFile+".old"); err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(cfg.BundleFile, 0700); err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(f.ctx); err != nil {
					t.Fatal(err)
				}
				if <-done == nil {
					t.Fatal("publication should have failed")
				}
				if err = os.Remove(cfg.BundleFile); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(cfg.BundleFile+".old", cfg.BundleFile); err != nil {
					t.Fatal(err)
				}
				_, err = old.GetStatus(f.ctx, &credentialsv1.GetStatusRequest{})
				code(t, err, codes.Unauthenticated)
				f.exec(`UPDATE credential_renewals SET prepare_expires_at=clock_timestamp()-interval '1 second' WHERE instance_id=$1`, f.caller.Instance)
			}
			var recoveredOutput bytes.Buffer
			retry := startRenewalProcess(t, f.ctx, binary, &recoveredOutput, "-config", configPath)
			if err := retry.Wait(); err != nil {
				t.Fatalf("restart recovery %v %s", err, recoveredOutput.String())
			}
			if _, err = os.Stat(journalPath); !os.IsNotExist(err) {
				t.Fatal("completed journal not cleared")
			}
			current, err := os.ReadFile(cfg.BundleFile)
			if err != nil {
				t.Fatal(err)
			}
			next := renewalPair(t, current, current)
			expected := renewalPair(t, saved.Certificate, saved.Key)
			if service.Fingerprint(next.Leaf) != service.Fingerprint(expected.Leaf) {
				t.Fatal("restart replaced pending key instead of recovering it")
			}
			conn := f.connect(next)
			client := credentialsv1.NewInstanceCredentialsClient(conn)
			state, err := client.GetStatus(f.ctx, &credentialsv1.GetStatusRequest{})
			if err != nil || state.GetVersion() != 2 {
				t.Fatal("renewed key not current", err)
			}
			f.control = tcsv1.NewTaskControlClient(conn)
			f.policy = icsv1.NewIdentityPolicyClient(conn)
			if out := f.submit(); out.GetTaskId() == "" {
				t.Fatal("renewed credential cannot accept work")
			}
		})
	}
}

func TestRPCRenewalClientExpiredPreparationRecovery(t *testing.T) {
	f := setup(t)
	old, _, bundle := enableRenewal(t, f)
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.ca.Raw})
	cfg := credentialclient.Config{Endpoint: f.address, ServerName: "localhost", ServerCAFile: filepath.Join(dir, "ca.pem"), BundleFile: filepath.Join(dir, "current.pem"), StateDir: filepath.Join(dir, "state")}
	write := func(path string, b []byte) {
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(cfg.ServerCAFile, ca)
	write(cfg.BundleFile, bundle)
	if err := os.Mkdir(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	csr, key := renewalCSR(t)
	r := &credentialsv1.PrepareRenewalRequest{RequestId: renewalID(), ExpectedVersion: 1, CsrPem: csr}
	out, err := old.PrepareRenewal(f.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE credential_renewals SET prepare_expires_at=clock_timestamp()-interval '1 second' WHERE instance_id=$1`, f.caller.Instance)
	raw, _ := json.Marshal(map[string]any{"Endpoint": cfg.Endpoint, "ServerName": cfg.ServerName, "TrustHash": pki.Fingerprint(ca), "Instance": f.caller.Instance, "RequestID": r.RequestId, "BaseFingerprint": service.Fingerprint(f.workerCert.Leaf), "Version": int64(1), "Key": key, "CSR": csr, "Certificate": out.CertificatePem, "ActivateBefore": time.Now().Add(-time.Second)})
	write(filepath.Join(cfg.StateDir, "pending.json"), raw)
	err = credentialclient.Renew(f.ctx, cfg)
	if !errors.Is(err, credentialclient.ErrAbandoned) {
		t.Fatal("expired preparation did not abandon safely", err)
	}
	if err = credentialclient.Renew(f.ctx, cfg); err != nil {
		t.Fatal("fresh attempt after abandonment", err)
	}
	if f.count(`SELECT count(*) FROM credential_renewals WHERE instance_id=$1 AND status='superseded'`, f.caller.Instance) != 1 || f.count(`SELECT count(*) FROM credential_renewals WHERE instance_id=$1 AND status='active'`, f.caller.Instance) != 1 {
		t.Fatal("incorrect recovery lifecycle")
	}
}
