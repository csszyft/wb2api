package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestModelCatalogCoalescesConcurrentRequests(t *testing.T) {
	var enterpriseCalls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	c := New()
	c.HTTP.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/enterprises/personal/models") {
			enterpriseCalls.Add(1)
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return jsonResp(200, `{"code":0,"data":{"models":[{"id":"cn-model","maxOutputTokens":8192}]}}`), nil
	})
	a := &auth.Auth{UID: "cn", Domain: "www.codebuddy.cn", AccessToken: "token"}
	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			infos, _, err := c.FetchModelsDiagContext(context.Background(), a)
			if err == nil && len(infos) != 1 {
				t.Errorf("unexpected catalog: %+v", infos)
			}
			errs <- err
		}()
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("catalog probe did not start")
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := enterpriseCalls.Load(); got != 1 {
		t.Fatalf("concurrent lookups made %d enterprise calls, want 1", got)
	}
}

func TestGlobalModelCatalogCoalescesConcurrentRequests(t *testing.T) {
	var enterpriseCalls atomic.Int64
	c := New()
	c.HTTP.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/enterprises/personal/models") {
			enterpriseCalls.Add(1)
			time.Sleep(30 * time.Millisecond)
		}
		return jsonResp(200, `{"code":0,"data":{"models":[{"id":"global-model","maxOutputTokens":8192}]}}`), nil
	})
	a := globalAuth()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if infos := c.FetchGlobalModelInfos(a); len(infos) == 0 {
				t.Error("empty global catalog")
			}
		}()
	}
	wg.Wait()
	if got := enterpriseCalls.Load(); got != 1 {
		t.Fatalf("concurrent global lookups made %d enterprise calls, want 1", got)
	}
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failingBody) Close() error             { return nil }

func TestV3ConfigBodyReadFailureIsNotIgnored(t *testing.T) {
	c := New()
	c.HTTP.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: failingBody{}}, nil
	})
	_, err := c.fetchV3ConfigModelMapContext(context.Background(), &auth.Auth{UID: "cn", Domain: "www.codebuddy.cn"}, codeBuddyIDEUA)
	if err == nil || !strings.Contains(err.Error(), "read v3/config body") {
		t.Fatalf("body read error was lost: %v", err)
	}
}
