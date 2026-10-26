package authority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"time"

	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/jackc/pgx/v5"
)

var ErrInvalidChain = errors.New("invalid or inactive authority chain")

type chainNode struct {
	Grant                               Grant  `json:"grant"`
	Root                                string `json:"root"`
	Parent                              string `json:"parent"`
	State                               string `json:"-"`
	Revoked                             bool   `json:"-"`
	Domain, TaskParent, TaskRoot, Owner string `json:"-"`
}
type Chain struct {
	Leaf      Grant
	ExpiresAt time.Time
	notBefore time.Time
	nodes     []chainNode
}

func (c *Chain) ValidAt(now time.Time) bool {
	return !now.Before(c.notBefore) && now.Before(c.ExpiresAt)
}
func (c *Chain) Live(ctx context.Context, tx pgx.Tx) (bool, error) {
	var now time.Time
	err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now)
	return err == nil && c.ValidAt(now), err
}
func (c *Chain) Digest() (string, error) {
	// Preserve existing root decision/effect digests across this upgrade.
	if len(c.nodes) == 1 {
		return c.Leaf.Digest()
	}
	raw, err := json.Marshal(struct {
		Format string      `json:"format"`
		Path   []chainNode `json:"path"`
	}{"anet.grant-chain/1", c.nodes})
	if err != nil {
		return "", err
	}
	return canonical.Digest(raw)
}
func strictRights(raw []byte) (Rights, error) {
	var r Rights
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return r, ErrInvalidChain
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return r, ErrInvalidChain
	}
	return r, nil
}
func node(ctx context.Context, tx pgx.Tx, id string) (chainNode, error) {
	var n chainNode
	var raw []byte
	g := &n.Grant
	err := tx.QueryRow(ctx, `SELECT g.grant_id,g.task_id,g.subject_principal_id,g.actor_agent_id,COALESCE(g.actor_instance_id,''),g.rights,g.not_before,g.expires_at,g.policy_version,COALESCE(g.consent_id,''),COALESCE(g.consent_version,0),g.root_grant_id,COALESCE(g.parent_grant_id,''),g.status,g.revoked_at IS NOT NULL,t.domain_id,COALESCE(t.parent_task_id,''),t.root_task_id,t.owner_principal_id
 FROM grants g JOIN tasks t USING(task_id) WHERE g.grant_id=$1`, id).Scan(&g.ID, &g.Task, &g.Subject, &g.Agent, &g.Instance, &raw, &g.NotBefore, &g.ExpiresAt, &g.Policy, &g.ConsentID, &g.ConsentVersion, &n.Root, &n.Parent, &n.State, &n.Revoked, &n.Domain, &n.TaskParent, &n.TaskRoot, &n.Owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return n, ErrInvalidChain
	}
	if err != nil {
		return n, err
	}
	g.Rights, err = strictRights(raw)
	g.NotBefore, g.ExpiresAt = g.NotBefore.UTC(), g.ExpiresAt.UTC()
	return n, err
}

// LockChain discovers a bounded acyclic path, locks every grant in ID order,
// reloads the path, then locks its root consent. Callers already hold the leaf
// task lock and must call Live after subsequent waits before authorizing work.
// Legacy consent-less fixtures may use a single root only; they cannot delegate.
func LockChain(ctx context.Context, tx pgx.Tx, id string, requireConsent bool) (*Chain, error) {
	ids := []string{}
	seen := map[string]bool{}
	for id != "" {
		if seen[id] || len(ids) >= MaxChainLength {
			return nil, ErrInvalidChain
		}
		seen[id] = true
		ids = append(ids, id)
		var parent string
		err := tx.QueryRow(ctx, "SELECT COALESCE(parent_grant_id,'') FROM grants WHERE grant_id=$1", id).Scan(&parent)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidChain
		}
		if err != nil {
			return nil, err
		}
		id = parent
	}
	if len(ids) == 0 {
		return nil, ErrInvalidChain
	}
	ordered := append([]string(nil), ids...)
	sort.Strings(ordered)
	for _, id := range ordered {
		var locked string
		err := tx.QueryRow(ctx, "SELECT grant_id FROM grants WHERE grant_id=$1 FOR SHARE", id).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidChain
		}
		if err != nil {
			return nil, err
		}
	}
	c := &Chain{}
	for i, id := range ids {
		n, err := node(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		parent := ""
		if i+1 < len(ids) {
			parent = ids[i+1]
		}
		if n.Parent != parent || n.Root != ids[len(ids)-1] || n.State != "active" || n.Revoked || n.Owner != n.Grant.Subject || n.Grant.Policy == "" {
			return nil, ErrInvalidChain
		}
		c.nodes = append(c.nodes, n)
	}
	c.Leaf = c.nodes[0].Grant
	root := c.nodes[len(c.nodes)-1]
	c.notBefore, c.ExpiresAt = c.Leaf.NotBefore, c.Leaf.ExpiresAt
	if root.Grant.ConsentID == "" {
		if requireConsent || len(ids) != 1 || root.Grant.Rights.DepthLeft != 0 || len(root.Grant.Rights.DelegationScope) != 0 {
			return nil, ErrInvalidChain
		}
	} else {
		var raw []byte
		var before, end time.Time
		var subject, agent, instance, domain, policy string
		err := tx.QueryRow(ctx, `SELECT rights,not_before,expires_at,subject_principal_id,actor_agent_id,COALESCE(actor_instance_id,''),domain_id,policy_version FROM root_consents
 WHERE consent_id=$1 AND version=$2 AND revoked_at IS NULL FOR SHARE`, root.Grant.ConsentID, root.Grant.ConsentVersion).Scan(&raw, &before, &end, &subject, &agent, &instance, &domain, &policy)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidChain
		}
		if err != nil {
			return nil, err
		}
		ceiling, err := strictRights(raw)
		if err != nil {
			return nil, err
		}
		g := root.Grant
		if root.TaskParent != "" || root.TaskRoot != g.Task || domain != root.Domain || subject != g.Subject || agent != g.Agent || (instance != "" && instance != g.Instance) || policy != g.Policy || !Within(g.Rights, ceiling) || g.NotBefore.Before(before) || g.ExpiresAt.After(end) {
			return nil, ErrInvalidChain
		}
		for i, n := range c.nodes {
			g := n.Grant
			if !WellFormed(g.Rights) || n.Domain != domain || !Contains(g.Rights.Spatial.Domains, domain) || n.TaskRoot != root.Grant.Task || g.Subject != subject || g.ConsentID != root.Grant.ConsentID || g.ConsentVersion != root.Grant.ConsentVersion {
				return nil, ErrInvalidChain
			}
			if i+1 < len(c.nodes) {
				p := c.nodes[i+1]
				if n.TaskParent != p.Grant.Task || !Shrinks(Scope{p.Grant.Rights, p.Grant.NotBefore, p.Grant.ExpiresAt}, Scope{g.Rights, g.NotBefore, g.ExpiresAt}) {
					return nil, ErrInvalidChain
				}
			}
			if g.NotBefore.After(c.notBefore) {
				c.notBefore = g.NotBefore
			}
			if g.ExpiresAt.Before(c.ExpiresAt) {
				c.ExpiresAt = g.ExpiresAt
			}
		}
		if before.After(c.notBefore) {
			c.notBefore = before
		}
		if end.Before(c.ExpiresAt) {
			c.ExpiresAt = end
		}
	}
	live, err := c.Live(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !live {
		return nil, ErrInvalidChain
	}
	return c, nil
}
