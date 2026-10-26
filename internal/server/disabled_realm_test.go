package server

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func TestDisabledRealmDoesNotOwnCatalogOrDefaultRoute(t *testing.T) {
	withGlobalEnabled(t)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "global", Domain: "www.workbuddy.ai"})
	p.Disable("global", "manual disable (panel)")
	h := NewHandler(Config{Pool: p, GlobalEnabled: true, RealmPrecedence: "global", StripRealmPrefix: true})
	if h.realmAvailable("global") || !h.realmAvailable("cn") {
		t.Fatal("disabled global account must not hide the remaining CN catalog")
	}
	if got := h.router.BareRealm(); got != "cn" {
		t.Fatalf("default route=%q want cn", got)
	}
	h.router.ModelRealms = func(model string) (bool, bool) {
		return model == "cn-only", model == "global-only"
	}
	if realm, _ := h.router.Resolve("global-only"); realm != "global" {
		t.Fatalf("known global-only model routed to unsupported realm %q", realm)
	}
	if realm, _ := h.router.Resolve("global:global-only"); realm != "global" {
		t.Fatal("an explicit realm prefix must remain explicit")
	}
}
