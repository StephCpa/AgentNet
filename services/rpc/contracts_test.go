package rpc_test

import (
	"encoding/json"
	"testing"

	icsv1 "github.com/agentnet/agentnet/proto/gen/anet/ics/v1"
	"google.golang.org/grpc/codes"
)

func TestRPCContractAdmissionGuards(t *testing.T) {
	for _, name := range []string{"null-schema", "invalid-schema", "empty-acceptance", "unknown-check", "no-contract-permission"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			var body map[string]any
			json.Unmarshal(f.command.Contract, &body)
			want := codes.InvalidArgument
			switch name {
			case "null-schema":
				body["output_schema"] = nil
			case "invalid-schema":
				body["output_schema"] = map[string]any{"type": "bogus"}
			case "empty-acceptance":
				body["acceptance_policy"] = map[string]any{}
			case "unknown-check":
				body["acceptance_policy"].(map[string]any)["checks"] = []string{"trust-worker"}
			case "no-contract-permission":
				f.command.Root.Rights.DepthLeft = 1
				f.command.Root.Rights.DelegationScope = []string{"compute"}
				body["delegation_policy"] = map[string]any{"allow": false}
				want = codes.PermissionDenied
			}
			f.command.Contract, _ = json.Marshal(body)
			_, err := f.policy.IssueRootPermit(f.ctx, &icsv1.IssueRootPermitRequest{SignedCommand: f.commandEnvelope(f.command)})
			code(t, err, want)
			g := gatewayClient(t, f, 0)
			_, err = g.SubmitCommand(f.ctx, intent(f))
			code(t, err, want)
			if f.count("SELECT count(*) FROM tasks WHERE domain_id=$1", f.domain) != 0 || f.count("SELECT count(*) FROM audit_log WHERE domain_id=$1 AND action='authz.root_permit'", f.domain) != 0 {
				t.Fatal("invalid contract obtained permission/admission")
			}
		})
	}
}
