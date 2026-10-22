package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestOutboundGateAppliesToSelectionStickyFallbackAndHealth(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetPickMode(PickSequential)
	p.SetOrder([]string{"a", "b"})
	p.SetOutboundAvailable(func(uid string) bool { return uid == "b" })
	if a := p.Pick(); a == nil || a.UID != "b" {
		t.Fatal("unavailable exit selected before ready exit")
	}
	if p.PickByUID("a") != nil || p.PickByUIDForModel("a", "model") != nil {
		t.Fatal("sticky binding bypassed exit availability")
	}
	p.Cooldown("a", CoolSoft, time.Hour, "old soft lock")
	p.Disable("b", "test")
	if p.Pick() != nil {
		t.Fatal("cooling fallback bypassed exit availability")
	}
	if p.ServableNow() {
		t.Fatal("health reported ready when no exit can serve")
	}
	p.SetOutboundAvailable(nil)
	if p.Pick() == nil {
		t.Fatal("turning off proxy gating did not restore original behavior")
	}
}

func TestAccountHookKeepsFreshRosterAndOrderAfterConcurrentChanges(t *testing.T) {
	p := New("")
	var mu sync.Mutex
	roster := []string{}
	p.SetAccountChangeHook(func(accounts []*auth.Auth) {
		// Account hooks run outside Pool's mutex.
		p.List()
		uids := []string{}
		for _, a := range accounts {
			uids = append(uids, a.UID)
		}
		mu.Lock()
		roster = uids
		mu.Unlock()
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); p.Add(&auth.Auth{UID: fmt.Sprintf("u%02d", i)}) }(i)
	}
	wg.Wait()
	mu.Lock()
	n := len(roster)
	mu.Unlock()
	if n != 20 {
		t.Fatalf("stale hook snapshot overwrote final roster: %d", n)
	}
	p.SetOrder([]string{"u19", "u03"})
	mu.Lock()
	first, second := roster[0], roster[1]
	mu.Unlock()
	if first != "u19" || second != "u03" {
		t.Fatal("proxy assignment order did not follow account order")
	}
	p.Remove("u19")
	mu.Lock()
	defer mu.Unlock()
	if len(roster) != 19 || roster[0] != "u03" {
		t.Fatal("removed account retained outbound slot")
	}
}

func TestProxyUnavailableCannotTriggerPaidQuotaFallback(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "global", Domain: "www.workbuddy.ai"})
	p.CooldownSoftForModel("global", time.Minute, time.Now().Add(time.Hour), DeepseekFreeModel, "6004 model rate limit")
	p.CooldownSoftForModel("global", time.Minute, time.Now().Add(time.Hour), DeepseekPaidModel, "6004 model rate limit")
	p.SetCredits("global", 100, 1000)
	if !p.DeepseekFreeExhausted() || !p.DeepseekCNFallbackReady() {
		t.Fatal("quota positive control failed")
	}
	p.SetOutboundAvailable(func(string) bool { return false })
	if p.DeepseekFreeExhausted() || p.DeepseekCNFallbackReady() || p.PickDeepseekPaidFallback(nil) != nil || p.PickDeepseekCNFallback(nil) != nil {
		t.Fatal("proxy failure was treated as quota exhaustion")
	}
	if _, ok := p.ModelRateLimitExhausted(DeepseekFreeModel, "global"); ok {
		t.Fatal("proxy failure was reported as quota retry condition")
	}
}
