package rpc

import (
	"context"
	"encoding/json"
	"time"

	"github.com/agentnet/agentnet/internal/canonical"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Handler errors may follow a committed transaction or a lost reply. Preserve
// that uncertainty instead of claiming no operation occurred. Never persist the
// request body, metadata, certificate, raw DB error, or error message here.
func (a Authenticator) auditFailure(ctx context.Context, id identity, method string, cause error) error {
	code := status.Code(cause)
	reason := code.String()
	for _, detail := range status.Convert(cause).Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.Domain == "agentnet" {
			reason = info.Reason
		}
	}
	decision := "deny"
	if code == codes.Internal || code == codes.Unavailable || code == codes.Canceled || code == codes.DeadlineExceeded || code == codes.Unknown {
		decision = "unknown"
	}
	payload, _ := json.Marshal(struct{ Method, Reason, Status string }{method, reason, code.String()})
	digest, err := canonical.Digest(payload)
	if err != nil {
		return status.Error(codes.Unavailable, "request audit unavailable")
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, err = a.DB.Exec(auditCtx, `INSERT INTO audit_log(domain_id,principal_id,agent_instance_id,action,decision,detail_digest,rpc_method,reason_code) VALUES($1,NULLIF($2,''),NULLIF($3,''),'rpc.request',$4,$5,$6,$7)`, a.Domain, id.caller.Principal, id.caller.Instance, decision, digest, method, reason)
	if err != nil {
		return status.Error(codes.Unavailable, "request audit unavailable")
	}
	return cause
}
