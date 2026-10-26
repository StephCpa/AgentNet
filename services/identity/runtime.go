package identity

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/internal/pki"
	"github.com/jackc/pgx/v5/pgxpool"
)

type FileConfig struct {
	Domain              string             `json:"domain"`
	Listen              string             `json:"listen"`
	PublicURL           string             `json:"public_url"`
	Issuer              string             `json:"issuer"`
	ClientID            string             `json:"client_id"`
	ClientSecretFile    string             `json:"client_secret_file,omitempty"`
	IssuerCAFile        string             `json:"issuer_ca_file,omitempty"`
	CertificateFile     string             `json:"certificate_file"`
	PrivateKeyFile      string             `json:"private_key_file"`
	Profiles            map[string]Profile `json:"profiles"`
	CredentialCAFile    string             `json:"credential_ca_file,omitempty"`
	CredentialCAKeyFile string             `json:"credential_ca_key_file,omitempty"`
	TrustDomain         string             `json:"trust_domain,omitempty"`
	CredentialTTL       string             `json:"credential_ttl,omitempty"`
}

func OpenRuntime(ctx context.Context, path, dsn string) (*http.Server, *pgxpool.Pool, error) {
	if path == "" || dsn == "" {
		return nil, nil, fmt.Errorf("config and ANET_DATABASE_URL are required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	raw, err = canonical.Bytes(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid configuration JSON")
	}
	var c FileConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil {
		return nil, nil, err
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(filepath.Dir(path), p)
	}
	cert, err := tls.LoadX509KeyPair(resolve(c.CertificateFile), resolve(c.PrivateKeyFile))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot load HTTPS certificate")
	}
	secret := ""
	var issuer *pki.Issuer
	if c.CredentialCAFile != "" || c.CredentialCAKeyFile != "" || c.TrustDomain != "" || c.CredentialTTL != "" {
		if c.CredentialCAFile == "" || c.CredentialCAKeyFile == "" || c.TrustDomain == "" {
			return nil, nil, fmt.Errorf("credential CA, private key and trust domain are required together")
		}
		ca, e := tls.LoadX509KeyPair(resolve(c.CredentialCAFile), resolve(c.CredentialCAKeyFile))
		if e != nil {
			return nil, nil, fmt.Errorf("cannot load credential CA")
		}
		ttl := 12 * time.Hour
		if c.CredentialTTL != "" {
			ttl, e = time.ParseDuration(c.CredentialTTL)
			if e != nil {
				return nil, nil, fmt.Errorf("invalid credential TTL")
			}
		}
		issuer, e = pki.New(ca, c.TrustDomain, ttl)
		if e != nil {
			return nil, nil, e
		}
	}
	if c.ClientSecretFile != "" {
		data, e := os.ReadFile(resolve(c.ClientSecretFile))
		if e != nil {
			return nil, nil, fmt.Errorf("cannot load client secret")
		}
		secret = strings.TrimSpace(string(data))
		if secret == "" {
			return nil, nil, fmt.Errorf("empty client secret")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if c.IssuerCAFile != "" {
		data, e := os.ReadFile(resolve(c.IssuerCAFile))
		if e != nil {
			return nil, nil, e
		}
		roots, e := x509.SystemCertPool()
		if e != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(data) {
			return nil, nil, fmt.Errorf("issuer CA file has no certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid database configuration")
	}
	fail := func(e error) (*http.Server, *pgxpool.Pool, error) { db.Close(); return nil, nil, e }
	if _, err = db.Exec(ctx, `SELECT session_hash FROM user_sessions LIMIT 0`); err != nil {
		return fail(fmt.Errorf("database unavailable or migrations incomplete"))
	}
	service, err := New(ctx, db, Config{Domain: c.Domain, Issuer: c.Issuer, ClientID: c.ClientID, ClientSecret: secret, PublicURL: c.PublicURL, Profiles: c.Profiles, HTTPClient: &http.Client{Transport: transport}, CredentialIssuer: issuer})
	if err != nil {
		return fail(err)
	}
	if c.Listen == "" {
		return fail(fmt.Errorf("listen is required"))
	}
	return &http.Server{Addr: c.Listen, Handler: service, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}, db, nil
}
