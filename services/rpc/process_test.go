package rpc_test

import (
	"bufio"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	icsv1 "github.com/agentnet/agentnet/proto/gen/anet/ics/v1"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	service "github.com/agentnet/agentnet/services/rpc"
)

// Starts the shipped executable with real PEM/config files and an independent
// TCP listener. The client uses the generated bindings, exactly as in-process
// protocol tests do; no test-only handler or plaintext transport is installed.
func TestRPCDaemonProcess(t *testing.T) {
	f := setup(t)
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
	c := service.FileConfig{Domain: f.domain, TrustDomain: "test.agentnet", Listen: "127.0.0.1:0", CertificateFile: "server.pem", PrivateKeyFile: "server.key", ClientCAFile: "ca.pem", ICSKeyID: "ics", ICSSigningKeyFile: "ics.key", CommandKeys: f.cfg.CommandKeys, ResultKeys: f.cfg.ResultKeys, Operations: map[string]service.OperationConfig{}}
	for target, op := range f.cfg.Operations {
		c.Operations[target] = service.OperationConfig{Action: op.Action, Resource: op.Resource, Capability: op.Capability, Purpose: op.Purpose, Dimensions: op.Dimensions, Sensitivity: op.Sensitivity, DataClass: op.DataClass}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	write("config.json", raw)
	binary := filepath.Join(dir, "anetd.exe")
	build := exec.CommandContext(f.ctx, "go", "build", "-o", binary, "../../cmd/anetd")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
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
}
