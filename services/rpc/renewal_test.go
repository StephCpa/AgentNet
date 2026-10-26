package rpc_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"github.com/agentnet/agentnet/internal/pki"
	credentialsv1 "github.com/agentnet/agentnet/proto/gen/anet/credentials/v1"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	credentialservice "github.com/agentnet/agentnet/services/credentials"
	service "github.com/agentnet/agentnet/services/rpc"
	"google.golang.org/grpc/codes"
	"net"
	"strings"
	"testing"
	"time"
)

func renewalCSR(t *testing.T) ([]byte, []byte) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
func renewalID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
func renewalPair(t *testing.T, cert, key []byte) tls.Certificate {
	t.Helper()
	p, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	p.Leaf, err = x509.ParseCertificate(p.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func enableRenewal(t *testing.T, f *fixture) (credentialsv1.InstanceCredentialsClient, *pki.Issuer, []byte) {
	t.Helper()
	issuer, err := pki.New(tls.Certificate{Certificate: [][]byte{f.ca.Raw}, PrivateKey: f.caKey}, "test.agentnet", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bundle := managedCredential(t, f, issuer)
	cs, err := credentialservice.New(f.db, f.domain, issuer)
	if err != nil {
		t.Fatal(err)
	}
	server, err := service.New(f.db, f.store, f.ics, service.Config{Domain: f.domain, TrustDomain: "test.agentnet", Certificate: f.serverCert, ClientCAs: f.roots, Operations: f.cfg.Operations, Credentials: cs})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.address = listener.Addr().String()
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	f.conn = f.connect(f.workerCert)
	f.control = tcsv1.NewTaskControlClient(f.conn)
	return credentialsv1.NewInstanceCredentialsClient(f.conn), issuer, bundle
}
func managedCredential(t *testing.T, f *fixture, issuer *pki.Issuer) []byte {
	t.Helper()
	f.caller.Instance = "ins_" + strings.TrimPrefix(f.caller.Instance, "instance-")
	raw, key := renewalCSR(t)
	csr, err := pki.ParseCSR(raw)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := issuer.Issue(csr, f.caller.Instance, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.workerCert = renewalPair(t, issued.PEM, key)
	f.exec(`INSERT INTO agent_instances(instance_id,agent_id,cred_hash,spiffe_id) VALUES($1,$2,$3,$4)`, f.caller.Instance, f.caller.Agent, issued.Fingerprint, issued.URI)
	f.exec(`INSERT INTO instance_credentials(fingerprint,instance_id,version,public_key_hash,certificate_der,certificate_pem,expires_at) VALUES($1,$2,1,$3,$4,$5,$6)`, issued.Fingerprint, f.caller.Instance, issued.PublicKeyHash, issued.DER, issued.PEM, issued.Expires)
	return append(issued.PEM, key...)
}
func prepareRenewal(t *testing.T, f *fixture, c credentialsv1.InstanceCredentialsClient) (*credentialsv1.PrepareRenewalRequest, *credentialsv1.PreparedCredential, tls.Certificate) {
	t.Helper()
	csr, key := renewalCSR(t)
	r := &credentialsv1.PrepareRenewalRequest{RequestId: renewalID(), ExpectedVersion: 1, CsrPem: csr}
	out, err := c.PrepareRenewal(f.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	return r, out, renewalPair(t, out.CertificatePem, key)
}
func TestRPCRenewalTwoPhaseAndReplay(t *testing.T) {
	f := setup(t)
	old, _, _ := enableRenewal(t, f)
	r, out, next := prepareRenewal(t, f, old)
	again, err := old.PrepareRenewal(f.ctx, r)
	if err != nil || string(again.GetCertificatePem()) != string(out.CertificatePem) {
		t.Fatal("prepared reply not recoverable", err)
	}
	s, err := old.GetStatus(f.ctx, &credentialsv1.GetStatusRequest{})
	if err != nil || s.GetVersion() != 1 {
		t.Fatal("prepare disabled old credential", err)
	}
	conn := f.connect(next)
	client := credentialsv1.NewInstanceCredentialsClient(conn)
	_, err = client.GetStatus(f.ctx, &credentialsv1.GetStatusRequest{})
	code(t, err, codes.Unauthenticated)
	_, err = client.PrepareRenewal(f.ctx, r)
	code(t, err, codes.Unauthenticated)
	_, err = tcsv1.NewTaskControlClient(conn).GetTask(f.ctx, &tcsv1.GetTaskRequest{TaskId: "not-yet-authorized"})
	code(t, err, codes.Unauthenticated)
	_, err = old.ActivateRenewal(f.ctx, &credentialsv1.ActivateRenewalRequest{RequestId: r.RequestId})
	code(t, err, codes.PermissionDenied)
	_, err = client.ActivateRenewal(f.ctx, &credentialsv1.ActivateRenewalRequest{RequestId: renewalID()})
	code(t, err, codes.PermissionDenied)
	s, err = client.ActivateRenewal(f.ctx, &credentialsv1.ActivateRenewalRequest{RequestId: r.RequestId})
	if err != nil || s.GetVersion() != 2 || s.GetFingerprint() != service.Fingerprint(next.Leaf) {
		t.Fatal("activation", s, err)
	}
	s, err = client.ActivateRenewal(f.ctx, &credentialsv1.ActivateRenewalRequest{RequestId: r.RequestId})
	if err != nil || s.GetVersion() != 2 {
		t.Fatal("activation replay", err)
	}
	_, err = old.GetStatus(f.ctx, &credentialsv1.GetStatusRequest{})
	code(t, err, codes.Unauthenticated)
	_, err = client.GetStatus(f.ctx, &credentialsv1.GetStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM instance_credentials WHERE instance_id=$1 AND retired_at IS NULL`, f.caller.Instance) != 1 || f.count(`SELECT count(*) FROM audit_log WHERE domain_id=$1 AND action='credential.renewal_activated'`, f.domain) != 1 {
		t.Fatal("activation not atomic/idempotent")
	}
}
func TestRPCRenewalFreshKeyAndConflicts(t *testing.T) {
	f := setup(t)
	client, _, _ := enableRenewal(t, f)
	raw, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, f.workerCert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.PrepareRenewal(f.ctx, &credentialsv1.PrepareRenewalRequest{RequestId: renewalID(), ExpectedVersion: 1, CsrPem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: raw})})
	code(t, err, codes.AlreadyExists)
	r, _, _ := prepareRenewal(t, f, client)
	other, _ := renewalCSR(t)
	r.CsrPem = other
	_, err = client.PrepareRenewal(f.ctx, r)
	code(t, err, codes.AlreadyExists)
	r.RequestId = renewalID()
	_, err = client.PrepareRenewal(f.ctx, r)
	code(t, err, codes.AlreadyExists)
	if f.count(`SELECT count(*) FROM credential_renewals WHERE instance_id=$1`, f.caller.Instance) != 1 {
		t.Fatal("multiple pending renewals")
	}
}
func TestRPCRenewalConcurrentPrepareAndActivate(t *testing.T) {
	f := setup(t)
	old, _, _ := enableRenewal(t, f)
	type result struct {
		r   *credentialsv1.PrepareRenewalRequest
		out *credentialsv1.PreparedCredential
		key []byte
		err error
	}
	done := make(chan result, 2)
	for i := 0; i < 2; i++ {
		csr, key := renewalCSR(t)
		r := &credentialsv1.PrepareRenewalRequest{RequestId: renewalID(), ExpectedVersion: 1, CsrPem: csr}
		go func() { out, err := old.PrepareRenewal(f.ctx, r); done <- result{r, out, key, err} }()
	}
	var winner result
	wins := 0
	for i := 0; i < 2; i++ {
		r := <-done
		if r.err == nil {
			winner = r
			wins++
		} else {
			code(t, r.err, codes.AlreadyExists)
		}
	}
	if wins != 1 {
		t.Fatal("expected one prepared version")
	}
	client := credentialsv1.NewInstanceCredentialsClient(f.connect(renewalPair(t, winner.out.CertificatePem, winner.key)))
	errors := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := client.ActivateRenewal(f.ctx, &credentialsv1.ActivateRenewalRequest{RequestId: winner.r.RequestId})
			errors <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if f.count(`SELECT count(*) FROM instance_credentials WHERE instance_id=$1 AND version=2`, f.caller.Instance) != 1 {
		t.Fatal("duplicate activation")
	}
}
func TestRPCRenewalInvalidatedByOwnerOrExpiry(t *testing.T) {
	for _, kind := range []string{"revoke", "rotate", "expire"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			old, issuer, _ := enableRenewal(t, f)
			r, _, next := prepareRenewal(t, f, old)
			switch kind {
			case "revoke":
				f.exec(`UPDATE agent_instances SET revoked_at=clock_timestamp(),credential_version=credential_version+1 WHERE instance_id=$1`, f.caller.Instance)
			case "expire":
				f.exec(`UPDATE credential_renewals SET prepare_expires_at=clock_timestamp()-interval '1 second' WHERE instance_id=$1`, f.caller.Instance)
			case "rotate":
				raw, _ := renewalCSR(t)
				csr, _ := pki.ParseCSR(raw)
				issued, err := issuer.Issue(csr, f.caller.Instance, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				f.exec(`UPDATE agent_instances SET cred_hash=$2,credential_version=credential_version+1 WHERE instance_id=$1`, f.caller.Instance, issued.Fingerprint)
				f.exec(`INSERT INTO instance_credentials(fingerprint,instance_id,version,public_key_hash,certificate_der,certificate_pem,expires_at) VALUES($1,$2,2,$3,$4,$5,$6)`, issued.Fingerprint, f.caller.Instance, issued.PublicKeyHash, issued.DER, issued.PEM, issued.Expires)
			}
			_, err := credentialsv1.NewInstanceCredentialsClient(f.connect(next)).ActivateRenewal(f.ctx, &credentialsv1.ActivateRenewalRequest{RequestId: r.RequestId})
			code(t, err, codes.Unauthenticated)
			if f.count(`SELECT count(*) FROM credential_renewals WHERE instance_id=$1 AND status='active'`, f.caller.Instance) != 0 {
				t.Fatal("invalidated renewal activated")
			}
		})
	}
}
func TestRPCRenewalAuditRollback(t *testing.T) {
	for _, action := range []string{"credential.renewal_prepared", "credential.renewal_activated"} {
		t.Run(action, func(t *testing.T) {
			f := setup(t)
			old, _, _ := enableRenewal(t, f)
			if action == "credential.renewal_prepared" {
				drop := auditConstraint(t, f, action)
				csr, _ := renewalCSR(t)
				_, err := old.PrepareRenewal(f.ctx, &credentialsv1.PrepareRenewalRequest{RequestId: renewalID(), ExpectedVersion: 1, CsrPem: csr})
				code(t, err, codes.Internal)
				if f.count(`SELECT count(*) FROM credential_renewals WHERE instance_id=$1`, f.caller.Instance) != 0 {
					t.Fatal("prepare survived audit failure")
				}
				drop()
				return
			}
			r, _, next := prepareRenewal(t, f, old)
			drop := auditConstraint(t, f, action)
			client := credentialsv1.NewInstanceCredentialsClient(f.connect(next))
			_, err := client.ActivateRenewal(f.ctx, &credentialsv1.ActivateRenewalRequest{RequestId: r.RequestId})
			code(t, err, codes.Internal)
			s, err := old.GetStatus(f.ctx, &credentialsv1.GetStatusRequest{})
			if err != nil || s.GetVersion() != 1 {
				t.Fatal("audit failure retired old key")
			}
			if f.count(`SELECT count(*) FROM instance_credentials WHERE instance_id=$1 AND version=2`, f.caller.Instance) != 0 {
				t.Fatal("partial activated history")
			}
			drop()
			_, err = client.ActivateRenewal(f.ctx, &credentialsv1.ActivateRenewalRequest{RequestId: r.RequestId})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestRPCRenewalExpiryAfterKeyLock(t *testing.T) {
	f := setup(t)
	old, _, _ := enableRenewal(t, f)
	r, _, next := prepareRenewal(t, f, old)
	f.exec(`UPDATE credential_renewals SET prepare_expires_at=clock_timestamp()+interval '2 seconds' WHERE instance_id=$1`, f.caller.Instance)
	var hash string
	if err := f.db.QueryRow(f.ctx, `SELECT public_key_hash FROM credential_renewals WHERE instance_id=$1`, f.caller.Instance).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "anet.credential-key:"+hash); err != nil {
		t.Fatal(err)
	}
	client := credentialsv1.NewInstanceCredentialsClient(f.connect(next))
	done := make(chan error, 1)
	go func() {
		_, err := client.ActivateRenewal(f.ctx, &credentialsv1.ActivateRenewalRequest{RequestId: r.RequestId})
		done <- err
	}()
	waitForLock(t, f, "INSERT INTO instance_credentials%")
	time.Sleep(2100 * time.Millisecond)
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	code(t, <-done, codes.FailedPrecondition)
	s, err := old.GetStatus(f.ctx, &credentialsv1.GetStatusRequest{})
	if err != nil || s.GetVersion() != 1 {
		t.Fatal("expired preparation activated after lock")
	}
}
func TestRPCRenewalRevocationWhileActivationWaits(t *testing.T) {
	f := setup(t)
	old, _, _ := enableRenewal(t, f)
	r, _, next := prepareRenewal(t, f, old)
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err = tx.Exec(f.ctx, `SELECT instance_id FROM agent_instances WHERE instance_id=$1 FOR UPDATE`, f.caller.Instance); err != nil {
		t.Fatal(err)
	}
	client := credentialsv1.NewInstanceCredentialsClient(f.connect(next))
	done := make(chan error, 1)
	go func() {
		_, err := client.ActivateRenewal(f.ctx, &credentialsv1.ActivateRenewalRequest{RequestId: r.RequestId})
		done <- err
	}()
	waitForLock(t, f, "SELECT i.instance_id,i.credential_version,%")
	if _, err = tx.Exec(f.ctx, `UPDATE agent_instances SET revoked_at=clock_timestamp(),credential_version=credential_version+1 WHERE instance_id=$1`, f.caller.Instance); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	code(t, <-done, codes.PermissionDenied)
}

func TestRPCRenewalAbandonAndCrossIssuerKeyClaims(t *testing.T) {
	f := setup(t)
	old, issuer, _ := enableRenewal(t, f)
	csr, key := renewalCSR(t)
	r := &credentialsv1.PrepareRenewalRequest{RequestId: renewalID(), ExpectedVersion: 1, CsrPem: csr}
	out, err := old.PrepareRenewal(f.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	pending := credentialsv1.NewInstanceCredentialsClient(f.connect(renewalPair(t, out.CertificatePem, key)))
	_, err = pending.AbandonRenewal(f.ctx, &credentialsv1.AbandonRenewalRequest{RequestId: r.RequestId, ExpectedVersion: 1})
	code(t, err, codes.Unauthenticated)
	// Owner issuance using the same prepared public key must also be rejected;
	// the check crosses tables, rather than only an index within either table.
	parsed, _ := pki.ParseCSR(csr)
	manual, err := issuer.Issue(parsed, f.caller.Instance, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.db.Exec(f.ctx, `INSERT INTO instance_credentials(fingerprint,instance_id,version,public_key_hash,certificate_der,certificate_pem,expires_at) VALUES($1,$2,2,$3,$4,$5,$6)`, manual.Fingerprint, f.caller.Instance, manual.PublicKeyHash, manual.DER, manual.PEM, manual.Expires)
	if err == nil {
		t.Fatal("owner issuance reused pending key")
	}
	s, err := old.AbandonRenewal(f.ctx, &credentialsv1.AbandonRenewalRequest{RequestId: r.RequestId, ExpectedVersion: 1})
	if err != nil || s.GetVersion() != 1 {
		t.Fatal("abandon", err)
	}
	_, err = pending.ActivateRenewal(f.ctx, &credentialsv1.ActivateRenewalRequest{RequestId: r.RequestId})
	code(t, err, codes.Unauthenticated)
	r.RequestId = renewalID()
	_, err = old.PrepareRenewal(f.ctx, r)
	code(t, err, codes.AlreadyExists)
	next, _, cert := prepareRenewal(t, f, old)
	client := credentialsv1.NewInstanceCredentialsClient(f.connect(cert))
	_, err = client.ActivateRenewal(f.ctx, &credentialsv1.ActivateRenewalRequest{RequestId: next.RequestId})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.AbandonRenewal(f.ctx, &credentialsv1.AbandonRenewalRequest{RequestId: next.RequestId, ExpectedVersion: 2})
	code(t, err, codes.PermissionDenied)
}

func TestRPCRenewalActivationAfterOldCertificateExpires(t *testing.T) {
	f := setup(t)
	_, _, _ = enableRenewal(t, f)
	short := cert(t, f.ca, f.caKey, "spiffe://test.agentnet/agent-instance/"+f.caller.Instance, false, 2*time.Second)
	fp := service.Fingerprint(short.Leaf)
	f.exec(`UPDATE agent_instances SET cred_hash=$2,credential_version=2 WHERE instance_id=$1`, f.caller.Instance, fp)
	f.exec(`INSERT INTO instance_credentials(fingerprint,instance_id,version,public_key_hash,certificate_der,certificate_pem,expires_at) VALUES($1,$2,2,$3,$4,$5,$6)`, fp, f.caller.Instance, pki.Fingerprint(short.Leaf.RawSubjectPublicKeyInfo), short.Certificate[0], pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: short.Certificate[0]}), short.Leaf.NotAfter)
	old := credentialsv1.NewInstanceCredentialsClient(f.connect(short))
	csr, key := renewalCSR(t)
	r := &credentialsv1.PrepareRenewalRequest{RequestId: renewalID(), ExpectedVersion: 2, CsrPem: csr}
	out, err := old.PrepareRenewal(f.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(short.Leaf.NotAfter) + 50*time.Millisecond)
	_, err = old.GetStatus(f.ctx, &credentialsv1.GetStatusRequest{})
	code(t, err, codes.Unauthenticated)
	next := credentialsv1.NewInstanceCredentialsClient(f.connect(renewalPair(t, out.CertificatePem, key)))
	active, err := next.ActivateRenewal(f.ctx, &credentialsv1.ActivateRenewalRequest{RequestId: r.RequestId})
	if err != nil || active.GetVersion() != 3 {
		t.Fatal("bounded prepared proof did not survive old certificate expiry", err)
	}
}
