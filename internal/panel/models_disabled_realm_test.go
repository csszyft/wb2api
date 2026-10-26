package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestPanelModelsSkipsDisabledGlobalAccounts(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	var cnCalls, globalCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/console/enterprises/personal/models":
			cnCalls.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"cn-only","maxInputTokens":65536}],"agents":[{"name":"cli","models":["cn-only"]}]}}`))
		case "/v2/enterprises/personal/models":
			globalCalls.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"global-only"}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn", Domain: "www.codebuddy.cn", AccessToken: "cn-token"})
	p.Add(&auth.Auth{UID: "global", Domain: "www.workbuddy.ai", AccessToken: "global-token"})
	p.Disable("global", "manual disable (panel)")
	up := upstream.New()
	up.ChatBaseCN, up.BillingBaseCN = srv.URL, srv.URL
	up.ChatBaseGlobal, up.BillingBaseGlobal = srv.URL, srv.URL
	pn := New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up})

	request := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", "/panel/api/models"+query, nil)
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		pn.ServeHTTP(rec, req)
		return rec
	}
	rec := request("")
	if rec.Code != http.StatusOK {
		t.Fatalf("default model lookup: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Realm    string           `json:"realm"`
		Models   []map[string]any `json:"models"`
		Servable map[string]bool  `json:"realm_servable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Realm != "cn" || len(body.Models) == 0 || body.Models[0]["id"] != "cn-only" {
		t.Fatalf("disabled global must fall back to CN: %+v", body)
	}
	if !body.Servable["cn"] || body.Servable["global"] || globalCalls.Load() != 0 || cnCalls.Load() == 0 {
		t.Fatalf("wrong realm selection: servable=%v cn=%d global=%d", body.Servable, cnCalls.Load(), globalCalls.Load())
	}
	if rec := request("?realm=global"); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "恢复账号") {
		t.Fatalf("disabled explicit realm should explain recovery: %d %s", rec.Code, rec.Body.String())
	}
}
