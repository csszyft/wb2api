package egress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExitCheckActuallyUsesProxyAndMergeCapacity(t *testing.T) {
	var mu sync.Mutex
	ip := "8.8.8.8"
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.String(), "http://checker.invalid/") {
			t.Errorf("proxy request not absolute: %s", r.URL)
		}
		mu.Lock()
		value := ip
		mu.Unlock()
		io.WriteString(w, `{"ip":"`+value+`"}`)
	}))
	defer proxy.Close()
	m, _ := New("")
	m.probeURLs = []string{"http://checker.invalid/ip"}
	u, _ := url.Parse(proxy.URL)
	u.User = url.UserPassword("group.account-A", "secret")
	ids, _, _, _ := m.Import([]string{u.String()}, 3)
	if err := m.Check(context.Background(), ids[0]); err != nil {
		t.Fatal(err)
	}
	if p := m.Snapshot().Proxies[0]; p.ExitIP != "8.8.8.8" || p.Capacity != 3 || p.Status != "ready" {
		t.Fatalf("IP check lost capacity or address: %+v", p)
	}
	u.User = url.UserPassword("group.account-B", "secret")
	ids2, _, _, _ := m.Import([]string{u.String()}, 1)
	if err := m.Check(context.Background(), ids2[0]); err != nil {
		t.Fatal(err)
	}
	if p := m.Snapshot().Proxies[1]; p.Capacity != 3 {
		t.Fatal("same actual IP did not inherit its established capacity")
	}
	mu.Lock()
	ip = "1.1.1.1"
	mu.Unlock()
	if err := m.Check(context.Background(), ids2[0]); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().UniqueIPs != 2 {
		t.Fatal("changed proxy IP did not update grouping")
	}
}

func TestExitCheckRejectsFakeIPAndKeepsLastAddressOnFailure(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "<html>8.8.8.8</html>", `{"ip":"invalid"}`, strings.Repeat("x", 9000)} {
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, raw) }))
		m, _ := New("")
		m.probeURLs = []string{"http://checker.invalid/ip"}
		id := verifiedProxy(t, m, proxy.URL, "8.8.8.8", 1)
		if err := m.Check(context.Background(), id); err == nil {
			t.Fatalf("invalid exit response accepted: %.40s", raw)
		}
		p := m.Snapshot().Proxies[0]
		if p.ExitIP != "8.8.8.8" || p.Status != "failed" {
			t.Fatal("failure erased prior address or left proxy assignable")
		}
		proxy.Close()
	}
}

func TestBatchCheckReturnsImmediatelyAndAutoAssignsAfterDetection(t *testing.T) {
	gate := make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-gate; io.WriteString(w, `{"ip":"8.8.8.8"}`) }))
	defer proxy.Close()
	m, _ := New("")
	defer m.Close()
	m.probeURLs = []string{"http://checker.invalid/ip"}
	ids, _, _, _ := m.Import([]string{proxy.URL}, 2)
	m.SyncAccounts([]Account{{UID: "a"}, {UID: "b"}, {UID: "c"}})
	m.UpdateSettings(Settings{Enabled: true, AutoAssign: true, DefaultCapacity: 1})
	start := time.Now()
	job, err := m.StartCheck(ids, nil, false)
	if err != nil || job.Status != "running" || time.Since(start) > time.Second {
		t.Fatalf("start blocked: %+v %v", job, err)
	}
	if _, err := m.StartCheck(ids, nil, false); err == nil {
		t.Fatal("duplicate batch accepted during active job")
	}
	close(gate)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if j := m.JobSnapshot(); j.Status != "running" {
			if j.OK != 1 || j.Done != 1 || j.Assignment == nil || j.Assignment.Assigned != 2 || len(j.Assignment.Unbound) != 1 {
				t.Fatalf("batch/assignment mismatch: %+v", j)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("batch did not finish")
}
