package authority

// The single-domain profile uses exact rights and explicit delegation ceilings.
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
	return WellFormed(r) && len(r.Spatial.Domains) == 1 && r.Spatial.Domains[0] == domain
}
func Subset(child, parent Rights, domain string) bool {
	return Valid(child, domain) && Valid(parent, domain) && Within(child, parent)
}
