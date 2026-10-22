package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const probeTimeout = 12 * time.Second

func canonicalIP(value string) string {
	ip := net.ParseIP(strings.TrimSpace(value))
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
		return ""
	}
	return ip.String()
}

func responseIP(raw []byte) string {
	if ip := canonicalIP(string(raw)); ip != "" {
		return ip
	}
	var response struct {
		IP     string `json:"ip"`
		Origin string `json:"origin"`
		Query  string `json:"query"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return ""
	}
	for _, value := range []string{response.IP, response.Origin, response.Query} {
		if ip := canonicalIP(value); ip != "" {
			return ip
		}
	}
	return ""
}

func (m *Manager) SetProbeTransport(base *http.Transport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if base != nil {
		m.probeBase = base.Clone()
	}
}

func (m *Manager) Check(ctx context.Context, id string) error {
	m.mu.RLock()
	i := proxyIndex(&m.state, id)
	if i < 0 {
		m.mu.RUnlock()
		return errors.New("代理不存在")
	}
	raw := m.state.Proxies[i].URL
	base := m.probeBase
	urls := append([]string(nil), m.probeURLs...)
	m.mu.RUnlock()
	u, err := ParseURL(raw)
	if err != nil {
		return err
	}
	if base == nil {
		base = http.DefaultTransport.(*http.Transport)
	}
	tr := base.Clone()
	tr.Proxy = http.ProxyURL(TransportURL(u))
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	started := time.Now()
	ip := ""
	lastErr := "出口检测失败：代理连接或检测服务不可用"
	for _, endpoint := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/json, text/plain")
		req.Header.Set("User-Agent", "wb2api-egress-check/1.0")
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				lastErr = "出口检测超时或已取消"
				break
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8193))
		resp.Body.Close()
		if readErr != nil || len(body) > 8192 || resp.StatusCode != http.StatusOK {
			lastErr = fmt.Sprintf("出口检测失败：检测服务 HTTP %d", resp.StatusCode)
			continue
		}
		ip = responseIP(body)
		if ip != "" {
			break
		}
		lastErr = "出口检测失败：检测服务未返回有效公网 IP"
	}
	m.mu.Lock()
	i = proxyIndex(&m.state, id)
	if i < 0 || m.state.Proxies[i].URL != raw {
		m.mu.Unlock()
		return errors.New("检测期间代理配置已变化，请重新检测")
	}
	next := cloneState(m.state)
	p := &next.Proxies[i]
	previousIP := p.ExitIP
	p.CheckedAt = time.Now()
	p.LatencyMS = time.Since(started).Milliseconds()
	if ip == "" {
		p.CheckError = lastErr
	} else {
		for j, other := range next.Proxies {
			if j != i && other.ExitIP == ip {
				p.Capacity = other.Capacity
				break
			}
		}
		p.ExitIP, p.CheckError = ip, ""
		delete(m.blocks, "proxy:"+id)
	}
	saveErr := m.saveLocked(next)
	hook := m.onRouteChange
	m.mu.Unlock()
	if saveErr != nil {
		return saveErr
	}
	if previousIP != ip && ip != "" && hook != nil {
		hook(id)
	}
	if ip == "" {
		return errors.New(lastErr)
	}
	return nil
}
