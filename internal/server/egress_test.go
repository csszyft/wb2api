package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func serverProxyPool(t *testing.T, urls, ips []string) (*pool.Pool, *egress.Manager, *upstream.Client) {
	t.Helper()
	proxies := []map[string]any{}
	bindings := map[string]string{}
	p := pool.New("")
	p.SetPickMode(pool.PickSequential)
	for i, raw := range urls {
		uid := string(rune('a' + i))
		id := "proxy-" + uid
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
		p.SetCredits(uid, 1000, 1000)
		bindings[uid] = id
		proxies = append(proxies, map[string]any{"id": id, "name": id, "url": raw, "enabled": true, "capacity": 10, "exit_ip": ips[i], "checked_at": time.Now()})
	}
	state := map[string]any{"version": 1, "settings": egress.Settings{Enabled: true, DefaultCapacity: 1}, "proxies": proxies, "bindings": bindings}
	raw, _ := json.Marshal(state)
	path := filepath.Join(t.TempDir(), "proxy-pool.json")
	os.WriteFile(path, raw, 0600)
	m, err := egress.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	accounts := []egress.Account{}
	for _, a := range p.List() {
		accounts = append(accounts, egress.Account{UID: a.UID})
	}
	m.SyncAccounts(accounts)
	p.SetOutboundAvailable(m.Available)
	c := upstream.New()
	c.ChatBaseCN = "http://upstream.invalid"
	c.HTTP.Timeout = time.Second
	c.HeaderTimeout = time.Second
	c.ConfigureEgress(m)
	t.Cleanup(func() { c.HTTP.CloseIdleConnections() })
	return p, m, c
}

func TestProxyTransportFailureRotatesWithoutAccountPenalties(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(407) }))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseOK)
	}))
	defer good.Close()
	p, m, c := serverProxyPool(t, []string{bad.URL, good.URL}, []string{"8.8.8.8", "1.1.1.1"})
	h := NewHandler(Config{Pool: p, Upstream: c, MaxRotate: 2})
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if r.Code != 200 {
		t.Fatalf("proxy failure stopped healthy exit: %d %s", r.Code, r.Body)
	}
	st, _ := p.Status("a")
	if st.ErrTotal != 0 || st.ConsecutiveFails != 0 || st.Cooling || st.Disabled || st.InFlight != 0 {
		t.Fatalf("proxy authentication punished WorkBuddy account: %+v", st)
	}
	if m.Available("a") || !m.Available("b") {
		t.Fatal("proxy cooldown did not stay isolated")
	}
}

func TestWAFGateGroupsActualIPAndLeavesOtherExitUsable(t *testing.T) {
	var hit []string
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = append(hit, r.Header.Get("Authorization"))
		w.WriteHeader(403)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = append(hit, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseOK)
	}))
	defer good.Close()
	p, m, c := serverProxyPool(t, []string{bad.URL, bad.URL, good.URL}, []string{"8.8.8.8", "8.8.8.8", "1.1.1.1"})
	h := NewHandler(Config{Pool: p, Upstream: c, MaxRotate: 3})
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if r.Code != 200 || len(hit) != 3 {
		t.Fatalf("one WAF exit stopped another: %d %s calls=%v", r.Code, r.Body, hit)
	}
	if m.Available("a") || m.Available("b") || !m.Available("c") || h.wafIP.active() {
		t.Fatal("WAF block was not scoped to actual exit IP")
	}
	if !h.wafIP.forRoute("ip:8.8.8.8").active() || h.wafIP.forRoute("ip:1.1.1.1").active() {
		t.Fatal("separate IP gate accounting failed")
	}
}

func TestUnboundProxyTerminalCannotBeRewrittenAsQuotaError(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "a", AccessToken: "at", ExpiresAt: 9999999999})
	m, _ := egress.New("")
	m.SyncAccounts([]egress.Account{{UID: "a"}})
	m.UpdateSettings(egress.Settings{Enabled: true, DefaultCapacity: 1})
	p.SetOutboundAvailable(m.Available)
	p.CooldownSoftForModel("a", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "6004 model rate limit")
	c := upstream.New()
	c.Egress = m
	h := NewHandler(Config{Pool: p, Upstream: c})
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if r.Code != 503 || !strings.Contains(r.Body.String(), `"code":"proxy_unavailable"`) || r.Header().Get("Retry-After") != "" {
		t.Fatalf("proxy unavailable became quota/missing model: %d %s", r.Code, r.Body)
	}
}
