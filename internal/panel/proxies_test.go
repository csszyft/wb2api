package panel

import (
	"encoding/json"
	"io"
	"net"
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
)

func proxyPanelFixture(t *testing.T) (*Panel, *egress.Manager) {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "a", Domain: "www.workbuddy.ai", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "b", AccessToken: "at2", ExpiresAt: 9999999999})
	path := filepath.Join(t.TempDir(), "proxy-pool.json")
	raw, _ := json.Marshal(map[string]any{"version": 1, "settings": egress.Settings{Enabled: true, DefaultCapacity: 1}, "proxies": []map[string]any{{"id": "px-a", "name": "us.accountA", "url": "http://user:password@127.0.0.1:1", "enabled": true, "capacity": 1, "exit_ip": "8.8.8.8", "checked_at": time.Now()}}, "bindings": map[string]string{}})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := egress.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return New(Config{Pool: p, Egress: m, APIKey: "test-key"}), m
}

func proxyPanelRequest(p *Panel, method, path, body string, key bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/panel/api/"+path, strings.NewReader(body))
	if key {
		r.Header.Set("Authorization", "Bearer test-key")
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w
}

func TestProxyPanelAuthAssignmentBindingAndPersistence(t *testing.T) {
	p, m := proxyPanelFixture(t)
	if r := proxyPanelRequest(p, "GET", "proxies", "", false); r.Code != 401 {
		t.Fatal("proxy pool exposed without API key")
	}
	r := proxyPanelRequest(p, "GET", "proxies", "", true)
	if r.Code != 200 || strings.Contains(r.Body.String(), "password") || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("proxy view leaks credentials or is unreadable: %d %s", r.Code, r.Body)
	}
	r = proxyPanelRequest(p, "POST", "proxies/assign", `{"realm":"global"}`, true)
	if r.Code != 200 || m.Route("a").ProxyID != "px-a" || m.Route("b").ProxyID != "" {
		t.Fatalf("region assignment failed: %d %s", r.Code, r.Body)
	}
	if r := proxyPanelRequest(p, "POST", "accounts/b/proxy", `{"proxy_id":"px-a"}`, true); r.Code != 400 {
		t.Fatal("panel bypassed IP capacity")
	}
	if r := proxyPanelRequest(p, "POST", "accounts/a/proxy", `{"proxy_id":"direct"}`, true); r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	if r := proxyPanelRequest(p, "POST", "accounts/b/proxy", `{"proxy_id":"px-a"}`, true); r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	if r := proxyPanelRequest(p, "POST", "proxies/px-a/remove", `{}`, true); r.Code != 400 {
		t.Fatal("deleted an active bound proxy")
	}
	r = proxyPanelRequest(p, "GET", "overview", "", true)
	if !strings.Contains(r.Body.String(), "account_egress") || !strings.Contains(r.Body.String(), "8.8.8.8") || strings.Contains(r.Body.String(), "password") {
		t.Fatal("account overview missing fixed exit information")
	}
}

func TestProxyPanelImportReturnsBeforeNetworkAndReportsInvalidEntries(t *testing.T) {
	p, m := proxyPanelFixture(t)
	body := `{"text":"socks5h://custom-group.a:secret@127.0.0.1:1\nwrong-line\n127.0.0.1:2:user:password","protocol":"http","capacity":2}`
	start := time.Now()
	r := proxyPanelRequest(p, "POST", "proxies/import", body, true)
	if r.Code != 202 || time.Since(start) > time.Second || !strings.Contains(r.Body.String(), `"imported":2`) || !strings.Contains(r.Body.String(), "第 2 项") {
		t.Fatalf("import is blocking or lost valid lines: %d %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "secret") || strings.Contains(r.Body.String(), "password") {
		t.Fatal("import receipt exposed proxy credentials")
	}
	m.Close()
}

func TestProxyBodyRejectsOversizeOrTrailingInput(t *testing.T) {
	p, _ := proxyPanelFixture(t)
	for _, body := range []string{`{} {}`, `{"enabled":true,"default_capacity":1,"extra":"` + strings.Repeat("x", 2<<20) + `"}`} {
		r := proxyPanelRequest(p, "POST", "proxies/settings", body, true)
		if r.Code != 400 {
			t.Fatalf("accepted oversize/trailing data: %d", r.Code)
		}
	}
}

// Fixture page served for the isolated browser smoke test. No account tokens or
// live upstream calls are used; the test owns the loopback listener and data.
func TestProxyBrowserFixture(t *testing.T) {
	if os.Getenv("WB2API_PROXY_BROWSER_FIXTURE") != "1" {
		t.Skip("browser fixture only")
	}
	p, manager := proxyPanelFixture(t)
	p.cfg.APIKey = ""
	p.cfg.Version = "proxy-test"
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"ip":"8.8.4.4"}`) }))
	defer origin.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			io.WriteString(w, `{"ip":"8.8.4.4"}`)
			return
		}
		remote, err := net.Dial("tcp", origin.Listener.Addr().String())
		if err != nil {
			w.WriteHeader(502)
			return
		}
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			remote.Close()
			return
		}
		buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffered.Flush()
		go func() { defer remote.Close(); defer client.Close(); io.Copy(remote, buffered) }()
		io.Copy(client, remote)
	}))
	defer proxy.Close()
	probeTransport := origin.Client().Transport.(*http.Transport).Clone()
	probeTransport.TLSClientConfig = probeTransport.TLSClientConfig.Clone()
	// This test tunnel routes public check hostnames to an isolated httptest
	// certificate. Production transports retain their normal TLS verification.
	probeTransport.TLSClientConfig.InsecureSkipVerify = true
	manager.SetProbeTransport(probeTransport)
	srv := httptest.NewServer(p)
	defer srv.Close()
	fixturePath := os.Getenv("WB2API_PROXY_FIXTURE_PATH")
	if fixturePath == "" {
		t.Fatal("missing fixture path")
	}
	if err := os.WriteFile(fixturePath, []byte(srv.URL), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixturePath+".proxy", []byte(proxy.URL), 0600); err != nil {
		t.Fatal(err)
	}
	stop := fixturePath + ".stop"
	t.Log("fixture listening", srv.URL)
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stop); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
