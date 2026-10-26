// Package rpc exposes the TCS and ICS kernels through authenticated gRPC.
package rpc

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/agentnet/agentnet/services/tcs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type authKey struct{}
type identity struct {
	caller tcs.Caller
	roles  []string
}
type Authenticator struct {
	DB                  *pgxpool.Pool
	Domain, TrustDomain string
}

func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func (a Authenticator) authenticate(ctx context.Context) (identity, error) {
	var id identity
	p, ok := peer.FromContext(ctx)
	if !ok {
		return id, status.Error(codes.Unauthenticated, "mTLS required")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || info.State.Version < tls.VersionTLS13 || len(info.State.VerifiedChains) == 0 || len(info.State.PeerCertificates) == 0 {
		return id, status.Error(codes.Unauthenticated, "verified client certificate required")
	}
	leaf := info.State.PeerCertificates[0]
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || leaf.NotAfter.Sub(leaf.NotBefore) > 24*time.Hour || len(leaf.URIs) != 1 {
		return id, status.Error(codes.Unauthenticated, "invalid instance certificate")
	}
	var registered string
	err := a.DB.QueryRow(ctx, `SELECT p.principal_id,a.agent_id,i.instance_id,ARRAY(SELECT role FROM instance_roles r WHERE r.instance_id=i.instance_id),COALESCE(i.spiffe_id,'')
 FROM agent_instances i JOIN agents a ON a.agent_id=i.agent_id JOIN principals p ON p.principal_id=a.owner_principal_id
 WHERE i.cred_hash=$1 AND a.domain_id=$2 AND p.domain_id=$2 AND i.revoked_at IS NULL AND p.revoked_at IS NULL AND a.status='active'`, Fingerprint(leaf), a.Domain).Scan(&id.caller.Principal, &id.caller.Agent, &id.caller.Instance, &id.roles, &registered)
	if errors.Is(err, pgx.ErrNoRows) {
		return id, status.Error(codes.Unauthenticated, "unregistered or revoked instance")
	}
	if err != nil {
		return id, status.Error(codes.Unauthenticated, "identity lookup failed")
	}
	if registered != leaf.URIs[0].String() {
		return id, status.Error(codes.Unauthenticated, "registered URI mismatch")
	}
	id.caller.CredentialHash = Fingerprint(leaf)
	id.caller.CredentialExpiresAt = &leaf.NotAfter
	u := leaf.URIs[0]
	if u.Scheme != "spiffe" || u.Host != a.TrustDomain || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" || u.Path != "/agent-instance/"+id.caller.Instance {
		return id, status.Error(codes.Unauthenticated, "instance URI mismatch")
	}
	return id, nil
}

func restrict(id identity, method string) error {
	if strings.HasSuffix(method, "/ReportEffectResult") || strings.HasSuffix(method, "/ListUnknownEffects") {
		for _, role := range id.roles {
			if role == "adapter" {
				return nil
			}
		}
		return status.Error(codes.PermissionDenied, "adapter role required")
	}
	return nil
}
func (a Authenticator) Unary(ctx context.Context, request any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
	id, err := a.authenticate(ctx)
	if err != nil {
		return nil, a.auditFailure(ctx, identity{}, info.FullMethod, err)
	}
	if err = restrict(id, info.FullMethod); err != nil {
		return nil, a.auditFailure(ctx, id, info.FullMethod, err)
	}
	response, err := next(context.WithValue(ctx, authKey{}, id), request)
	if err != nil {
		return nil, a.auditFailure(ctx, id, info.FullMethod, err)
	}
	return response, nil
}

type authenticatedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authenticatedStream) Context() context.Context { return s.ctx }
func (a Authenticator) Stream(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
	id, err := a.authenticate(stream.Context())
	if err != nil {
		return a.auditFailure(stream.Context(), identity{}, info.FullMethod, err)
	}
	if err = restrict(id, info.FullMethod); err != nil {
		return a.auditFailure(stream.Context(), id, info.FullMethod, err)
	}
	err = next(srv, &authenticatedStream{stream, context.WithValue(stream.Context(), authKey{}, id)})
	if err != nil {
		return a.auditFailure(stream.Context(), id, info.FullMethod, err)
	}
	return nil
}
func caller(ctx context.Context) (tcs.Caller, error) {
	id, ok := ctx.Value(authKey{}).(identity)
	if !ok {
		return tcs.Caller{}, status.Error(codes.Unauthenticated, "authenticated context required")
	}
	return id.caller, nil
}
