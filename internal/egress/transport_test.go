package egress

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHTTPProxyUsesCredentialsAndDoesNotFallBack(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Proxy-Authorization"))
		mu.Unlock()
		io.WriteString(w, "proxied")
	}))
	defer proxy.Close()
	m, _ := New("")
	u, _ := url.Parse(proxy.URL)
	u.User = url.UserPassword("us.accountA", "s3cret@:")
	id := verifiedProxy(t, m, u.String(), "8.8.8.8", 1)
	m.SyncAccounts([]Account{{UID: "a"}})
	if err := m.Bind("a", id); err != nil {
		t.Fatal(err)
	}
	m.UpdateSettings(Settings{Enabled: true, DefaultCapacity: 1})
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = func(*http.Request) (*url.URL, error) {
		return nil, errors.New("environment proxy must not override binding")
	}
	tr := NewTransport(base, m)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: time.Second}
	req, _ := http.NewRequest("GET", "http://destination.invalid/test", nil)
	resp, err := client.Do(WithAccount(req, "a"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(raw) != "proxied" {
		t.Fatal("bound request did not reach proxy")
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "Basic "+base64.StdEncoding.EncodeToString([]byte("us.accountA:s3cret@:")) {
		t.Fatalf("proxy credentials changed: %v", got)
	}
	proxy.Close()
	tr.Invalidate(id)
	_, err = client.Do(WithAccount(req, "a"))
	var proxyErr *RequestError
	if !errors.As(err, &proxyErr) || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("proxy error did not stay safe/distinct: %v", err)
	}
	if m.Available("a") {
		t.Fatal("failed connection did not create bounded proxy backoff")
	}
	if tr.HTTP1Transport() == tr {
		t.Fatal("HTTP/1 fallback lost routed transport")
	}
}

func TestHTTPSConnectProxyPreservesTLSAndAuthentication(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "tls origin") }))
	defer origin.Close()
	var mu sync.Mutex
	connects := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("acct:password")) {
			w.WriteHeader(407)
			return
		}
		conn, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		client, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			conn.Close()
			return
		}
		mu.Lock()
		connects++
		mu.Unlock()
		rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		rw.Flush()
		go func() { defer conn.Close(); defer client.Close(); io.Copy(conn, rw) }()
		io.Copy(client, conn)
	}))
	defer proxy.Close()
	m, _ := New("")
	u, _ := url.Parse(proxy.URL)
	u.User = url.UserPassword("acct", "password")
	id := verifiedProxy(t, m, u.String(), "8.8.8.8", 1)
	m.SyncAccounts([]Account{{UID: "a"}})
	m.Bind("a", id)
	m.UpdateSettings(Settings{Enabled: true, DefaultCapacity: 1})
	tr := NewTransport(origin.Client().Transport, m)
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest("GET", origin.URL, nil)
	resp, err := (&http.Client{Transport: tr, Timeout: time.Second}).Do(WithAccount(req, "a"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	mu.Lock()
	n := connects
	mu.Unlock()
	if string(raw) != "tls origin" || n != 1 {
		t.Fatalf("CONNECT tunnel failed: body=%s connects=%d", raw, n)
	}
}

func TestSOCKS5HRemoteDNSAndProxyAccountAuthentication(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	result := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- err.Error()
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		fail := func(err error) { result <- err.Error() }
		head := make([]byte, 2)
		if _, err := io.ReadFull(conn, head); err != nil {
			fail(err)
			return
		}
		methods := make([]byte, int(head[1]))
		if _, err := io.ReadFull(conn, methods); err != nil {
			fail(err)
			return
		}
		conn.Write([]byte{5, 2})
		if _, err := io.ReadFull(conn, head); err != nil {
			fail(err)
			return
		}
		user := make([]byte, int(head[1]))
		io.ReadFull(conn, user)
		length := make([]byte, 1)
		io.ReadFull(conn, length)
		password := make([]byte, int(length[0]))
		io.ReadFull(conn, password)
		conn.Write([]byte{1, 0})
		request := make([]byte, 4)
		if _, err := io.ReadFull(conn, request); err != nil {
			fail(err)
			return
		}
		if request[3] != 3 {
			result <- fmt.Sprintf("destination was resolved locally, atyp=%d", request[3])
			return
		}
		io.ReadFull(conn, length)
		host := make([]byte, int(length[0]))
		io.ReadFull(conn, host)
		port := make([]byte, 2)
		io.ReadFull(conn, port)
		conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
		r, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			fail(err)
			return
		}
		r.Body.Close()
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 7\r\nConnection: close\r\n\r\nSOCKSOK")
		result <- string(user) + "|" + string(password) + "|" + string(host)
	}()
	m, _ := New("")
	id := verifiedProxy(t, m, "socks5h://custom-group.account-B:p%40ss@"+listener.Addr().String(), "8.8.8.8", 1)
	m.SyncAccounts([]Account{{UID: "a"}})
	m.Bind("a", id)
	m.UpdateSettings(Settings{Enabled: true, DefaultCapacity: 1})
	tr := NewTransport(http.DefaultTransport.(*http.Transport).Clone(), m)
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest("GET", "http://remote-only.invalid/test", nil)
	resp, err := (&http.Client{Transport: tr, Timeout: 3 * time.Second}).Do(WithAccount(req, "a"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "SOCKSOK" {
		t.Fatal("SOCKS response missing")
	}
	select {
	case value := <-result:
		if value != "custom-group.account-B|p@ss|remote-only.invalid" {
			t.Fatalf("SOCKS auth/DNS mismatch: %s", value)
		}
	case <-time.After(time.Second):
		t.Fatal("SOCKS test did not finish")
	}
}

func TestProxyAuthAndClientCancelDoNotPunishOtherRoutes(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(407) }))
	defer proxy.Close()
	m, _ := New("")
	id := verifiedProxy(t, m, proxy.URL, "8.8.8.8", 1)
	m.SyncAccounts([]Account{{UID: "a"}})
	m.Bind("a", id)
	m.UpdateSettings(Settings{Enabled: true, DefaultCapacity: 1})
	tr := NewTransport(http.DefaultTransport.(*http.Transport).Clone(), m)
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest("GET", "http://fake.invalid", nil)
	_, err := (&http.Client{Transport: tr}).Do(WithAccount(req, "a"))
	var pe *RequestError
	if !errors.As(err, &pe) || pe.Code != "proxy_auth" {
		t.Fatalf("407 was confused with account/upstream error: %v", err)
	}
	m.mu.Lock()
	m.blocks = map[string]time.Time{}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = (&http.Client{Transport: tr}).Do(WithAccount(req.WithContext(ctx), "a"))
	if err == nil || !m.Available("a") {
		t.Fatal("client cancel incorrectly cooled proxy")
	}
}
