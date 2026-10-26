package pki_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"github.com/agentnet/agentnet/internal/pki"
	"math/big"
	"net/url"
	"testing"
	"time"
)

func ca(t *testing.T) tls.Certificate {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	c := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, c, c, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
func TestPKIIgnoresRequestedIdentityAndCA(t *testing.T) {
	issuer, err := pki.New(ca(t), "test.agentnet", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	uri, _ := url.Parse("spiffe://admin.test/agent-instance/admin")
	bc, _ := asn1.Marshal(struct {
		CA   bool
		Path int
	}{true, 0})
	raw, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "administrator"}, DNSNames: []string{"admin.test"}, URIs: []*url.URL{uri}, ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 19}, Critical: true, Value: bc}}}, key)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.ParseCSR(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: raw}))
	if err != nil {
		t.Fatal(err)
	}
	out, err := issuer.Issue(csr, "ins_fixture", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(out.DER)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA || leaf.Subject.CommonName != "" || len(leaf.DNSNames) != 0 || len(leaf.URIs) != 1 || leaf.URIs[0].String() != "spiffe://test.agentnet/agent-instance/ins_fixture" || leaf.NotAfter.Sub(leaf.NotBefore) > 24*time.Hour || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatal("CSR changed certificate authority or identity")
	}
}
func TestPKIRejectsWeakAndTamperedRequests(t *testing.T) {
	for _, mode := range []string{"weak-rsa", "sha1", "tampered", "private-key"} {
		t.Run(mode, func(t *testing.T) {
			var raw []byte
			typ := "CERTIFICATE REQUEST"
			switch mode {
			case "weak-rsa", "sha1":
				bits := 2048
				alg := x509.SHA1WithRSA
				if mode == "weak-rsa" {
					bits = 1024
					alg = x509.SHA256WithRSA
				}
				key, err := rsa.GenerateKey(rand.Reader, bits)
				if err != nil {
					t.Fatal(err)
				}
				raw, err = x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{SignatureAlgorithm: alg}, key)
				if err != nil {
					t.Fatal(err)
				}
			default:
				_, key, _ := ed25519.GenerateKey(rand.Reader)
				if mode == "private-key" {
					typ = "PRIVATE KEY"
					raw, _ = x509.MarshalPKCS8PrivateKey(key)
				} else {
					raw, _ = x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
					raw[len(raw)-1] ^= 1
				}
			}
			if _, err := pki.ParseCSR(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: raw})); err == nil {
				t.Fatal("bad CSR accepted")
			}
		})
	}
}
func TestPKIIssuerBounds(t *testing.T) {
	c := ca(t)
	short, err := pki.New(c, "test.agentnet", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, requestKey, _ := ed25519.GenerateKey(rand.Reader)
	raw, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, requestKey)
	if err != nil {
		t.Fatal(err)
	}
	request, err := pki.ParseCSR(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: raw}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = short.Issue(request, "ins_short", time.Now()); err != nil {
		t.Fatal("valid minimum TTL is not issuable", err)
	}
	if _, err = short.Issue(request, "ins_expired", time.Now().Add(49*time.Hour)); err == nil {
		t.Fatal("expired issuer signed a certificate")
	}
	if _, err := pki.New(c, "test.agentnet", 25*time.Hour); err == nil {
		t.Fatal("long life allowed")
	}
	if _, err := pki.New(c, "test.agentnet/bad", time.Hour); err == nil {
		t.Fatal("invalid trust domain allowed")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	c.PrivateKey = other
	if _, err := pki.New(c, "test.agentnet", time.Hour); err == nil {
		t.Fatal("mismatched CA key accepted")
	}
}
