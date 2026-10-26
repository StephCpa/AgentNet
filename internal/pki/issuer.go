// Package pki signs client certificates for locally generated workload keys.
// It is not a SPIFFE Workload API or an implementation of node attestation.
package pki

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var ErrInvalid = errors.New("invalid certificate issuer or request")
var trustDomainRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}[a-z0-9]$`)
var instanceRE = regexp.MustCompile(`^ins_[a-zA-Z0-9-]+$`)

type Issuer struct {
	ca     *x509.Certificate
	key    crypto.Signer
	chain  [][]byte
	domain string
	ttl    time.Duration
}
type Issued struct {
	DER, PEM                        []byte
	Fingerprint, PublicKeyHash, URI string
	Expires                         time.Time
}

func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func New(certificate tls.Certificate, trustDomain string, ttl time.Duration) (*Issuer, error) {
	if !trustDomainRE.MatchString(trustDomain) || len(certificate.Certificate) == 0 || ttl < time.Minute || ttl > 24*time.Hour {
		return nil, ErrInvalid
	}
	ca, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil || !ca.IsCA || !ca.BasicConstraintsValid || ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, ErrInvalid
	}
	key, ok := certificate.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, ErrInvalid
	}
	pub, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil || !bytes.Equal(pub, ca.RawSubjectPublicKeyInfo) {
		return nil, ErrInvalid
	}
	now := time.Now()
	if now.Before(ca.NotBefore) || !now.Add(time.Minute).Before(ca.NotAfter) {
		return nil, ErrInvalid
	}
	chain := make([][]byte, len(certificate.Certificate))
	for n, der := range certificate.Certificate {
		chain[n] = append([]byte(nil), der...)
	}
	return &Issuer{ca: ca, key: key, chain: chain, domain: trustDomain, ttl: ttl}, nil
}
func ParseCSR(raw []byte) (*x509.CertificateRequest, error) {
	if len(raw) == 0 || len(raw) > 16<<10 {
		return nil, ErrInvalid
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(block.Headers) > 0 || strings.TrimSpace(string(rest)) != "" {
		return nil, ErrInvalid
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		return nil, ErrInvalid
	}
	switch csr.SignatureAlgorithm {
	case x509.PureEd25519, x509.SHA256WithRSA, x509.SHA384WithRSA, x509.SHA512WithRSA, x509.SHA256WithRSAPSS, x509.SHA384WithRSAPSS, x509.SHA512WithRSAPSS, x509.ECDSAWithSHA256, x509.ECDSAWithSHA384, x509.ECDSAWithSHA512:
	default:
		return nil, ErrInvalid
	}
	switch key := csr.PublicKey.(type) {
	case ed25519.PublicKey:
		if len(key) != ed25519.PublicKeySize {
			return nil, ErrInvalid
		}
	case *rsa.PublicKey:
		if key.N.BitLen() < 2048 || key.N.BitLen() > 8192 || key.E < 65537 {
			return nil, ErrInvalid
		}
	case *ecdsa.PublicKey:
		if key.Curve != elliptic.P256() && key.Curve != elliptic.P384() {
			return nil, ErrInvalid
		}
	default:
		return nil, ErrInvalid
	}
	return csr, nil
}
func (i *Issuer) Issue(csr *x509.CertificateRequest, instance string, now time.Time) (Issued, error) {
	if csr == nil || !instanceRE.MatchString(instance) {
		return Issued{}, ErrInvalid
	}
	parsed, err := ParseCSR(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr.Raw}))
	if err != nil {
		return Issued{}, err
	}
	csr = parsed
	before := now.UTC().Truncate(time.Second).Add(-time.Second)
	if before.Before(i.ca.NotBefore) {
		before = i.ca.NotBefore
	}
	expires := before.Add(i.ttl)
	if expires.After(i.ca.NotAfter) {
		expires = i.ca.NotAfter
	}
	if now.Before(i.ca.NotBefore) || !expires.After(now.Add(time.Second)) {
		return Issued{}, ErrInvalid
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Issued{}, err
	}
	serial.Add(serial, big.NewInt(1))
	uri := &url.URL{Scheme: "spiffe", Host: i.domain, Path: "/agent-instance/" + instance}
	// CSR identity, SAN, CA and requested extensions are intentionally not copied.
	template := &x509.Certificate{SerialNumber: serial, NotBefore: before, NotAfter: expires, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true, URIs: []*url.URL{uri}}
	der, err := x509.CreateCertificate(rand.Reader, template, i.ca, csr.PublicKey, i.key)
	if err != nil {
		return Issued{}, err
	}
	out := Issued{DER: der, Fingerprint: Fingerprint(der), PublicKeyHash: Fingerprint(csr.RawSubjectPublicKeyInfo), URI: uri.String(), Expires: expires}
	out.PEM = i.PEM(der)
	return out, nil
}
func (i *Issuer) PEM(der []byte) []byte {
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for _, ca := range i.chain {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca})...)
	}
	return out
}
