package egress

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const Direct = "direct"

type Settings struct {
	Enabled            bool `json:"enabled"`
	AutoAssign         bool `json:"auto_assign"`
	AllowDirectUnbound bool `json:"allow_direct_unbound"`
	DefaultCapacity    int  `json:"default_capacity"`
}

type Proxy struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	URL        string    `json:"url"` // Stored only in the protected proxy file.
	Enabled    bool      `json:"enabled"`
	Capacity   int       `json:"capacity"`
	ExitIP     string    `json:"exit_ip,omitempty"`
	CheckedAt  time.Time `json:"checked_at,omitempty"`
	CheckError string    `json:"check_error,omitempty"`
	LatencyMS  int64     `json:"latency_ms,omitempty"`
}

type fileState struct {
	Version  int               `json:"version"`
	Settings Settings          `json:"settings"`
	Proxies  []Proxy           `json:"proxies"`
	Bindings map[string]string `json:"bindings"`
}

type Account struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Realm    string `json:"realm,omitempty"`
}

type Route struct {
	Binding      string    `json:"binding"`
	ProxyID      string    `json:"proxy_id,omitempty"`
	ProxyName    string    `json:"proxy_name,omitempty"`
	ExitIP       string    `json:"exit_ip,omitempty"`
	Key          string    `json:"-"`
	Direct       bool      `json:"direct"`
	Available    bool      `json:"available"`
	Status       string    `json:"status"`
	Message      string    `json:"message,omitempty"`
	CoolingUntil time.Time `json:"cooling_until,omitempty"`
}

type ProxyView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	MaskedURL   string    `json:"masked_url"`
	Enabled     bool      `json:"enabled"`
	Capacity    int       `json:"capacity"`
	ExitIP      string    `json:"exit_ip,omitempty"`
	CheckedAt   time.Time `json:"checked_at,omitempty"`
	LatencyMS   int64     `json:"latency_ms,omitempty"`
	Status      string    `json:"status"`
	Message     string    `json:"message,omitempty"`
	Bound       int       `json:"bound"`
	IPBound     int       `json:"ip_bound"`
	AccountUIDs []string  `json:"account_uids"`
}

type Snapshot struct {
	Settings  Settings         `json:"settings"`
	Proxies   []ProxyView      `json:"proxies"`
	Routes    map[string]Route `json:"routes"`
	Accounts  []Account        `json:"accounts"`
	UniqueIPs int              `json:"unique_ips"`
	Bound     int              `json:"bound"`
	Unbound   int              `json:"unbound"`
	Job       *Job             `json:"job,omitempty"`
}

type Manager struct {
	mu            sync.RWMutex
	state         fileState
	path          string
	accounts      []Account
	blocks        map[string]time.Time
	index         map[string]int
	boundCounts   map[string]int
	capacities    map[string]int
	probeBase     *http.Transport
	probeURLs     []string
	onRouteChange func(string)

	jobMu     sync.Mutex
	job       *Job
	jobCancel func()
	jobDone   chan struct{}
}

func New(path string) (*Manager, error) {
	m := &Manager{
		path:      path,
		state:     fileState{Version: 1, Settings: Settings{AutoAssign: true, DefaultCapacity: 1}, Bindings: map[string]string{}},
		blocks:    make(map[string]time.Time),
		probeURLs: []string{"https://api.ipify.org?format=json", "https://ifconfig.me/ip"},
	}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, errors.New("无法读取代理池文件")
		}
		if len(raw) > 4<<20 {
			return nil, errors.New("代理池文件过大")
		}
		if len(raw) > 0 {
			if json.Unmarshal(raw, &m.state) != nil {
				return nil, errors.New("代理池文件不是有效 JSON")
			}
			if m.state.Version != 1 || len(m.state.Proxies) > MaxProxies || !validCapacity(m.state.Settings.DefaultCapacity) {
				return nil, errors.New("代理池版本、容量或代理数量无效")
			}
			seen := map[string]bool{}
			for i := range m.state.Proxies {
				p := &m.state.Proxies[i]
				u, err := ParseURL(p.URL)
				if err != nil || p.ID == "" || p.ID == Direct || seen[p.ID] || !validCapacity(p.Capacity) {
					return nil, errors.New("代理池包含无效或重复的代理条目")
				}
				p.URL = u.String()
				if p.ExitIP != "" {
					p.ExitIP = canonicalIP(p.ExitIP)
					if p.ExitIP == "" {
						return nil, errors.New("代理池包含无效出口 IP")
					}
				}
				seen[p.ID] = true
			}
		}
	}
	if m.state.Bindings == nil {
		m.state.Bindings = map[string]string{}
	}
	m.rebuildLookupLocked()
	return m, nil
}

