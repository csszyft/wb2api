package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
)

func TestAccountProxyCoversChatRefreshBillingModelsAndProfile(t *testing.T) {
	var mu sync.Mutex
	paths := map[string]int{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() || r.URL.Host != "workbuddy-test.invalid" {
			t.Errorf("request bypassed proxy: %s", r.URL)
		}
		mu.Lock()
		paths[r.URL.Path]++
		mu.Unlock()
		if strings.Contains(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		if strings.Contains(r.URL.Path, "/auth/token/refresh") {
			io.WriteString(w, `{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}}`)
			return
		}
		if r.URL.Path == accountProfilePath {
			io.WriteString(w, `{"code":0,"data":{"uid":"a","nickname":"Proxy account"}}`)
			return
		}
		if strings.Contains(r.URL.Path, "/models") {
			io.WriteString(w, `{"code":0,"data":{"models":[{"id":"glm-5.2"}]}}`)
			return
		}
		if r.URL.Path == "/v3/config" {
			io.WriteString(w, `{"code":0,"data":{}}`)
			return
		}
		io.WriteString(w, `{"code":0,"data":{}}`)
	}))
	defer proxy.Close()
	path := filepath.Join(t.TempDir(), "proxy-pool.json")
	raw, _ := json.Marshal(map[string]any{"version": 1, "settings": egress.Settings{Enabled: true, DefaultCapacity: 1}, "proxies": []map[string]any{{"id": "px-a", "name": "proxy-a", "url": proxy.URL, "enabled": true, "capacity": 1, "exit_ip": "8.8.8.8", "checked_at": time.Now()}}, "bindings": map[string]string{"a": "px-a"}})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := egress.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.SyncAccounts([]egress.Account{{UID: "a"}}); err != nil {
		t.Fatal(err)
	}
	c := New()
	c.ChatBaseCN, c.BillingBaseCN, c.WebBaseCN = "http://workbuddy-test.invalid", "http://workbuddy-test.invalid", "http://workbuddy-test.invalid"
	c.HTTP.Timeout = time.Second
	c.ConfigureEgress(m)
	defer c.HTTP.CloseIdleConnections()
	a := &auth.Auth{UID: "a", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	rc, status, _, err := c.ChatStreamContext(context.Background(), a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("proxied chat failed: %d %v", status, err)
	}
	io.Copy(io.Discard, rc)
	rc.Close()
	if err := c.RefreshToken(a); err != nil || a.AccessTokenValue() != "new-at" {
		t.Fatalf("proxied refresh failed: %v", err)
	}
	if _, err := c.billingJSON(a, "GET", "/billing/test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FetchModels(a); err != nil {
		t.Fatal(err)
	}
	if name, err := c.FetchAccountProfile(a); err != nil || name != "Proxy account" {
		t.Fatalf("proxied profile failed: %s %v", name, err)
	}
	req, _ := c.globalRegisterReq("POST", "http://workbuddy-test.invalid/register-test", a.AccessTokenValue(), nil)
	if code, _, _, err := c.globalRegisterJSON(req, a); err != nil || code != 0 {
		t.Fatal("registration path bypassed fixed proxy", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, suffix := range []string{"/v2/chat/completions", "/v2/plugin/auth/token/refresh", "/billing/test", accountProfilePath, "/register-test"} {
		if paths[suffix] < 1 {
			t.Fatal("missing account-routed path " + suffix)
		}
	}
}

func TestProxiedChatProtocolFallbackIsIsolatedByExit(t *testing.T) {
	c := New()
	m, _ := egress.New("")
	c.ConfigureEgress(m)
	u, _ := http.NewRequest("GET", "https://workbuddy-test.invalid/chat", nil)
	c.noteChatHTTP2Failure(u.URL, "ip:8.8.8.8")
	if c.chatHTTPFor(u.URL, "ip:1.1.1.1") != c.ChatHTTP {
		t.Fatal("HTTP/1 fallback leaked to another IP")
	}
	h1 := c.chatHTTPFor(u.URL, "ip:8.8.8.8")
	if h1 == c.ChatHTTP {
		t.Fatal("HTTP/2 fallback did not activate for failing exit")
	}
	if _, ok := h1.Transport.(*egress.Transport); !ok {
		t.Fatal("HTTP/1 fallback lost account proxy routing")
	}
	h1.CloseIdleConnections()
	c.HTTP.CloseIdleConnections()
}
