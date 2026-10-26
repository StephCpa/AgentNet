package authority

import (
	"encoding/json"
	"testing"
	"time"
)

func scopes() (Scope, Scope, Scope) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := Scope{Rights{Actions: []string{"read", "write"}, Resources: []string{"tool:a", "tool:b"}, DataClasses: []string{"public", "internal"}, DepthLeft: 3, DelegationScope: []string{"compute", "search"}, Spatial: Spatial{[]string{"a", "b"}, true}}, start, start.Add(time.Hour)}
	c := p
	c.Rights.DepthLeft = 2
	r := p
	r.Rights.Actions = []string{"read"}
	r.Rights.Resources = []string{"tool:a"}
	r.Rights.DataClasses = []string{"public"}
	r.Rights.Spatial.Domains = []string{"b"}
	r.Rights.DelegationScope = []string{"compute"}
	r.Rights.DepthLeft = 1
	r.NotBefore = start.Add(time.Minute)
	r.ExpiresAt = start.Add(30 * time.Minute)
	return p, c, r
}
func cloneScope(s Scope) Scope {
	raw, _ := json.Marshal(s)
	var out Scope
	_ = json.Unmarshal(raw, &out)
	return out
}
func TestDelegationIntersection(t *testing.T) {
	p, c, r := scopes()
	out, ok := Attenuate(p, c, r)
	if !ok || !Shrinks(p, out) || !Within(out.Rights, r.Rights) || out.Rights.DepthLeft != 1 || out.NotBefore != r.NotBefore || out.ExpiresAt != r.ExpiresAt || len(out.Rights.Actions) != 1 || len(c.Rights.Actions) != 2 {
		t.Fatal("intersection failed or mutated proposal", out, ok)
	}
}
func TestDelegationRejectsEveryExpansion(t *testing.T) {
	p, c, r := scopes()
	edits := map[string]func(*Scope){"actions": func(x *Scope) { x.Rights.Actions = []string{"admin"} }, "resources": func(x *Scope) { x.Rights.Resources = []string{"secret"} }, "classes": func(x *Scope) { x.Rights.DataClasses = []string{"secret"} }, "domains": func(x *Scope) { x.Rights.Spatial.Domains = []string{"foreign"} }, "egress": func(x *Scope) { x.Rights.Spatial.EgressOnlyViaGateway = false }, "depth": func(x *Scope) { x.Rights.DepthLeft = 3 }, "scope": func(x *Scope) { x.Rights.DelegationScope = []string{"admin"} }, "early": func(x *Scope) { x.NotBefore = p.NotBefore.Add(-time.Second) }, "late": func(x *Scope) { x.ExpiresAt = p.ExpiresAt.Add(time.Second) }, "empty": func(x *Scope) { x.Rights.Actions = nil }, "duplicate": func(x *Scope) { x.Rights.Actions = []string{"read", "read"} }, "negative": func(x *Scope) { x.Rights.DepthLeft = -1 }}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			bad := cloneScope(c)
			edit(&bad)
			if _, ok := Attenuate(p, bad, r); ok {
				t.Fatal("expanded proposal accepted")
			}
		})
	}
}
func TestDelegationRecipientCannotRestoreRights(t *testing.T) {
	p, c, r := scopes()
	r.Rights.Actions = []string{"admin"}
	if _, ok := Attenuate(p, c, r); ok {
		t.Fatal("empty intersection accepted")
	}
	p, c, r = scopes()
	r.NotBefore = p.ExpiresAt
	if _, ok := Attenuate(p, c, r); ok {
		t.Fatal("empty time intersection accepted")
	}
	p, c, r = scopes()
	p.Rights.Resources = []string{"tool:*"}
	c.Rights.Resources = []string{"tool:a"}
	if _, ok := Attenuate(p, c, r); ok {
		t.Fatal("implicit wildcard mapping accepted")
	}
}
func TestChainDigestBindsAncestorsAndPreservesRoot(t *testing.T) {
	p, c, _ := scopes()
	root := Grant{ID: "root", Rights: p.Rights, NotBefore: p.NotBefore, ExpiresAt: p.ExpiresAt}
	leaf := Grant{ID: "leaf", Rights: c.Rights, NotBefore: c.NotBefore, ExpiresAt: c.ExpiresAt}
	one := &Chain{Leaf: root, nodes: []chainNode{{Grant: root}}}
	old, _ := root.Digest()
	got, _ := one.Digest()
	if got != old {
		t.Fatal("root digest changed")
	}
	chain := &Chain{Leaf: leaf, nodes: []chainNode{{Grant: leaf, Root: "root", Parent: "root"}, {Grant: root, Root: "root"}}}
	before, _ := chain.Digest()
	chain.nodes[1].Grant.Policy = "changed"
	after, _ := chain.Digest()
	if before == after {
		t.Fatal("ancestor not bound")
	}
}
func TestChainRejectsUnknownRightsFields(t *testing.T) {
	if _, err := DecodeRights([]byte(`{"actions":["read"],"temporal":{"expires_at":"hidden"}}`)); err == nil {
		t.Fatal("unknown constraint silently discarded")
	}
}
