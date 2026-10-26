// Package credentialclient persists renewal recovery state on the workload host.
package credentialclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/agentnet/agentnet/internal/pki"
	credentialsv1 "github.com/agentnet/agentnet/proto/gen/anet/credentials/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	Endpoint     string `json:"endpoint"`
	ServerName   string `json:"server_name"`
	ServerCAFile string `json:"server_ca_file"`
	BundleFile   string `json:"bundle_file"`
	StateDir     string `json:"state_dir"`
}
type journal struct {
	Endpoint, ServerName, TrustHash, Instance, RequestID, BaseFingerprint string
	Version                                                               int64
	Key, CSR, Certificate                                                 []byte
	ActivateBefore                                                        time.Time
}

var ErrAbandoned = errors.New("expired or conflicting preparation abandoned; retry with a new key")

func abandon(ctx context.Context, c Config, roots *x509.CertPool, j journal, path string) error {
	bundle, err := os.ReadFile(c.BundleFile)
	if err != nil {
		return err
	}
	old, err := pair(bundle)
	if err != nil {
		return err
	}
	if pki.Fingerprint(old.Certificate[0]) != j.BaseFingerprint {
		return fmt.Errorf("cannot abandon after local credential changed")
	}
	conn, err := connect(c, roots, old)
	if err != nil {
		return err
	}
	defer conn.Close()
	out, err := credentialsv1.NewInstanceCredentialsClient(conn).AbandonRenewal(ctx, &credentialsv1.AbandonRenewalRequest{RequestId: j.RequestID, ExpectedVersion: j.Version})
	if err != nil {
		return err
	}
	if out.GetFingerprint() != j.BaseFingerprint || out.GetVersion() != j.Version {
		return fmt.Errorf("abandonment did not preserve old credential")
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	return ErrAbandoned
}

func pair(bundle []byte) (tls.Certificate, error) {
	p, err := tls.X509KeyPair(bundle, bundle)
	if err != nil {
		return p, fmt.Errorf("invalid credential bundle")
	}
	p.Leaf, err = x509.ParseCertificate(p.Certificate[0])
	return p, err
}
func connect(c Config, roots *x509.CertPool, p tls.Certificate) (*grpc.ClientConn, error) {
	return grpc.NewClient(c.Endpoint, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: c.ServerName, Certificates: []tls.Certificate{p}})))
}
func readJSON(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	return d.Decode(out)
}
func save(path string, j journal) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return writeFile(path, b)
}

