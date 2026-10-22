package pool

import (
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const DeepseekFreeModel = "deepseek-v4.1-flash"
const DeepseekPaidModel = "deepseek-v4.1-flash-sg"

// PickDeepseekPaidFallback only selects healthy global accounts with a known
// balance above the reserve. Check the free pool under the same lock so a
// restored free account wins over SG. No cooling-account probe or CN fallback.
func (p *Pool) PickDeepseekPaidFallback(tried map[string]bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if !p.deepseekFreeExhaustedLocked(now) {
		return nil
	}
	gate := reserveGate{p: p, active: true, line: p.reserveCredits, requireKnown: true}
	return p.pickHealthyLocked(tried, now, DeepseekPaidModel, "global", gate)
}

func (p *Pool) deepseekSGExhaustedLocked(now time.Time) bool {
	n := 0
	for _, e := range p.byUID {
		if e.disabled || e.manualDisabled || e.a.Realm() != "global" {
			continue
		}
		if e.outboundAvailable != nil && !e.outboundAvailable(e.a.UID) {
			return false // Do not escalate to paid CN while the fixed exit is down.
		}
		n++
		mc := e.modelCooldowns[DeepseekPaidModel]
		quotaRateLimited := !e.modelRateLimitUntil(now, DeepseekPaidModel).IsZero() && !isModelBusyReason(mc.Reason)
		if e.dailyTokenBlocked(now, DeepseekPaidModel) || quotaRateLimited {
			continue
		}
		known := e.creditsTotal > 0 || (e.a.IsEnterprise() && e.creditsTotal == -1)
		if known && e.credits <= p.reserveCredits {
			continue
		}
		return false // Busy, missing model, unknown balance and network faults are not exhaustion.
	}
	return n > 0
}

// DeepseekCNFallbackReady preserves the international free -> SG -> CN order.
// Only quota exhaustion or the known reserve floor allows the paid CN phase.
func (p *Pool) DeepseekCNFallbackReady() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	return p.deepseekFreeExhaustedLocked(now) && p.deepseekSGExhaustedLocked(now)
}

// PickDeepseekCNFallback rechecks the cheaper international options under the
// same lock as selection. CN's identically named 4.1 is paid: it must have a
// known balance above reserve, even though the bare ID is free in global.
func (p *Pool) PickDeepseekCNFallback(tried map[string]bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if !p.deepseekFreeExhaustedLocked(now) || !p.deepseekSGExhaustedLocked(now) {
		return nil
	}
	gate := reserveGate{p: p, active: true, line: p.reserveCredits, requireKnown: true}
	return p.pickHealthyLocked(tried, now, DeepseekFreeModel, "cn", gate)
}
