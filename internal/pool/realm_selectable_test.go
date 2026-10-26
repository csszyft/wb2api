package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestHasSelectableRealmIgnoresDisabledButNotCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "global", Domain: "www.workbuddy.ai"})
	p.Disable("global", "disabled")
	if !p.HasSelectableRealm("cn") || p.HasSelectableRealm("global") {
		t.Fatal("disabled accounts must not keep a realm selectable")
	}
	if !p.HasRealm("global") {
		t.Fatal("membership must remain available for diagnostics")
	}
	p.Cooldown("cn", CoolSoft, time.Minute, "temporary")
	if !p.HasSelectableRealm("cn") {
		t.Fatal("temporary cooldown must not flap catalog and route ownership")
	}
}