func (m *Manager) rebuildLookupLocked() {
	m.index = make(map[string]int, len(m.state.Proxies))
	m.capacities = make(map[string]int)
	m.boundCounts = make(map[string]int)
	for i, p := range m.state.Proxies {
		m.index[p.ID] = i
		if p.ExitIP != "" && (m.capacities[p.ExitIP] == 0 || p.Capacity < m.capacities[p.ExitIP]) {
			m.capacities[p.ExitIP] = p.Capacity
		}
	}
	for _, id := range m.state.Bindings {
		if i, ok := m.index[id]; ok && m.state.Proxies[i].ExitIP != "" {
			m.boundCounts[m.state.Proxies[i].ExitIP]++
		}
	}
}

func validCapacity(n int) bool { return n >= 1 && n <= 10000 }

func cloneState(s fileState) fileState {
	out := s
	out.Proxies = append([]Proxy(nil), s.Proxies...)
	out.Bindings = make(map[string]string, len(s.Bindings))
	for uid, id := range s.Bindings {
		out.Bindings[uid] = id
	}
	return out
}

func (m *Manager) saveLocked(next fileState) error {
	if m.path == "" {
		m.state = next
		m.rebuildLookupLocked()
		return nil
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return errors.New("无法编码代理池配置")
	}
	dir := filepath.Dir(m.path)
	if os.MkdirAll(dir, 0o700) != nil {
		return errors.New("无法创建代理池目录")
	}
	f, err := os.CreateTemp(dir, ".proxy-pool-*.tmp")
	if err != nil {
		return errors.New("无法创建代理池临时文件")
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(append(raw, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("代理池写入失败，设置未生效")
	}
	if os.Rename(tmp, m.path) != nil {
		return errors.New("代理池保存失败，设置未生效")
	}
	m.state = next
	m.rebuildLookupLocked()
	return nil
}

func (m *Manager) Enabled() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state.Settings.Enabled
}

func (m *Manager) UpdateSettings(settings Settings) error {
	if !validCapacity(settings.DefaultCapacity) {
		return errors.New("每个 IP 的默认账号容量必须在 1–10000 之间")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := cloneState(m.state)
	next.Settings = settings
	if settings.Enabled && settings.AutoAssign {
		assignLocked(&next, m.accounts, false)
	}
	return m.saveLocked(next)
}

func (m *Manager) Import(urls []string, capacity int) (ids []string, added, duplicate int, err error) {
	if !validCapacity(capacity) {
		return nil, 0, 0, errors.New("每个 IP 的账号容量必须在 1–10000 之间")
	}
	parsed := make([]string, len(urls))
	for i, raw := range urls {
		u, err := ParseURL(raw)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("第 %d 项：%s", i+1, err)
		}
		parsed[i] = u.String()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := cloneState(m.state)
	seen := map[string]string{}
	for _, p := range next.Proxies {
		seen[p.URL] = p.ID
	}
	uniqueIDs := map[string]bool{}
	for _, raw := range parsed {
		id := seen[raw]
		if id == "" {
			if len(next.Proxies) >= MaxProxies {
				return nil, 0, 0, fmt.Errorf("代理池最多保存 %d 条链接", MaxProxies)
			}
			id = proxyID(raw)
			name := fmt.Sprintf("出口 %d", len(next.Proxies)+1)
			if u, err := ParseURL(raw); err == nil && u.User != nil && u.User.Username() != "" {
				name = u.User.Username()
				if len([]rune(name)) > 60 {
					name = string([]rune(name)[:60])
				}
			}
			next.Proxies = append(next.Proxies, Proxy{ID: id, Name: name, URL: raw, Enabled: true, Capacity: capacity})
			seen[raw] = id
			added++
		} else {
			duplicate++
		}
		if !uniqueIDs[id] {
			ids = append(ids, id)
			uniqueIDs[id] = true
		}
	}
	if err := m.saveLocked(next); err != nil {
		return nil, 0, 0, err
	}
	return ids, added, duplicate, nil
}

func proxyIndex(s *fileState, id string) int {
	for i := range s.Proxies {
		if s.Proxies[i].ID == id {
			return i
		}
	}
	return -1
}

func capacityForIP(s *fileState, ip string) int {
	capacity := 0
	for _, p := range s.Proxies {
		if p.ExitIP == ip && (capacity == 0 || p.Capacity < capacity) {
			capacity = p.Capacity
		}
	}
	return capacity
}

func ipCounts(s *fileState) map[string]int {
	counts := map[string]int{}
	for _, id := range s.Bindings {
		if i := proxyIndex(s, id); i >= 0 && s.Proxies[i].ExitIP != "" {
			counts[s.Proxies[i].ExitIP]++
		}
	}
	return counts
}

type AssignResult struct {
	Assigned int      `json:"assigned"`
	Kept     int      `json:"kept"`
	Unbound  []string `json:"unbound"`
}

// Fill in account order, grouping links by their measured IP. Existing fixed
// bindings are preserved unless the caller explicitly requests redistribution.
func assignLocked(s *fileState, accounts []Account, replace bool) AssignResult {
	out := AssignResult{Unbound: []string{}}
	if replace {
		for _, a := range accounts {
			delete(s.Bindings, a.UID)
		}
	}
	counts := ipCounts(s)
	for _, a := range accounts {
		if a.UID == "" {
			continue
		}
		if s.Bindings[a.UID] != "" {
			out.Kept++
			continue
		}
		assigned := false
		for _, p := range s.Proxies {
			if !p.Enabled || p.ExitIP == "" || p.CheckError != "" || p.CheckedAt.IsZero() || counts[p.ExitIP] >= capacityForIP(s, p.ExitIP) {
				continue
			}
			s.Bindings[a.UID] = p.ID
			counts[p.ExitIP]++
			out.Assigned++
			assigned = true
			break
		}
		if !assigned {
			out.Unbound = append(out.Unbound, a.UID)
		}
	}
	return out
}

// SyncAccounts is called outside the account-pool lock. Removed accounts release
// their reserved slots; new accounts can receive a verified free slot.
func (m *Manager) SyncAccounts(accounts []Account) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts = append([]Account(nil), accounts...)
	next := cloneState(m.state)
	known := map[string]bool{}
	for _, a := range accounts {
		known[a.UID] = true
	}
	changed := false
	for uid := range next.Bindings {
		if !known[uid] {
			delete(next.Bindings, uid)
			changed = true
		}
	}
	if next.Settings.Enabled && next.Settings.AutoAssign {
		if result := assignLocked(&next, accounts, false); result.Assigned > 0 {
			changed = true
		}
	}
	if changed {
		return m.saveLocked(next)
	}
	return nil
}

func (m *Manager) Assign(accounts []Account, replace bool) (AssignResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := cloneState(m.state)
	known := make(map[string]bool, len(m.accounts))
	for _, account := range m.accounts {
		known[account.UID] = true
	}
	for _, account := range accounts {
		if !known[account.UID] {
			return AssignResult{}, errors.New("所选账号已被移除，请刷新账号列表")
		}
	}
	result := assignLocked(&next, accounts, replace)
	return result, m.saveLocked(next)
}

func (m *Manager) Bind(uid, id string) error {
	if uid == "" {
		return errors.New("缺少账号标识")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	known := false
	for _, account := range m.accounts {
		if account.UID == uid {
			known = true
			break
		}
	}
	if !known {
		return errors.New("账号已被移除，请刷新账号列表")
	}
	next := cloneState(m.state)
	if id == "" {
		delete(next.Bindings, uid)
	} else if id == Direct {
		next.Bindings[uid] = Direct
	} else {
		i := proxyIndex(&next, id)
		if i < 0 {
			return errors.New("代理不存在")
		}
		p := next.Proxies[i]
		if !p.Enabled || p.ExitIP == "" || p.CheckError != "" || p.CheckedAt.IsZero() {
			return errors.New("请先成功检测并启用这个代理")
		}
		if next.Bindings[uid] != id {
			delete(next.Bindings, uid)
			if ipCounts(&next)[p.ExitIP] >= capacityForIP(&next, p.ExitIP) {
				return errors.New("该出口 IP 已达到账号容量")
			}
			next.Bindings[uid] = id
		}
	}
	return m.saveLocked(next)
}

func (m *Manager) UpdateProxy(id, name string, capacity int, enabled bool) error {
	if !validCapacity(capacity) || len([]rune(name)) > 80 || strings.ContainsAny(name, "\r\n") {
		return errors.New("代理名称或容量无效")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := cloneState(m.state)
	i := proxyIndex(&next, id)
	if i < 0 {
		return errors.New("代理不存在")
	}
	ip := next.Proxies[i].ExitIP
	if ip != "" && ipCounts(&next)[ip] > capacity {
		return errors.New("新容量低于该出口已绑定账号数，请先调整绑定")
	}
	if ip != "" {
		for j := range next.Proxies {
			if next.Proxies[j].ExitIP == ip {
				next.Proxies[j].Capacity = capacity
			}
		}
	}
	next.Proxies[i].Name = strings.TrimSpace(name)
	next.Proxies[i].Capacity = capacity
	next.Proxies[i].Enabled = enabled
	return m.saveLocked(next)
}

func (m *Manager) RemoveProxy(id string) error {
	m.mu.Lock()
	next := cloneState(m.state)
	i := proxyIndex(&next, id)
	if i < 0 {
		m.mu.Unlock()
		return errors.New("代理不存在")
	}
	for _, binding := range next.Bindings {
		if binding == id {
			m.mu.Unlock()
			return errors.New("此代理仍绑定账号，请先调整账号出口再移除")
		}
	}
	next.Proxies = append(next.Proxies[:i], next.Proxies[i+1:]...)
	err := m.saveLocked(next)
	hook := m.onRouteChange
	m.mu.Unlock()
	if err == nil && hook != nil {
		hook(id)
	}
	return err
}

func (m *Manager) routeLocked(uid string) Route {
	s := &m.state
	id := s.Bindings[uid]
	if !s.Settings.Enabled {
		r := Route{Binding: id, Direct: true, Available: true, Key: Direct, Status: "off", Message: "代理池未启用，使用原出口"}
		if i, ok := m.index[id]; ok {
			r.ProxyID, r.ProxyName, r.ExitIP = id, s.Proxies[i].Name, s.Proxies[i].ExitIP
		}
		return r
	}
	if id == Direct || (id == "" && s.Settings.AllowDirectUnbound) {
		r := Route{Binding: id, Direct: true, Available: true, Key: Direct, Status: "direct", Message: "使用原出口"}
		if until := m.blocks[Direct]; time.Now().Before(until) {
			r.Available, r.Status, r.Message, r.CoolingUntil = false, "cooling", "原出口暂时冷却", until
		}
		return r
	}
	if id == "" {
		return Route{Status: "unbound", Message: "账号未绑定代理，等待分配出口"}
	}
	i, exists := m.index[id]
	if !exists {
		return Route{Binding: id, ProxyID: id, Status: "missing", Message: "绑定的代理不存在"}
	}
	p := s.Proxies[i]
	r := Route{Binding: id, ProxyID: id, ProxyName: p.Name, ExitIP: p.ExitIP, Key: "ip:" + p.ExitIP, Status: "ready", Available: true}
	if !p.Enabled {
		r.Available, r.Status, r.Message = false, "disabled", "绑定的代理已停用"
	} else if p.CheckedAt.IsZero() || p.ExitIP == "" {
		r.Available, r.Status, r.Message = false, "unchecked", "绑定的代理尚未成功检测出口 IP"
	} else if p.CheckError != "" {
		r.Available, r.Status, r.Message = false, "failed", p.CheckError
	} else if m.boundCounts[p.ExitIP] > m.capacities[p.ExitIP] {
		r.Available, r.Status, r.Message = false, "over_capacity", "出口 IP 合并后超过容量，请重新分配"
	} else if until := m.blocks[r.Key]; time.Now().Before(until) {
		r.Available, r.Status, r.Message, r.CoolingUntil = false, "cooling", "出口 IP 暂时冷却", until
	} else if until := m.blocks["proxy:"+id]; time.Now().Before(until) {
		r.Available, r.Status, r.Message, r.CoolingUntil = false, "cooling", "代理连接暂时不可用", until
	}
	return r
}

func (m *Manager) Route(uid string) Route {
	if m == nil {
		return Route{Direct: true, Available: true, Key: Direct, Status: "off"}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.routeLocked(uid)
}

func (m *Manager) Available(uid string) bool { return m.Route(uid).Available }

func (m *Manager) ProxyURL(uid string) (Route, *url.URL, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r := m.routeLocked(uid)
	if !r.Available {
		return r, nil, &RequestError{Code: "proxy_" + r.Status, Message: r.Message}
	}
	if r.Direct {
		return r, nil, nil
	}
	i := proxyIndex(&m.state, r.ProxyID)
	u, err := ParseURL(m.state.Proxies[i].URL)
	if err != nil {
		return r, nil, &RequestError{Code: "proxy_invalid", Message: "绑定的代理地址无效"}
	}
	return r, TransportURL(u), nil
}

func (m *Manager) Block(key string, until time.Time) {
	if m == nil || key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for old, deadline := range m.blocks {
		if !time.Now().Before(deadline) {
			delete(m.blocks, old)
		}
	}
	if until.After(m.blocks[key]) {
		m.blocks[key] = until
	}
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.RLock()
	s := m.state
	out := Snapshot{Settings: s.Settings, Proxies: []ProxyView{}, Routes: map[string]Route{}, Accounts: append([]Account(nil), m.accounts...)}
	counts := ipCounts(&s)
	ips := map[string]bool{}
	for _, p := range s.Proxies {
		v := ProxyView{ID: p.ID, Name: p.Name, MaskedURL: maskedURL(p.URL), Enabled: p.Enabled, Capacity: p.Capacity, ExitIP: p.ExitIP, CheckedAt: p.CheckedAt, LatencyMS: p.LatencyMS, Status: "ready", AccountUIDs: []string{}}
		for uid, id := range s.Bindings {
			if id == p.ID {
				v.AccountUIDs = append(v.AccountUIDs, uid)
			}
		}
		sort.Strings(v.AccountUIDs)
		v.Bound, v.IPBound = len(v.AccountUIDs), counts[p.ExitIP]
		if p.ExitIP != "" {
			v.Capacity = capacityForIP(&s, p.ExitIP)
		}
		if !p.Enabled {
			v.Status = "disabled"
		} else if p.CheckError != "" {
			v.Status, v.Message = "failed", p.CheckError
		} else if p.ExitIP == "" || p.CheckedAt.IsZero() {
			v.Status = "unchecked"
		} else if v.IPBound > v.Capacity {
			v.Status, v.Message = "over_capacity", "该 IP 已绑定账号超过容量"
		} else if time.Now().Before(m.blocks["ip:"+p.ExitIP]) || time.Now().Before(m.blocks["proxy:"+p.ID]) {
			v.Status = "cooling"
		}
		if p.Enabled && p.ExitIP != "" && p.CheckError == "" {
			ips[p.ExitIP] = true
		}
		out.Proxies = append(out.Proxies, v)
	}
	for _, a := range m.accounts {
		out.Routes[a.UID] = m.routeLocked(a.UID)
		if id := s.Bindings[a.UID]; id != "" && id != Direct {
			out.Bound++
		} else {
			out.Unbound++
		}
	}
	out.UniqueIPs = len(ips)
	m.mu.RUnlock()
	out.Job = m.JobSnapshot()
	return out
}

func (m *Manager) SetRouteChangeHook(hook func(string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onRouteChange = hook
}

// RequestError distinguishes unavailable proxy routes from account faults.
// Message never contains the provider's authentication URL.
type RequestError struct {
	Code    string
	Message string
	Cause   error
}

func (e *RequestError) Error() string { return e.Message }
func (e *RequestError) Unwrap() error { return e.Cause }
