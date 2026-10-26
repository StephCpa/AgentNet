package identity_test

import (
	"github.com/agentnet/agentnet/services/identity"
	"strings"
	"testing"
)

func TestIdentityDelegationConsentVisible(t *testing.T) {
	f := setup(t)
	p := f.cfg.Profiles["research"]
	p.Rights.DepthLeft = 2
	p.Rights.DelegationScope = []string{"compute"}
	f.cfg.Profiles["research"] = p
	service, err := identity.New(f.ctx, f.db, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.server.Config.Handler = service
	csrf := f.login()
	_, page := f.get(f.server.URL+"/", 200)
	if !strings.Contains(page, "最多 2 层") || !strings.Contains(page, "compute") || strings.Contains(page, "不允许向其他 Agent 委托") {
		t.Fatal("consent page hid delegation", page)
	}
	h := f.post("/consents", f.form(csrf), f.server.URL, 303)
	consent := h.Get("X-AgentNet-Consent")
	if f.count(`SELECT count(*) FROM root_consents WHERE consent_id=$1 AND rights->>'depth_left'='2' AND rights->'delegation_scope' ? 'compute'`, consent) != 1 {
		t.Fatal("approved delegation not persisted")
	}
	_, page = f.get(f.server.URL+"/", 200)
	if strings.Count(page, "最多 2 层") != 2 {
		t.Fatal("stored consent delegation missing")
	}
}
