package upstream

import (
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
)

// ConfigureEgress is called after the startup transport/timeouts are configured.
// Both clients share the same routed connection pools; all authenticated RPCs
// carry the account UID in a local context value, never in a new upstream header.
func (c *Client) ConfigureEgress(manager *egress.Manager) {
	c.Egress = manager
	if manager == nil || c.HTTP == nil {
		return
	}
	if base, ok := c.HTTP.Transport.(*http.Transport); ok {
		manager.SetProbeTransport(base)
	}
	transport := egress.NewTransport(c.HTTP.Transport, manager)
	c.HTTP.Transport = transport
	if c.ChatHTTP != nil {
		c.ChatHTTP.Transport = transport
	}
	manager.SetRouteChangeHook(func(id string) {
		transport.Invalidate(id)
		c.chatFallback.mu.Lock()
		h1 := c.chatFallback.h1
		c.chatFallback.mu.Unlock()
		if h1 != nil {
			if routed, ok := h1.Transport.(*egress.Transport); ok {
				routed.Invalidate(id)
			}
		}
	})
}

func (c *Client) doAccount(req *http.Request, a *auth.Auth) (*http.Response, error) {
	if a != nil {
		req = egress.WithAccount(req, a.UID)
	}
	return c.HTTP.Do(req)
}
