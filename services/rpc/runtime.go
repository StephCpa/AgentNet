package rpc

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	localadapter "github.com/agentnet/agentnet/adapters/local"
	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/internal/localtx"
	"github.com/agentnet/agentnet/internal/pki"
	credentialservice "github.com/agentnet/agentnet/services/credentials"
	"github.com/agentnet/agentnet/services/gateway"
	"github.com/agentnet/agentnet/services/ics"
	"github.com/agentnet/agentnet/services/tcs"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
)

type OperationConfig struct {
	LocalHandler string   `json:"local_handler,omitempty"`
	Action       string   `json:"action"`
	Resource     string   `json:"resource"`
	Capability   string   `json:"capability"`
	Purpose      string   `json:"purpose"`
	Dimensions   []string `json:"dimensions"`
	TargetTTL    string   `json:"target_ttl,omitempty"`
	Sensitivity  string   `json:"sensitivity"`
	DataClass    string   `json:"data_class"`
}
type FileConfig struct {
	Domain              string                       `json:"domain"`
	TrustDomain         string                       `json:"trust_domain"`
	Listen              string                       `json:"listen"`
	CertificateFile     string                       `json:"certificate_file"`
	PrivateKeyFile      string                       `json:"private_key_file"`
	ClientCAFile        string                       `json:"client_ca_file"`
	ICSKeyID            string                       `json:"ics_key_id"`
	ICSSigningKeyFile   string                       `json:"ics_signing_key_file"`
	CommandKeys         map[string]ed25519.PublicKey `json:"command_keys"`
	ResultKeys          map[string]ed25519.PublicKey `json:"result_keys"`
	ICSVerificationKeys map[string]ed25519.PublicKey `json:"ics_verification_keys,omitempty"`
	Operations          map[string]OperationConfig   `json:"operations"`
	GWKeyID             string                       `json:"gw_key_id,omitempty"`
	GWSigningKeyFile    string                       `json:"gw_signing_key_file,omitempty"`
	GWCommandTTL        string                       `json:"gw_command_ttl,omitempty"`
	CredentialCAFile    string                       `json:"credential_ca_file,omitempty"`
	CredentialCAKeyFile string                       `json:"credential_ca_key_file,omitempty"`
	CredentialTTL       string                       `json:"credential_ttl,omitempty"`
}

