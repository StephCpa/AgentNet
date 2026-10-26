package rpc

import (
	"context"
	gwv1 "github.com/agentnet/agentnet/proto/gen/anet/gw/v1"
	tcsv1 "github.com/agentnet/agentnet/proto/gen/anet/tcs/v1"
	"github.com/agentnet/agentnet/services/gateway"
	"github.com/agentnet/agentnet/services/tcs"
)

type ingress struct {
	gwv1.UnimplementedGatewayServer
	service *gateway.Service
	control *control
}

func (g *ingress) SubmitCommand(ctx context.Context, r *gwv1.SubmitCommandRequest) (*tcsv1.AdmissionResult, error) {
	who, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(r) {
		return nil, rpcError(tcs.Invalid)
	}
	out, err := g.service.Submit(ctx, who, r.IntentJson)
	if err != nil {
		return nil, rpcError(err)
	}
	return g.control.admissionResult(ctx, who, out)
}

func (g *ingress) CreateSubtask(ctx context.Context, r *gwv1.CreateSubtaskRequest) (*tcsv1.AdmissionResult, error) {
	who, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if !strict(r) {
		return nil, rpcError(tcs.Invalid)
	}
	out, err := g.service.CreateSubtask(ctx, who, r.IntentJson)
	if err != nil {
		return nil, rpcError(err)
	}
	return g.control.admissionResult(ctx, who, out)
}
