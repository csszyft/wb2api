package server

import "strings"

// splitRealmPrefix 解析显式 realm 前缀协议（PLAN D6）："[realm:]model"。
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离（大小写敏感）；否则 ok=false 并原串返回。
// 非 "cn"/"global" 的前段（含冒号的模型名）整体视为裸名，不会被误切成域前缀。
func splitRealmPrefix(model string) (realm, bare string, ok bool) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "", model, false
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "", model, false
	}
	return prefix, model[idx+1:], true
}

// RealmRouter 模型名 → realm 归属决策的唯一真相（handler 路由与 cmd 粘性闭包共用）。
//
// 规则：
//  1. 显式 "cn:"/"global:" 前缀优先——老客户端配置里写死的带前缀模型名零改动。
//  2. 裸名若只出现在一个域的目录中，直接归属该域（即使该域账号暂不可用，
//     也不把已知国际版模型错发给 CN 上游）。
//  3. 两域共有或归属未知时，按池内可用域与 Precedence 决定。
//
// 单域部署（例如只登国际版账号）下裸名直接落到该域；配合 strip_realm_prefix
// 即得到"输出无前缀、且裸名路由正确"的语义。
type RealmRouter struct {
	// GlobalEnabled 与 handler 第三道闸同源（config global.enabled）。
	// false 时 global 域视为不存在（纯 CN 锁定逃生门）。
	GlobalEnabled bool
	// Precedence applies to shared/unknown models when both realms have accounts.
	Precedence string
	// HasRealm reports whether a realm has a selectable (not disabled) account.
	HasRealm func(realm string) bool
	// ModelRealms reads the shared catalog snapshots without network calls.
	ModelRealms func(model string) (cn, global bool)
}

// Resolve 解析模型名为 (realm, bareModel)。bare 用于选号/粘性/出站 body 重写
// （前缀是网关侧路由协议，上游只认裸名）。
func (r RealmRouter) Resolve(model string) (realm, bare string) {
	realm, bare, _ = r.ResolveWithSource(model)
	return realm, bare
}

// ResolveWithSource 同 Resolve，并额外报告 realm 是否来自显式前缀（用户强指定）。
//
// explicit only records the client's prefix. Use ResolveRoute's restricted flag
// to decide fallback permission, since exclusive catalog groups also stay fixed.
func (r RealmRouter) ResolveWithSource(model string) (realm, bare string, explicit bool) {
	realm, bare, explicit, _ = r.ResolveRoute(model)
	return realm, bare, explicit
}

func normalizeModelAlias(bare string) string {
	if strings.EqualFold(strings.TrimSpace(bare), "deepseek-v4.1-flash-1017") {
		return "deepseek-v4.1-flash"
	}
	return bare
}

// ResolveRoute snapshots grouping and fallback permission in the same lookup.
func (r RealmRouter) ResolveRoute(model string) (realm, bare string, explicit, restricted bool) {
	if realm, bare, ok := splitRealmPrefix(model); ok {
		return realm, normalizeModelAlias(bare), true, true
	}
	model = normalizeModelAlias(model)
	if realm := r.CatalogRealm(model); realm != "" {
		return realm, model, false, true
	}
	return r.BareRealm(), model, false, false
}

// CatalogRealm assigns models listed in just one catalog to that realm.
// Shared and unknown models keep the configured realm precedence.
func (r RealmRouter) CatalogRealm(model string) string {
	if r.ModelRealms == nil || !r.GlobalEnabled {
		return ""
	}
	cn, global := r.ModelRealms(model)
	if cn && !global {
		return "cn"
	}
	if global && !cn {
		return "global"
	}
	return ""
}

// BareRealm returns the default realm for shared or unknown bare model names.
func (r RealmRouter) BareRealm() string {
	has := func(realm string) bool { return r.HasRealm != nil && r.HasRealm(realm) }
	cn, gl := has("cn"), r.GlobalEnabled && has("global")
	switch {
	case gl && !cn:
		return "global" // 单域：只登国际版账号 → 裸名全走国际版
	case cn && !gl:
		return "cn"
	}
	if r.Precedence == "cn" {
		return "cn"
	}
	return "global"
}
