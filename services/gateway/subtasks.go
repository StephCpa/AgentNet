package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/agentnet/agentnet/internal/authority"
	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/agentnet/agentnet/internal/signed"
	"github.com/agentnet/agentnet/services/tcs"
)

// SubtaskIntent contains proposed delegation constraints, never credentials or
// a trusted command lifetime. TCS independently authorizes every new creation.
type SubtaskIntent struct {
	Kind              string                           `json:"kind"`
	ID                string                           `json:"command_id"`
	Parent            string                           `json:"parent_task_id"`
	Grant             string                           `json:"parent_grant_id"`
	Lease             string                           `json:"lease_id"`
	Epoch             int64                            `json:"epoch"`
	ExpectedVersion   int64                            `json:"expected_task_version"`
	RecipientPolicy   string                           `json:"recipient_policy_id"`
	RecipientAgent    string                           `json:"recipient_agent_id"`
	RecipientInstance string                           `json:"recipient_instance_id"`
	Contract          json.RawMessage                  `json:"contract"`
	Rights            authority.Rights                 `json:"rights"`
	NotBefore         time.Time                        `json:"not_before"`
	ExpiresAt         time.Time                        `json:"expires_at"`
	Allocations       map[string]tcs.SubtaskAllocation `json:"allocations"`
}

func (s *Service) CreateSubtask(ctx context.Context, c tcs.Caller, raw []byte) (tcs.Admission, error) {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return tcs.Admission{}, tcs.Invalid
	}
	canonicalRaw, err := canonical.Bytes(raw)
	if err != nil {
		return tcs.Admission{}, tcs.Invalid
	}
	var r SubtaskIntent
	d := json.NewDecoder(bytes.NewReader(canonicalRaw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || r.Kind != "anet.subtask-intent/1" || r.ID == "" || len(r.ID) > 256 || r.Parent == "" || r.Grant == "" || r.Lease == "" || r.Epoch < 1 || r.ExpectedVersion < 1 || r.RecipientPolicy == "" || r.RecipientAgent == "" || r.RecipientInstance == "" || r.NotBefore.IsZero() || !r.ExpiresAt.After(r.NotBefore) || len(r.Allocations) != 4 {
		return tcs.Admission{}, tcs.Invalid
	}
	command, _, err := s.bindCommand(ctx, c, canonicalRaw, r.ID, func(expires time.Time) (tcs.Envelope, error) {
		cmd := tcs.SubtaskCommand{ID: r.ID, Audience: s.cfg.Domain, NotAfter: expires, Parent: r.Parent, Grant: r.Grant, Lease: r.Lease, Epoch: r.Epoch, ExpectedVersion: r.ExpectedVersion, RecipientPolicy: r.RecipientPolicy, RecipientAgent: r.RecipientAgent, RecipientInstance: r.RecipientInstance, Contract: r.Contract, Rights: r.Rights, NotBefore: r.NotBefore, ExpiresAt: r.ExpiresAt, Allocations: r.Allocations}
		return signed.Encode(s.cfg.KeyID, s.cfg.Key, tcs.SubtaskSubmission{Kind: "anet.subtask/1", Caller: c, Command: cmd})
	})
	if err != nil {
		return tcs.Admission{}, err
	}
	// A GW signature is an authenticated proposal, not a delegation approval.
	// Child authorization comes from TCS's locked parent chain and recipient policy;
	// no new root permit is issued and the original user subject is preserved.
	return s.store.CreateSubtask(ctx, c, command)
}
