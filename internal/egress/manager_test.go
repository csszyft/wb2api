package egress

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func verifiedProxy(t *testing.T, m *Manager, raw, ip string, capacity int) string {
	t.Helper()
	ids, _, _, err := m.Import([]string{raw}, capacity)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	next := cloneState(m.state)
	i := proxyIndex(&next, ids[0])
	next.Proxies[i].ExitIP, next.Proxies[i].CheckedAt = ip, time.Now()
	err = m.saveLocked(next)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return ids[0]
}

func TestFixedBindingsCapacityPersistenceAndReimport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy-pool.json")
	m, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	a := verifiedProxy(t, m, "socks5h://us.a:secret@proxy.example:1080", "8.8.8.8", 2)
	verifiedProxy(t, m, "socks5h://us.b:secret@proxy.example:1080", "8.8.8.8", 2)
	b := verifiedProxy(t, m, "http://us.c:secret@proxy.example:8080", "1.1.1.1", 2)
	accounts := []Account{{UID: "u1"}, {UID: "u2"}, {UID: "u3"}, {UID: "u4"}, {UID: "u5"}}
	if err := m.SyncAccounts(accounts); err != nil {
		t.Fatal(err)
	}
	result, err := m.Assign(accounts, false)
	if err != nil || result.Assigned != 4 || len(result.Unbound) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if m.state.Bindings["u1"] != a || m.state.Bindings["u2"] != a || m.state.Bindings["u3"] != b {
		t.Fatal("IP capacity did not fill in stable account order")
	}
	if err := m.UpdateSettings(Settings{Enabled: true, AutoAssign: true, DefaultCapacity: 1}); err != nil {
		t.Fatal(err)
	}
	if err := m.Bind("u5", a); err == nil {
		t.Fatal("manual binding bypassed IP capacity")
	}
	before := m.Snapshot()
	if _, _, _, err := m.Import([]string{"socks5h://us.a:secret@proxy.example:1080"}, 1); err != nil {
		t.Fatal(err)
	}
	if m.Route("u1").ProxyID != a {
		t.Fatal("reimport changed fixed binding")
	}
	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if err := reloaded.SyncAccounts(accounts); err != nil {
		t.Fatal(err)
	}
	for uid, route := range before.Routes {
		if reloaded.Route(uid).ProxyID != route.ProxyID {
			t.Fatal("restart changed binding for " + uid)
		}
	}
	if reloaded.Route("u5").Available {
		t.Fatal("unbound strict-mode account may not use original exit")
	}
	if err := m.UpdateProxy(a, "US fixed", 1, true); err == nil {
		t.Fatal("capacity decrease silently overbooked bound IP")
	}
	if err := m.RemoveProxy(a); err == nil {
		t.Fatal("bound proxy was removed")
	}
	if err := m.SyncAccounts(accounts[2:]); err != nil {
		t.Fatal(err)
	}
	if m.Route("u5").ProxyID != a {
		t.Fatal("automatic assignment did not reuse the released IP slot")
	}
	if err := m.Bind("u5", Direct); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveProxy(a); err != nil {
		t.Fatal("removed accounts failed to release proxy capacity", err)
	}
	view, _ := json.Marshal(m.Snapshot())
	if strings.Contains(string(view), "secret") {
		t.Fatal("proxy password leaked in snapshot")
	}
}

func TestProxyFailureAndIPBlocksStayScoped(t *testing.T) {
	m, _ := New("")
	a := verifiedProxy(t, m, "http://a:secret@proxy.example:8080", "8.8.8.8", 2)
	b := verifiedProxy(t, m, "http://b:secret@proxy.example:8080", "8.8.8.8", 2)
	c := verifiedProxy(t, m, "http://c:secret@proxy.example:8080", "1.1.1.1", 2)
	accounts := []Account{{UID: "a"}, {UID: "b"}, {UID: "c"}}
	m.SyncAccounts(accounts)
	m.Bind("a", a)
	m.Bind("b", b)
	m.Bind("c", c)
	m.UpdateSettings(Settings{Enabled: true, DefaultCapacity: 1})
	m.Block("proxy:"+a, time.Now().Add(time.Minute))
	if m.Available("a") || !m.Available("b") || !m.Available("c") {
		t.Fatal("single proxy connection failure affected other credentials")
	}
	m.Block("ip:8.8.8.8", time.Now().Add(time.Minute))
	if m.Available("a") || m.Available("b") || !m.Available("c") {
		t.Fatal("IP block leaked to another exit")
	}
	if err := m.Bind("c", Direct); err != nil {
		t.Fatal(err)
	}
	if !m.Route("c").Direct {
		t.Fatal("explicit original-exit binding did not work")
	}
	if err := m.Bind("unknown", c); err == nil {
		t.Fatal("binding accepted unknown account")
	}
}

func TestProxyPoolSaveFailureKeepsOldBindings(t *testing.T) {
	m, _ := New("")
	m.SyncAccounts([]Account{{UID: "a"}})
	a := verifiedProxy(t, m, "http://user:secret@proxy.example:8080", "8.8.8.8", 1)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	m.path = filepath.Join(blocker, "proxy-pool.json")
	if err := m.Bind("a", a); err == nil {
		t.Fatal("save failure not reported")
	}
	if m.state.Bindings["a"] != "" {
		t.Fatal("failed save applied a nonpersistent binding")
	}
}

func TestProxyConcurrentAssignDoesNotOverbook(t *testing.T) {
	m, _ := New("")
	verifiedProxy(t, m, "http://user:secret@proxy.example:8080", "8.8.8.8", 1)
	accounts := []Account{{UID: "a"}, {UID: "b"}, {UID: "c"}}
	m.SyncAccounts(accounts)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.Assign(accounts, false); m.Snapshot(); m.Route("a") }()
	}
	wg.Wait()
	if count := m.Snapshot().Bound; count != 1 {
		t.Fatalf("concurrent assignment exceeded capacity: %d", count)
	}
}