// Renew resumes an unfinished attempt before creating a new key. OneShot callers
// request rotation immediately; Watch below starts new attempts when renewal is due.
func Renew(ctx context.Context, c Config) error {
	if c.Endpoint == "" || c.ServerName == "" || c.ServerCAFile == "" || c.BundleFile == "" || c.StateDir == "" {
		return fmt.Errorf("endpoint, server name, CA, bundle and state directory required")
	}
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(c.StateDir, "renew.lock"))
	if err != nil {
		return fmt.Errorf("renewal state is in use: %w", err)
	}
	defer unlock()
	ca, err := os.ReadFile(c.ServerCAFile)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return fmt.Errorf("no server CA")
	}
	path := filepath.Join(c.StateDir, "pending.json")
	var j journal
	err = readJSON(path, &j)
	if errors.Is(err, os.ErrNotExist) {
		bundle, e := os.ReadFile(c.BundleFile)
		if e != nil {
			return e
		}
		old, e := pair(bundle)
		if e != nil {
			return e
		}
		conn, e := connect(c, roots, old)
		if e != nil {
			return e
		}
		status, e := credentialsv1.NewInstanceCredentialsClient(conn).GetStatus(ctx, &credentialsv1.GetStatusRequest{})
		conn.Close()
		if e != nil {
			return e
		}
		if status.InstanceId == "" || status.Version < 1 || status.Fingerprint != pki.Fingerprint(old.Certificate[0]) {
			return fmt.Errorf("credential status mismatch")
		}
		_, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return e
		}
		csr, e := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
		if e != nil {
			return e
		}
		der, e := x509.MarshalPKCS8PrivateKey(key)
		if e != nil {
			return e
		}
		nonce := make([]byte, 32)
		if _, e = rand.Read(nonce); e != nil {
			return e
		}
		j = journal{Endpoint: c.Endpoint, ServerName: c.ServerName, TrustHash: pki.Fingerprint(ca), Instance: status.InstanceId, RequestID: base64.RawURLEncoding.EncodeToString(nonce), Version: status.Version, BaseFingerprint: status.Fingerprint, Key: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), CSR: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})}
		if e = save(path, j); e != nil {
			return e
		}
	} else if err != nil {
		return fmt.Errorf("cannot read renewal recovery state: %w", err)
	}
	if j.Endpoint != c.Endpoint || j.ServerName != c.ServerName || j.TrustHash != pki.Fingerprint(ca) || j.RequestID == "" || j.Version < 1 {
		return fmt.Errorf("renewal recovery state does not match configuration")
	}
	if len(j.Certificate) == 0 {
		bundle, e := os.ReadFile(c.BundleFile)
		if e != nil {
			return e
		}
		old, e := pair(bundle)
		if e != nil {
			return e
		}
		if pki.Fingerprint(old.Certificate[0]) != j.BaseFingerprint {
			return fmt.Errorf("current credential changed outside pending renewal")
		}
		conn, e := connect(c, roots, old)
		if e != nil {
			return e
		}
		prepared, e := credentialsv1.NewInstanceCredentialsClient(conn).PrepareRenewal(ctx, &credentialsv1.PrepareRenewalRequest{RequestId: j.RequestID, ExpectedVersion: j.Version, CsrPem: j.CSR})
		conn.Close()
		if e != nil {
			if status.Code(e) == codes.AlreadyExists {
				return abandon(ctx, c, roots, j, path)
			}
			return e
		}
		if prepared.InstanceId != j.Instance || prepared.Version != j.Version+1 {
			return fmt.Errorf("prepared credential identity mismatch")
		}
		candidate, e := pair(append(append([]byte(nil), prepared.CertificatePem...), j.Key...))
		if e != nil {
			return e
		}
		if len(candidate.Leaf.URIs) != 1 || len(old.Leaf.URIs) != 1 || candidate.Leaf.URIs[0].String() != old.Leaf.URIs[0].String() {
			return fmt.Errorf("prepared credential URI mismatch")
		}
		j.Certificate = prepared.CertificatePem
		if prepared.ActivateBefore == nil || prepared.ActivateBefore.CheckValid() != nil {
			return fmt.Errorf("invalid activation deadline")
		}
		j.ActivateBefore = prepared.ActivateBefore.AsTime()
		if e = save(path, j); e != nil {
			return e
		}
	}
	bundle := append(append([]byte(nil), j.Certificate...), j.Key...)
	candidate, err := pair(bundle)
	if err != nil {
		return err
	}
	conn, err := connect(c, roots, candidate)
	if err != nil {
		return err
	}
	status, err := credentialsv1.NewInstanceCredentialsClient(conn).ActivateRenewal(ctx, &credentialsv1.ActivateRenewalRequest{RequestId: j.RequestID})
	conn.Close()
	if err != nil {
		if !j.ActivateBefore.IsZero() && time.Now().After(j.ActivateBefore) {
			return abandon(ctx, c, roots, j, path)
		}
		return err
	}
	if status.InstanceId != j.Instance || status.Version != j.Version+1 || status.Fingerprint != pki.Fingerprint(candidate.Certificate[0]) {
		return fmt.Errorf("activation result mismatch")
	}
	if err = writeFile(c.BundleFile, bundle); err != nil {
		return err
	}
	return os.Remove(path)
}

// Watch keeps retrying the same journal on network/availability failures. Failed
// attempts keep their keys until activation or confirmed abandonment through the
// still-current old credential. Owner recovery is needed if neither key can act.
func Watch(ctx context.Context, c Config, report func(error)) error {
	if c.Endpoint == "" || c.ServerName == "" || c.ServerCAFile == "" || c.BundleFile == "" || c.StateDir == "" {
		return fmt.Errorf("complete renewal configuration required")
	}
	for {
		_, pendingErr := os.Stat(filepath.Join(c.StateDir, "pending.json"))
		due := pendingErr == nil
		bundle, err := os.ReadFile(c.BundleFile)
		if err == nil {
			cert, e := pair(bundle)
			err = e
			if e == nil {
				margin := cert.Leaf.NotAfter.Sub(cert.Leaf.NotBefore) / 3
				if margin > time.Hour {
					margin = time.Hour
				}
				due = due || time.Until(cert.Leaf.NotAfter) <= margin
			}
		}
		if due && (err == nil || pendingErr == nil) {
			attempt, cancel := context.WithTimeout(ctx, 45*time.Second)
			err = Renew(attempt, c)
			cancel()
		}
		if err != nil && report != nil {
			report(err)
		}
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
