package pool

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// A fixed UTC+8 boundary avoids dependence on the container timezone or tzdata.
var DailyTokenLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

type DailyModelTokens struct {
	Day          string `json:"day"`
	Input        int64  `json:"input_tokens"`
	Output       int64  `json:"output_tokens"`
	Total        int64  `json:"total_tokens"`
	MissingUsage int64  `json:"missing_usage"`
}

type DailyModelQuota struct {
	Model string `json:"model"`
	Limit int64  `json:"limit"`
	DailyModelTokens
	Limited bool `json:"limited"`
}

func DailyTokenDay(now time.Time) string { return now.In(DailyTokenLocation).Format("2006-01-02") }
func DailyTokenReset(now time.Time) time.Time {
	t := now.In(DailyTokenLocation)
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, DailyTokenLocation)
}
func CanonicalTokenModel(model string) string {
	model = strings.TrimSpace(model)
	for _, prefix := range []string{"global:", "cn:"} {
		if strings.HasPrefix(model, prefix) {
			return strings.TrimPrefix(model, prefix)
		}
	}
	return model
}
func addTokens(a, b int64) int64 {
	if b <= 0 {
		return a
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
func (e *entry) dailyTokenBlocked(now time.Time, model string) bool {
	model = CanonicalTokenModel(model)
	limit := e.dailyTokenLimits[model]
	u := e.dailyModelTokens[model]
	return limit > 0 && u.Day == DailyTokenDay(now) && u.Total >= limit
}
func (e *entry) recordDailyTokens(now time.Time, d TokenUsageDelta) {
	model := CanonicalTokenModel(d.Model)
	if model == "" {
		return
	}
	day := DailyTokenDay(now)
	if e.dailyModelTokens == nil {
		e.dailyModelTokens = map[string]DailyModelTokens{}
	}
	// Keep just today's counters, including unlimited models so adding a limit
	// midway through the day cannot reset already recorded consumption.
	for m, u := range e.dailyModelTokens {
		if u.Day != day {
			delete(e.dailyModelTokens, m)
		}
	}
	u := e.dailyModelTokens[model]
	u.Day = day
	total := int64(0)
	known := false
	if d.HasPromptTokens && d.PromptTokens >= 0 {
		u.Input = addTokens(u.Input, d.PromptTokens)
		total = addTokens(total, d.PromptTokens)
		known = true
	}
	if d.HasCompletionTokens && d.CompletionTokens >= 0 {
		u.Output = addTokens(u.Output, d.CompletionTokens)
		total = addTokens(total, d.CompletionTokens)
		known = true
	}
	if d.HasTotalTokens && d.TotalTokens >= 0 {
		total = d.TotalTokens
		known = true
	}
	if known {
		u.Total = addTokens(u.Total, total)
	} else {
		u.MissingUsage++
	}
	e.dailyModelTokens[model] = u
}
func (e *entry) dailyQuotaView(now time.Time) []DailyModelQuota {
	day := DailyTokenDay(now)
	keys := map[string]bool{}
	for m := range e.dailyTokenLimits {
		keys[m] = true
	}
	for m, u := range e.dailyModelTokens {
		if u.Day == day {
			keys[m] = true
		}
	}
	rows := make([]DailyModelQuota, 0, len(keys))
	for m := range keys {
		u := e.dailyModelTokens[m]
		if u.Day != day {
			u = DailyModelTokens{Day: day}
		}
		rows = append(rows, DailyModelQuota{Model: m, Limit: e.dailyTokenLimits[m], DailyModelTokens: u, Limited: e.dailyTokenBlocked(now, m)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Model < rows[j].Model })
	return rows
}

// SetDailyTokenLimit changes one model without clearing its consumption or any
// other model's limits. Zero removes the limit. Save synchronously for restarts.
func (p *Pool) SetDailyTokenLimit(uid, model string, limit int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.byUID[uid]
	if e == nil {
		return fmt.Errorf("account not found")
	}
	if strings.Contains(model, ":") && !strings.HasPrefix(model, e.a.Realm()+":") {
		return fmt.Errorf("模型前缀与账号区域不一致")
	}
	model = CanonicalTokenModel(model)
	if model == "" || len(model) > 256 || strings.ContainsAny(model, " \t\r\n:") || limit < 0 || limit > 9000000000000000 {
		return fmt.Errorf("模型名或每日 Token 上限无效")
	}
	if e.dailyTokenLimits == nil {
		e.dailyTokenLimits = map[string]int64{}
	}
	if limit == 0 {
		delete(e.dailyTokenLimits, model)
	} else {
		if len(e.dailyTokenLimits) >= 256 && e.dailyTokenLimits[model] == 0 {
			return fmt.Errorf("单账号最多设置 256 个模型限制")
		}
		e.dailyTokenLimits[model] = limit
	}
	p.dirty.Store(true)
	p.saveLocked()
	return nil
}

// DailyTokenExhausted distinguishes local daily limits from upstream failures.
func (p *Pool) DailyTokenExhausted(model, realm string) (time.Time, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, e := range p.byUID {
		if e.disabled || e.manualDisabled || (realm != "" && e.a.Realm() != realm) {
			continue
		}
		n++
		if !e.dailyTokenBlocked(now, model) {
			return time.Time{}, false
		}
	}
	return DailyTokenReset(now), n > 0
}

// SeedDailyTokens recovers today's counters from usage.json on upgrade. Each
// field takes the larger persisted value; it never adds two copies of a request.
func (p *Pool) SeedDailyTokens(now time.Time, totals map[string]map[string]DailyModelTokens) {
	p.mu.Lock()
	defer p.mu.Unlock()
	day := DailyTokenDay(now)
	for uid, models := range totals {
		e := p.byUID[uid]
		if e == nil {
			continue
		}
		if e.dailyModelTokens == nil {
			e.dailyModelTokens = map[string]DailyModelTokens{}
		}
		for model, v := range models {
			if v.Day != day {
				continue
			}
			m := CanonicalTokenModel(model)
			u := e.dailyModelTokens[m]
			if u.Day != day {
				u = DailyModelTokens{Day: day}
			}
			if v.Input > u.Input {
				u.Input = v.Input
			}
			if v.Output > u.Output {
				u.Output = v.Output
			}
			if v.Total > u.Total {
				u.Total = v.Total
			}
			if v.MissingUsage > u.MissingUsage {
				u.MissingUsage = v.MissingUsage
			}
			e.dailyModelTokens[m] = u
		}
	}
	p.dirty.Store(true)
}

func (p *Pool) deepseekFreeExhaustedLocked(now time.Time) bool {
	n := 0
	for _, e := range p.byUID {
		if e.disabled || e.manualDisabled || e.a.Realm() != "global" {
			continue
		}
		if e.outboundAvailable != nil && !e.outboundAvailable(e.a.UID) {
			return false // An unavailable fixed exit is not free-quota exhaustion.
		}
		n++
		mc := e.modelCooldowns[DeepseekFreeModel]
		quotaRateLimited := !e.modelRateLimitUntil(now, DeepseekFreeModel).IsZero() && !isModelBusyReason(mc.Reason)
		if !e.dailyTokenBlocked(now, DeepseekFreeModel) && !quotaRateLimited {
			return false
		}
	}
	return n > 0
}
func (p *Pool) DeepseekFreeExhausted() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.deepseekFreeExhaustedLocked(time.Now())
}
