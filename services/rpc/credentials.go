package rpc

import (
	"context"
	credentialsv1 "github.com/agentnet/agentnet/proto/gen/anet/credentials/v1"
	service "github.com/agentnet/agentnet/services/credentials"
	"github.com/agentnet/agentnet/services/tcs"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type renewals struct {
	credentialsv1.UnimplementedInstanceCredentialsServer
	service *service.Service
}

func credentialStatus(s service.Status) *credentialsv1.CredentialStatus {
	return &credentialsv1.CredentialStatus{InstanceId: s.Instance, Fingerprint: s.Fingerprint, Version: s.Version, ExpiresAt: timestamppb.New(s.Expires)}
}
func (r *renewals) GetStatus(ctx context.Context, in *credentialsv1.GetStatusRequest) (*credentialsv1.CredentialStatus, error) {
	c, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(in) {
		return nil, rpcError(tcs.Invalid)
	}
	out, err := r.service.GetStatus(ctx, c)
	if err != nil {
		return nil, rpcError(err)
	}
	return credentialStatus(out), nil
}
func (r *renewals) PrepareRenewal(ctx context.Context, in *credentialsv1.PrepareRenewalRequest) (*credentialsv1.PreparedCredential, error) {
	c, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(in) {
		return nil, rpcError(tcs.Invalid)
	}
	out, err := r.service.Prepare(ctx, c, in.RequestId, in.ExpectedVersion, in.CsrPem)
	if err != nil {
		return nil, rpcError(err)
	}
	return &credentialsv1.PreparedCredential{InstanceId: out.Instance, Version: out.Version, CertificatePem: out.PEM, ActivateBefore: timestamppb.New(out.ActivateBefore)}, nil
}
func (r *renewals) ActivateRenewal(ctx context.Context, in *credentialsv1.ActivateRenewalRequest) (*credentialsv1.CredentialStatus, error) {
	c, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(in) {
		return nil, rpcError(tcs.Invalid)
	}
	out, err := r.service.Activate(ctx, c, in.RequestId)
	if err != nil {
		return nil, rpcError(err)
	}
	return credentialStatus(out), nil
}
func (r *renewals) AbandonRenewal(ctx context.Context, in *credentialsv1.AbandonRenewalRequest) (*credentialsv1.CredentialStatus, error) {
	c, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(in) {
		return nil, rpcError(tcs.Invalid)
	}
	out, err := r.service.Abandon(ctx, c, in.RequestId, in.ExpectedVersion)
	if err != nil {
		return nil, rpcError(err)
	}
	return credentialStatus(out), nil
}
