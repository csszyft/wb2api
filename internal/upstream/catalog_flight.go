package upstream

import (
	"context"
	"sync"
)

// catalogFlight coalesces lookups per realm without sharing an account's
// egress route or credentials with a different account. The worker has its
// own context so one canceled browser request cannot abort every waiter.
type catalogFlight struct {
	done      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	fetch     func(context.Context)
	waiters   int
	canceled  bool
	completed bool
}

type catalogFlightGroup struct {
	mu      sync.Mutex
	flights map[string]*catalogFlight
}

func (g *catalogFlightGroup) do(ctx context.Context, key string, fetch func(context.Context)) bool {
	if ctx.Err() != nil {
		return false
	}
	g.mu.Lock()
	if g.flights == nil {
		g.flights = make(map[string]*catalogFlight)
	}
	f := g.flights[key]
	if f == nil {
		workerCtx, cancel := context.WithCancel(context.Background())
		f = &catalogFlight{done: make(chan struct{}), ctx: workerCtx, cancel: cancel, fetch: fetch}
		g.flights[key] = f
		go g.run(key, f)
	}
	f.waiters++
	g.mu.Unlock()

	select {
	case <-f.done:
		g.mu.Lock()
		completed := !f.canceled
		g.mu.Unlock()
		return completed
	case <-ctx.Done():
		last := false
		g.mu.Lock()
		if !f.completed {
			f.waiters--
			if f.waiters == 0 {
				last = true
				f.canceled = true
				f.cancel()
				if g.flights[key] == f {
					delete(g.flights, key)
				}
			}
		}
		g.mu.Unlock()
		if last {
			<-f.done // A sole caller receives the same partial/fail-soft result.
			return true
		}
		return false
	}
}

func (g *catalogFlightGroup) run(key string, f *catalogFlight) {
	f.fetch(f.ctx)
	g.mu.Lock()
	f.cancel()
	if g.flights[key] == f {
		delete(g.flights, key)
	}
	f.completed = true
	close(f.done)
	g.mu.Unlock()
}

// cloneModelInfos isolates cached slices from callers that merge overlays or
// fill metadata in place.
func cloneModelInfos(in []ModelInfo) []ModelInfo {
	if in == nil {
		return nil
	}
	out := make([]ModelInfo, len(in))
	copy(out, in)
	for i := range out {
		out[i].Efforts = append([]string(nil), in[i].Efforts...)
		out[i].Tags = append([]string(nil), in[i].Tags...)
		if in[i].PromoFactor != nil {
			factor := *in[i].PromoFactor
			out[i].PromoFactor = &factor
		}
	}
	return out
}

func cloneModelDiag(in ModelFetchDiag) ModelFetchDiag {
	out := in
	out.CliAgentIDs = append([]string(nil), in.CliAgentIDs...)
	out.RawModelIDs = append([]string(nil), in.RawModelIDs...)
	out.AllModelIDs = append([]string(nil), in.AllModelIDs...)
	out.Dropped = append([]string(nil), in.Dropped...)
	out.V3OnlyIDs = append([]string(nil), in.V3OnlyIDs...)
	out.Agents = make([]ModelFetchAgent, len(in.Agents))
	copy(out.Agents, in.Agents)
	for i := range out.Agents {
		out.Agents[i].Models = append([]string(nil), in.Agents[i].Models...)
	}
	return out
}
