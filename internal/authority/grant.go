package authority

import (
	"context"
	"encoding/json"
	"time"

	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/jackc/pgx/v5"
)

type Grant struct {
	ID             string    `json:"grant_id"`
	Task           string    `json:"task_id"`
	Subject        string    `json:"subject"`
	Agent          string    `json:"actor_agent"`
	Instance       string    `json:"actor_instance"`
	Rights         Rights    `json:"rights"`
	NotBefore      time.Time `json:"not_before"`
	ExpiresAt      time.Time `json:"expires_at"`
	Policy         string    `json:"policy_version"`
	ConsentID      string    `json:"consent_id"`
	ConsentVersion int64     `json:"consent_version"`
}

func LoadGrant(ctx context.Context, tx pgx.Tx, id string) (Grant, error) {
	var g Grant
	var b []byte
	err := tx.QueryRow(ctx, `SELECT grant_id,task_id,subject_principal_id,actor_agent_id,COALESCE(actor_instance_id,''),rights,not_before,expires_at,policy_version,COALESCE(consent_id,''),COALESCE(consent_version,0) FROM grants WHERE grant_id=$1`, id).Scan(&g.ID, &g.Task, &g.Subject, &g.Agent, &g.Instance, &b, &g.NotBefore, &g.ExpiresAt, &g.Policy, &g.ConsentID, &g.ConsentVersion)
	if err != nil {
		return g, err
	}
	err = json.Unmarshal(b, &g.Rights)
	g.NotBefore = g.NotBefore.UTC()
	g.ExpiresAt = g.ExpiresAt.UTC()
	return g, err
}
func (g Grant) Digest() (string, error) {
	b, err := json.Marshal(g)
	if err != nil {
		return "", err
	}
	return canonical.Digest(b)
}
