package authority

// The initial root profile uses exact resources/actions and zero delegation.
// Wildcards are never expanded; later delegation requires a separate chain check.
type Rights struct {
	Actions         []string `json:"actions"`
	Resources       []string `json:"resources"`
	DataClasses     []string `json:"data_classes"`
	DepthLeft       int      `json:"depth_left"`
	DelegationScope []string `json:"delegation_scope"`
	Spatial         Spatial  `json:"spatial"`
}
type Spatial struct {
	Domains              []string `json:"domains"`
	EgressOnlyViaGateway bool     `json:"egress_only_via_gateway"`
}

func Contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func set(values []string) bool {
	seen := map[string]bool{}
	for _, v := range values {
		if v == "" || seen[v] {
			return false
		}
		seen[v] = true
	}
	return len(values) > 0
}
func Valid(r Rights, domain string) bool {
	return set(r.Actions) && set(r.Resources) && set(r.DataClasses) && len(r.Spatial.Domains) == 1 && r.Spatial.Domains[0] == domain && r.Spatial.EgressOnlyViaGateway && r.DepthLeft == 0 && len(r.DelegationScope) == 0
}
func Subset(child, parent Rights, domain string) bool {
	if !Valid(child, domain) || !Valid(parent, domain) {
		return false
	}
	for _, pair := range [][2][]string{{child.Actions, parent.Actions}, {child.Resources, parent.Resources}, {child.DataClasses, parent.DataClasses}} {
		for _, v := range pair[0] {
			if !Contains(pair[1], v) {
				return false
			}
		}
	}
	return true
}
