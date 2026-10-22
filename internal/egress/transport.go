package egress

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type accountContextKey struct{}

func WithAccount(req *http.Request, uid string) *http.Request {
	if req == nil || uid == "" {
		return req
	}
	return req.WithContext(context.WithValue(req.Context(), accountContextKey{}, uid))
}

// Transport isolates each sticky proxy's connection pool. The proxy resolver
// overrides environment proxies for bound accounts and never falls back after
// a proxy failure. Unbound behavior is controlled by the manager settings.
type Transport struct {
	base    http.RoundTripper
	manager *Manager
	mu      sync.Mutex
	byProxy map[string]*http.Transport
}

func NewTransport(base http.RoundTripper, manager *Manager) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{base: base, manager: manager, byProxy: map[string]*http.Transport{}}
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	uid, _ := req.Context().Value(accountContextKey{}).(string)
	if uid == "" || t.manager == nil || !t.manager.Enabled() {
		return t.base.RoundTrip(req)
	}
	route, proxyURL, err := t.manager.ProxyURL(uid)
	if err != nil {
		return nil, err
	}
	if route.Direct {
		return t.base.RoundTrip(req)
	}
	t.mu.Lock()
	tr := t.byProxy[route.ProxyID]
	if tr == nil {
		base, ok := t.base.(*http.Transport)
		if !ok {
			t.mu.Unlock()
			return nil, &RequestError{Code: "proxy_transport", Message: "当前出站传输器不支持账号代理"}
		}
		tr = base.Clone()
		tr.Proxy = http.ProxyURL(proxyURL)
		t.byProxy[route.ProxyID] = tr
	}
	t.mu.Unlock()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		tr.CloseIdleConnections()
		if req.Context().Err() == nil {
			t.manager.Block("proxy:"+route.ProxyID, time.Now().Add(30*time.Second))
		}
		return nil, &RequestError{Code: "proxy_connection", Message: fmt.Sprintf("%s 的代理连接失败，请检测代理或稍后重试", route.ProxyName), Cause: err}
	}
	if resp.StatusCode == http.StatusProxyAuthRequired {
		resp.Body.Close()
		t.manager.Block("proxy:"+route.ProxyID, time.Now().Add(30*time.Second))
		return nil, &RequestError{Code: "proxy_auth", Message: "代理认证失败，请检查代理用户名和密码"}
	}
	return resp, nil
}

func (t *Transport) Invalidate(id string) {
	t.mu.Lock()
	tr := t.byProxy[id]
	delete(t.byProxy, id)
	t.mu.Unlock()
	if tr != nil {
		tr.CloseIdleConnections()
	}
}

func (t *Transport) CloseIdleConnections() {
	if base, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		base.CloseIdleConnections()
	}
	t.mu.Lock()
	for _, tr := range t.byProxy {
		tr.CloseIdleConnections()
	}
	t.mu.Unlock()
}

// HTTP1Transport preserves account routing when chat temporarily disables h2.
func (t *Transport) HTTP1Transport() http.RoundTripper {
	base, ok := t.base.(*http.Transport)
	if !ok {
		return t
	}
	clone := base.Clone()
	clone.ForceAttemptHTTP2 = false
	clone.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	if clone.TLSClientConfig != nil {
		clone.TLSClientConfig = clone.TLSClientConfig.Clone()
		clone.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	return NewTransport(clone, t.manager)
}