// OpenRuntime loads explicit configuration. It never enrolls identities, creates
// consent, trusts request metadata, or applies migrations as a startup side effect.
func OpenRuntime(ctx context.Context, path, dsn string) (*grpc.Server, *pgxpool.Pool, string, error) {
	if path == "" || dsn == "" {
		return nil, nil, "", fmt.Errorf("config file and ANET_DATABASE_URL are required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, "", err
	}
	raw, err = canonical.Bytes(raw)
	if err != nil {
		return nil, nil, "", fmt.Errorf("invalid configuration JSON")
	}
	var c FileConfig
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil {
		return nil, nil, "", err
	}
	base := filepath.Dir(path)
	resolve := func(name string) string {
		if filepath.IsAbs(name) {
			return name
		}
		return filepath.Join(base, name)
	}
	cert, err := tls.LoadX509KeyPair(resolve(c.CertificateFile), resolve(c.PrivateKeyFile))
	if err != nil {
		return nil, nil, "", err
	}
	ca, err := os.ReadFile(resolve(c.ClientCAFile))
	if err != nil {
		return nil, nil, "", err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, nil, "", fmt.Errorf("client CA file has no certificates")
	}
	private, err := os.ReadFile(resolve(c.ICSSigningKeyFile))
	if err != nil {
		return nil, nil, "", err
	}
	block, rest := pem.Decode(private)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, nil, "", fmt.Errorf("ICS key must be one PKCS8 PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, "", fmt.Errorf("invalid ICS private key")
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, "", fmt.Errorf("ICS requires Ed25519")
	}
	public := key.Public().(ed25519.PublicKey)
	if c.ICSKeyID == "" {
		return nil, nil, "", fmt.Errorf("ics_key_id is required")
	}
	if c.ICSVerificationKeys == nil {
		c.ICSVerificationKeys = map[string]ed25519.PublicKey{}
	}
	if old, ok := c.ICSVerificationKeys[c.ICSKeyID]; ok && !old.Equal(public) {
		return nil, nil, "", fmt.Errorf("ICS key ID collides with a different public key")
	}
	c.ICSVerificationKeys[c.ICSKeyID] = public
	ops := map[string]tcs.Operation{}
	handlers := map[string]localtx.Handler{}
	for name, o := range c.Operations {
		if o.Capability == "local-tx" {
			if o.LocalHandler != "record.put/v1" {
				return nil, nil, "", fmt.Errorf("local-tx requires a registered local_handler")
			}
			handlers[name] = localadapter.Records{}
		} else if o.LocalHandler != "" {
			return nil, nil, "", fmt.Errorf("local_handler requires local-tx")
		}
		var ttl time.Duration
		if o.TargetTTL != "" {
			ttl, err = time.ParseDuration(o.TargetTTL)
			if err != nil {
				return nil, nil, "", fmt.Errorf("invalid target_ttl")
			}
		}
		ops[name] = tcs.Operation{Action: o.Action, Resource: o.Resource, Capability: o.Capability, Purpose: o.Purpose, Dimensions: o.Dimensions, TargetTTL: ttl, Sensitivity: o.Sensitivity, DataClass: o.DataClass}
	}
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, "", err
	}
	fail := func(err error) (*grpc.Server, *pgxpool.Pool, string, error) { db.Close(); return nil, nil, "", err }
	if _, err = db.Exec(ctx, `SELECT consent_id FROM root_consents LIMIT 0`); err != nil {
		return fail(fmt.Errorf("database unavailable or migrations incomplete"))
	}
	if _, err = db.Exec(ctx, `SELECT rpc_method,reason_code FROM audit_log LIMIT 0`); err != nil {
		return fail(fmt.Errorf("request audit migration incomplete"))
	}
	store, err := tcs.New(db, tcs.Config{Domain: c.Domain, CommandKeys: c.CommandKeys, RootKeys: c.ICSVerificationKeys, DecisionKeys: c.ICSVerificationKeys, ResultKeys: c.ResultKeys, Operations: ops, LocalHandlers: handlers})
	if err != nil {
		return fail(err)
	}
	policy, err := ics.New(db, ics.Config{Domain: c.Domain, KeyID: c.ICSKeyID, Key: key, CommandKeys: c.CommandKeys, Operations: ops})
	if err != nil {
		return fail(err)
	}
	var ingress *gateway.Service
	if c.GWKeyID != "" || c.GWSigningKeyFile != "" || c.GWCommandTTL != "" {
		if c.GWKeyID == "" || c.GWSigningKeyFile == "" {
			return fail(fmt.Errorf("GW key ID and signing key are required together"))
		}
		raw, e := os.ReadFile(resolve(c.GWSigningKeyFile))
		if e != nil {
			return fail(fmt.Errorf("cannot load GW signing key"))
		}
		block, rest := pem.Decode(raw)
		if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
			return fail(fmt.Errorf("GW key must be one PKCS8 PEM private key"))
		}
		parsed, e := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e != nil {
			return fail(fmt.Errorf("invalid GW signing key"))
		}
		gwKey, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			return fail(fmt.Errorf("GW requires Ed25519"))
		}
		trusted, ok := c.CommandKeys[c.GWKeyID]
		if !ok || !trusted.Equal(gwKey.Public()) || trusted.Equal(public) {
			return fail(fmt.Errorf("GW signing key must match its command trust entry and differ from ICS"))
		}
		var ttl time.Duration
		if c.GWCommandTTL != "" {
			ttl, e = time.ParseDuration(c.GWCommandTTL)
			if e != nil || ttl <= 0 {
				return fail(fmt.Errorf("invalid GW command TTL"))
			}
		}
		if _, e = db.Exec(ctx, `SELECT signed_command FROM gateway_commands LIMIT 0`); e != nil {
			return fail(fmt.Errorf("gateway migrations incomplete"))
		}
		ingress, e = gateway.New(db, store, policy, gateway.Config{Domain: c.Domain, KeyID: c.GWKeyID, Key: gwKey, CommandTTL: ttl})
		if e != nil {
			return fail(e)
		}
	}
	var renewal *credentialservice.Service
	if c.CredentialCAFile != "" || c.CredentialCAKeyFile != "" || c.CredentialTTL != "" {
		if c.CredentialCAFile == "" || c.CredentialCAKeyFile == "" {
			return fail(fmt.Errorf("renewal CA and key are required together"))
		}
		ca, e := tls.LoadX509KeyPair(resolve(c.CredentialCAFile), resolve(c.CredentialCAKeyFile))
		if e != nil {
			return fail(fmt.Errorf("cannot load renewal CA"))
		}
		ttl := 12 * time.Hour
		if c.CredentialTTL != "" {
			ttl, e = time.ParseDuration(c.CredentialTTL)
			if e != nil {
				return fail(fmt.Errorf("invalid credential TTL"))
			}
		}
		issuer, e := pki.New(ca, c.TrustDomain, ttl)
		if e != nil {
			return fail(e)
		}
		issuingCA, e := x509.ParseCertificate(ca.Certificate[0])
		if e != nil {
			return fail(e)
		}
		chain := x509.NewCertPool()
		for _, der := range ca.Certificate[1:] {
			cert, e := x509.ParseCertificate(der)
			if e != nil {
				return fail(e)
			}
			chain.AddCert(cert)
		}
		if _, e = issuingCA.Verify(x509.VerifyOptions{Roots: roots, Intermediates: chain, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); e != nil {
			return fail(fmt.Errorf("renewal CA is not trusted by client_ca_file"))
		}
		if _, e = db.Exec(ctx, `SELECT request_id FROM credential_renewals LIMIT 0`); e != nil {
			return fail(fmt.Errorf("renewal migration incomplete"))
		}
		renewal, e = credentialservice.New(db, c.Domain, issuer)
		if e != nil {
			return fail(e)
		}
	}
	server, err := New(db, store, policy, Config{Domain: c.Domain, TrustDomain: c.TrustDomain, Certificate: cert, ClientCAs: roots, Operations: ops, Gateway: ingress, Credentials: renewal})
	if err != nil {
		return fail(err)
	}
	if c.Listen == "" {
		server.Stop()
		return fail(fmt.Errorf("listen is required"))
	}
	return server, db, c.Listen, nil
}
