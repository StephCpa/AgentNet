package rpc_test

import (
	"bufio"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentnet/agentnet/internal/pki"
	credentialsv1 "github.com/agentnet/agentnet/proto/gen/anet/credentials/v1"
	gwv1 "github.com/agentnet/agentnet/proto/gen/anet/gw/v1"
	icsv1 "github.com/agentnet/agentnet/proto/gen/anet/ics/v1"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	service "github.com/agentnet/agentnet/services/rpc"
)

// Starts the shipped executable with real PEM/config files and an independent
// TCP listener. The client uses the generated bindings, exactly as in-process
// protocol tests do; no test-only handler or plaintext transport is installed.
func TestRPCDaemonProcess(t *testing.T) {
	f := setup(t)
	addRecords(f)
	dir := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	certificate, private, err := pemFiles(f.serverCert)
	if err != nil {
		t.Fatal(err)
	}
	write("server.pem", certificate)
	write("server.key", private)
	write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.ca.Raw}))
	icsKey, err := x509.MarshalPKCS8PrivateKey(f.icsKey)
	if err != nil {
		t.Fatal(err)
	}
	write("ics.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: icsKey}))
	gwKey, err := x509.MarshalPKCS8PrivateKey(f.gwKey)
	if err != nil {
		t.Fatal(err)
	}
	write("gw.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: gwKey}))
	caKey, err := x509.MarshalPKCS8PrivateKey(f.caKey)
	if err != nil {
		t.Fatal(err)
	}
	write("ca.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: caKey}))
	c := service.FileConfig{Domain: f.domain, TrustDomain: "test.agentnet", Listen: "127.0.0.1:0", CertificateFile: "server.pem", PrivateKeyFile: "server.key", ClientCAFile: "ca.pem", ICSKeyID: "ics", ICSSigningKeyFile: "ics.key", CommandKeys: f.cfg.CommandKeys, ResultKeys: f.cfg.ResultKeys, Operations: map[string]service.OperationConfig{}}
	c.GWKeyID = "gateway"
	c.GWSigningKeyFile = "gw.key"
	c.GWCommandTTL = "5m"
	c.CredentialCAFile = "ca.pem"
	c.CredentialCAKeyFile = "ca.key"
	c.CredentialTTL = "1h"
	for target, op := range f.cfg.Operations {
		c.Operations[target] = service.OperationConfig{Action: op.Action, Resource: op.Resource, Capability: op.Capability, Purpose: op.Purpose, Dimensions: op.Dimensions, Sensitivity: op.Sensitivity, DataClass: op.DataClass}
	}
	localOp := c.Operations["records"]
	localOp.LocalHandler = "record.put/v1"
	c.Operations["records"] = localOp
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	write("config.json", raw)
	for _, name := range []string{"missing-key", "untrusted-key", "same-ics-key", "zero-ttl", "long-ttl", "unknown-local", "external-local"} {
		t.Run(name, func(t *testing.T) {
			bad := c
			bad.Operations = map[string]service.OperationConfig{}
			for k, v := range c.Operations {
				bad.Operations[k] = v
			}
			switch name {
			case "unknown-local":
				op := bad.Operations["records"]
				op.LocalHandler = "unregistered"
				bad.Operations["records"] = op
			case "external-local":
				op := bad.Operations["records"]
				op.Capability = "queryable"
				bad.Operations["records"] = op
			case "missing-key":
				bad.GWSigningKeyFile = ""
			case "untrusted-key":
				bad.GWKeyID = "untrusted"
			case "same-ics-key":
				bad.GWSigningKeyFile = "ics.key"
				bad.CommandKeys = map[string]ed25519.PublicKey{"gateway": f.icsKey.Public().(ed25519.PublicKey)}
			case "zero-ttl":
				bad.GWCommandTTL = "0s"
			case "long-ttl":
				bad.GWCommandTTL = "6m"
			}
			badRaw, e := json.Marshal(bad)
			if e != nil {
				t.Fatal(e)
			}
			write("bad-config.json", badRaw)
			server, db, _, e := service.OpenRuntime(f.ctx, filepath.Join(dir, "bad-config.json"), os.Getenv("ANET_TEST_DSN"))
			if server != nil {
				server.Stop()
			}
			if db != nil {
				db.Close()
			}
			if e == nil {
				t.Fatal("invalid GW configuration accepted")
			}
		})
	}
	binary := filepath.Join(dir, "anetd.exe")
	build := exec.CommandContext(f.ctx, "go", "build", "-o", binary, "../../cmd/anetd")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	start := func() func() {
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
		stopped := false
		stop := func() {
			if !stopped {
				stopped = true
				_ = child.Process.Kill()
				_ = child.Wait()
			}
		}
		t.Cleanup(stop)
		ready := make(chan string, 1)
		go func() {
			scanner := bufio.NewScanner(pipe)
			for scanner.Scan() {
				if _, address, ok := strings.Cut(scanner.Text(), "LISTEN "); ok {
					ready <- address
					return
				}
			}
			ready <- ""
		}()
		select {
		case f.address = <-ready:
			if f.address == "" {
				t.Fatal("daemon exited before listening")
			}
		case <-time.After(15 * time.Second):
			t.Fatal("daemon startup timed out")
		}
		return stop
	}
	stop := start()
	conn := f.connect(f.workerCert)
	f.control = tcsv1.NewTaskControlClient(conn)
	f.policy = icsv1.NewIdentityPolicyClient(conn)
	a := f.submit()
	snapshot, err := f.control.GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: a.TaskId})
	if err != nil || snapshot.GetTaskId() != a.TaskId || a.RootGrantId == "" || len(snapshot.GetBudget()) != 4 {
		t.Fatalf("daemon lifecycle: %v, %v", snapshot, err)
	}
	proposal, _ := f.proposal(a)
	if _, err = f.control.AdmitEffect(f.ctx, proposal); err != nil {
		t.Fatal(err)
	}
	savedCommand := f.command
	f.command.ID += "-local"
	f.command.ContractID += "-local"
	localRequest := recordRequest(f, f.submit())
	localResult, e := f.control.ExecuteLocalEffect(f.ctx, localRequest)
	if e != nil || localResult.GetAdmission().GetState() != "COMMITTED" {
		t.Fatal(localResult, e)
	}
	f.command = savedCommand
	f.command.ID += "-gateway"
	f.command.ContractID += "-gateway"
	r := intent(f)
	g := gwv1.NewGatewayClient(conn)
	first, err := g.SubmitCommand(f.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	saved := storedCommand(f)
	stop()
	_ = start()
	replayLocal, e := tcsv1.NewTaskControlClient(f.connect(f.workerCert)).ExecuteLocalEffect(f.ctx, localRequest)
	if e != nil || replayLocal.GetAdmission().GetOutcome() != tcsv1.EffectAdmission_EXISTING || replayLocal.GetResultRef() != localResult.ResultRef {
		t.Fatal("local result after daemon restart", replayLocal, e)
	}
	g = gwv1.NewGatewayClient(f.connect(f.workerCert))
	again, err := g.SubmitCommand(f.ctx, r)
	if err != nil || again.GetTaskId() != first.TaskId || again.GetOutcome() != tcsv1.AdmissionResult_EXISTING {
		t.Fatalf("gateway daemon restart: %v %v", again, err)
	}
	if string(saved.Payload) != string(storedCommand(f).Payload) {
		t.Fatal("daemon restart re-signed command")
	}
	issuer, err := pki.New(tls.Certificate{Certificate: [][]byte{f.ca.Raw}, PrivateKey: f.caKey}, "test.agentnet", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_ = managedCredential(t, f, issuer)
	old := credentialsv1.NewInstanceCredentialsClient(f.connect(f.workerCert))
	request, _, next := prepareRenewal(t, f, old)
	active, err := credentialsv1.NewInstanceCredentialsClient(f.connect(next)).ActivateRenewal(f.ctx, &credentialsv1.ActivateRenewalRequest{RequestId: request.RequestId})
	if err != nil || active.GetVersion() != 2 {
		t.Fatal("daemon renewal CA loading", err)
	}
}
