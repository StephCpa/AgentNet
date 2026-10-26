package authority

import (
	"sort"
	"time"
)

// Scope keeps the authoritative time interval explicit. Resource strings are
// exact vocabulary entries; no implicit wildcard or cross-domain mapping occurs.
type Scope struct {
	Rights               Rights
	NotBefore, ExpiresAt time.Time
}

const MaxChainLength = 64

func optionalSet(values []string) bool { return len(values) == 0 || set(values) }
func WellFormed(r Rights) bool {
	return set(r.Actions) && set(r.Resources) && set(r.DataClasses) && set(r.Spatial.Domains) && r.Spatial.EgressOnlyViaGateway && r.DepthLeft >= 0 && r.DepthLeft < MaxChainLength && optionalSet(r.DelegationScope) && (r.DepthLeft == 0 || len(r.DelegationScope) > 0)
}
func within(c, p []string) bool {
	for _, v := range c {
		if !Contains(p, v) {
			return false
		}
	}
	return true
}

// Within compares two ceilings, without consuming a delegation step (e.g. root
// grant versus user consent). Child creation must use Shrinks instead.
func Within(c, p Rights) bool {
	return WellFormed(c) && WellFormed(p) && c.DepthLeft <= p.DepthLeft &&
		within(c.Actions, p.Actions) && within(c.Resources, p.Resources) && within(c.DataClasses, p.DataClasses) && within(c.Spatial.Domains, p.Spatial.Domains) && within(c.DelegationScope, p.DelegationScope)
}
func Shrinks(parent, child Scope) bool {
	return Within(child.Rights, parent.Rights) && child.Rights.DepthLeft < parent.Rights.DepthLeft &&
		!parent.NotBefore.IsZero() && parent.ExpiresAt.After(parent.NotBefore) && !child.NotBefore.Before(parent.NotBefore) && child.ExpiresAt.After(child.NotBefore) && !child.ExpiresAt.After(parent.ExpiresAt)
}
func intersection(a, b []string) []string {
	out := []string{}
	for _, v := range a {
		if Contains(b, v) {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// Attenuate rejects a proposal that expands its parent before intersecting the
// recipient ceiling. All three inputs must come from the creator's trusted
// transaction context; a caller-supplied parent/policy is not authorization.
func Attenuate(parent, proposal, recipient Scope) (Scope, bool) {
	if !Shrinks(parent, proposal) || !WellFormed(recipient.Rights) || recipient.NotBefore.IsZero() || !recipient.ExpiresAt.After(recipient.NotBefore) {
		return Scope{}, false
	}
	out := proposal
	out.Rights.Actions = intersection(proposal.Rights.Actions, recipient.Rights.Actions)
	out.Rights.Resources = intersection(proposal.Rights.Resources, recipient.Rights.Resources)
	out.Rights.DataClasses = intersection(proposal.Rights.DataClasses, recipient.Rights.DataClasses)
	out.Rights.Spatial.Domains = intersection(proposal.Rights.Spatial.Domains, recipient.Rights.Spatial.Domains)
	out.Rights.DelegationScope = intersection(proposal.Rights.DelegationScope, recipient.Rights.DelegationScope)
	out.Rights.DepthLeft = min(proposal.Rights.DepthLeft, recipient.Rights.DepthLeft)
	if recipient.NotBefore.After(out.NotBefore) {
		out.NotBefore = recipient.NotBefore
	}
	if recipient.ExpiresAt.Before(out.ExpiresAt) {
		out.ExpiresAt = recipient.ExpiresAt
	}
	if !Shrinks(parent, out) {
		return Scope{}, false
	}
	return out, true
}
