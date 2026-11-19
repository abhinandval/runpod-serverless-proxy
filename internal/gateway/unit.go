package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/abhinandval/runpod-serverless-proxy/internal/agent"
)

// unit is the gateway's live view of one serverless unit. The request path
// reads and writes only this in-memory state.
type unit struct {
	name      string
	engine    string
	baseURL   string
	coldStart time.Duration
	request   time.Duration

	mu        sync.Mutex
	status    agent.Status // last status the agent reported
	readyAt   time.Time    // when the unit was last known ready; zero if not
	waking    *wake        // in-flight wake-up shared by every waiting request
	wakeCount int          // how many cold starts this gateway has driven
}

// wake is one shared attempt to bring a unit to ready.
type wake struct {
	done chan struct{}
	err  error
}

// markNotReady forgets that the unit was ready, so the next request wakes it.
func (u *unit) markNotReady() {
	u.mu.Lock()
	u.readyAt = time.Time{}
	u.mu.Unlock()
}

// engineModel is the model name the engine itself answers to, once known.
func (u *unit) engineModel() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status.Model
}

// knownReady reports whether the unit was seen ready recently enough to send a
// request without checking first.
func (g *Gateway) knownReady(u *unit) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return !u.readyAt.IsZero() && g.now().Sub(u.readyAt) < g.ReadyTTL
}

// ensureReady blocks until the unit is ready, the wake-up fails, or ctx ends.
// Concurrent callers share one wake-up: the first poll through the load
// balancer is what makes RunPod start a worker.
func (g *Gateway) ensureReady(ctx context.Context, u *unit) error {
	if g.knownReady(u) {
		return nil
	}
	u.mu.Lock()
	w := u.waking
	if w == nil {
		w = &wake{done: make(chan struct{})}
		u.waking = w
		u.wakeCount++
		// The wake-up outlives the request that started it: other requests may be
		// waiting on it, and a unit that is warming should finish warming.
		go g.runWake(u, w)
	}
	u.mu.Unlock()

	select {
	case <-w.done:
		return w.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *Gateway) runWake(u *unit, w *wake) {
	ctx, cancel := context.WithTimeout(context.Background(), u.coldStart)
	defer cancel()
	w.err = g.pollUntilReady(ctx, u)
	u.mu.Lock()
	if w.err == nil {
		u.readyAt = g.now()
	}
	u.waking = nil
	u.mu.Unlock()
	close(w.done)
}

// pollUntilReady polls the agent's status until it reports ready or failed.
// While no worker is up RunPod holds the call (up to about 300 s) and then
// answers 400 "timed out waiting for worker"; both are just "not yet".
func (g *Gateway) pollUntilReady(ctx context.Context, u *unit) error {
	for {
		st, err := g.agentStatus(ctx, u)
		if err == nil {
			u.mu.Lock()
			u.status = st
			u.mu.Unlock()
			switch st.Phase {
			case agent.PhaseReady:
				return nil
			case agent.PhaseFailed:
				return fmt.Errorf("unit %s failed to start: %s", u.name, st.Error)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("unit %s was not ready within %s", u.name, u.coldStart)
		case <-time.After(g.PollEvery):
		}
	}
}

// agentStatus fetches GET /agent/status through the unit's load balancer.
func (g *Gateway) agentStatus(ctx context.Context, u *unit) (agent.Status, error) {
	var st agent.Status
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.baseURL+"/agent/status", nil)
	if err != nil {
		return st, err
	}
	req.Header.Set("Authorization", "Bearer "+g.inferenceKey)
	resp, err := g.client.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return st, err
	}
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("agent status: http %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("agent status: %w", err)
	}
	return st, nil
}
