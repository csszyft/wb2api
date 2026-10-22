package pool

import "github.com/linguo2625469/workbuddy2api-panel/internal/auth"

// Hooks execute after the pool lock is released. The outbound predicate is
// read-only and must never call back into Pool; selection/health share it.
func (p *Pool) SetOutboundAvailable(fn func(string) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outboundAvailable = fn
	for _, e := range p.byUID {
		e.outboundAvailable = fn
	}
}

func (p *Pool) SetAccountChangeHook(hook func([]*auth.Auth)) {
	p.mu.Lock()
	p.accountChangeHook = hook
	p.mu.Unlock()
	p.NotifyAccountChange()
}

// Serialize notifications and take a fresh snapshot after acquiring the hook
// lock, so concurrent imports cannot deliver an older roster after a newer one.
func (p *Pool) NotifyAccountChange() bool {
	p.accountHookMu.Lock()
	defer p.accountHookMu.Unlock()
	p.mu.RLock()
	hook, accounts := p.accountChangeLocked()
	p.mu.RUnlock()
	if hook == nil {
		return false
	}
	hook(accounts)
	return true
}

func (p *Pool) accountChangeLocked() (func([]*auth.Auth), []*auth.Auth) {
	if p.accountChangeHook == nil {
		return nil, nil
	}
	uids := p.effectiveOrderLocked()
	accounts := make([]*auth.Auth, 0, len(uids))
	for _, uid := range uids {
		accounts = append(accounts, p.byUID[uid].a)
	}
	return p.accountChangeHook, accounts
}
