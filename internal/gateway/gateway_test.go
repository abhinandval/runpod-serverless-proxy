package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abhinandval/runpod-serverless-proxy/internal/agent"
	"github.com/abhinandval/runpod-serverless-proxy/internal/config"
)

const testConfig = `
gateway:
  api_keys: ["gw-key"]
fleet:
  inference_key: "runpod-key"
units:
  u1:
    engine: llama.cpp
    url: https://unit.test
    timeouts: {cold_start: 2s, request: 5s}
models:
  qwen:
    unit: u1
    aliases: [gpt-4o-mini]
`

// fakeUnit plays a RunPod load balancer with one agent behind it. Each status
// poll returns the next scripted phase; the last one repeats.
type fakeUnit struct {
	mu       sync.Mutex
	phases   []agent.Phase
	polls    int
	requests []map[string]any // decoded bodies of forwarded requests
	auth     []string
	goCold   int // the next N forwarded requests are answered "not ready"
}

func (f *fakeUnit) phase() agent.Phase {
	if len(f.phases) == 0 {
		return agent.PhaseReady
	}
	return f.phases[0]
}

func (f *fakeUnit) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(code int, ctype, body string) (*http.Response, error) {
		return &http.Response{StatusCode: code, Status: http.StatusText(code), Header: http.Header{"Content-Type": {ctype}, "X-Runpod-Worker-Id": {"w1"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
	if r.URL.Path == "/agent/status" {
		f.polls++
		p := f.phase()
		if len(f.phases) > 1 {
			f.phases = f.phases[1:]
		}
		st := agent.Status{Phase: p, Engine: "llama.cpp", Model: "engine-model"}
		if p == agent.PhaseFailed {
			st.Error = "llama.cpp exited while loading"
		}
		raw, _ := json.Marshal(st)
		return reply(200, "application/json", string(raw))
	}
	if f.goCold > 0 || f.phase() != agent.PhaseReady {
		if f.goCold > 0 {
			f.goCold--
		}
		return reply(503, "application/json", `{"error":{"type":"unit_not_ready"}}`)
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.requests = append(f.requests, body)
	f.auth = append(f.auth, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Api-Key"))
	if body["stream"] == true {
		if strings.HasPrefix(r.URL.Path, "/v1/messages") {
			return reply(200, "text/event-stream", "event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n")
		}
		return reply(200, "text/event-stream", "data: hello\n\ndata: [DONE]\n\n")
	}
	return reply(200, "application/json", `{"ok":true}`)
}

func newTestGateway(t *testing.T, f *fakeUnit) *Gateway {
	t.Helper()
	cfg, err := config.Parse([]byte(testConfig), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	g.PollEvery, g.KeepAliveEvery, g.ReadyTTL = 15*time.Millisecond, 4*time.Millisecond, time.Minute
	return g
}

func do(g *Gateway, method, path, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, r)
	return rec
}

func TestAuthModelsAndUnknownModel(t *testing.T) {
	g := newTestGateway(t, &fakeUnit{})

	if rec := do(g, "POST", "/v1/chat/completions", "wrong", `{"model":"qwen"}`); rec.Code != 401 || !strings.Contains(rec.Body.String(), `"error":{`) {
		t.Errorf("bad key = %d %s", rec.Code, rec.Body)
	}
	// Anthropic clients send the key in x-api-key and expect Anthropic's error shape.
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"nope"}`))
	r.Header.Set("x-api-key", "gw-key")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, r)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"type":"error"`) || rec.Header().Get("X-Request-Id") == "" {
		t.Errorf("unknown model = %d %s", rec.Code, rec.Body)
	}

	rec = do(g, "GET", "/v1/models", "gw-key", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"qwen"`) || strings.Contains(rec.Body.String(), "gpt-4o-mini") {
		t.Errorf("models = %d %s", rec.Code, rec.Body)
	}
	if rec := do(g, "GET", "/healthz", "", ""); rec.Code != 200 {
		t.Errorf("healthz = %d", rec.Code)
	}
}

func TestColdStreamingRequestIsKeptAliveThenServed(t *testing.T) {
	f := &fakeUnit{phases: []agent.Phase{agent.PhaseDownloading, agent.PhaseLoading, agent.PhaseReady}}
	g := newTestGateway(t, f)

	rec := do(g, "POST", "/v1/chat/completions", "gw-key", `{"model":"gpt-4o-mini","stream":true,"messages":[]}`)
	out := rec.Body.String()
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d content-type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	ka, data := strings.Index(out, ": keep-alive"), strings.Index(out, "data: hello")
	if ka < 0 || data < 0 || ka > data {
		t.Errorf("want keep-alives before the engine's data, got %q", out)
	}
	// The alias is rewritten to the engine's own model name, and the unit is
	// called with the fleet key, never the client's.
	if len(f.requests) != 1 || f.requests[0]["model"] != "engine-model" || f.auth[0] != "Bearer runpod-key|" {
		t.Errorf("forwarded %v auth %v", f.requests, f.auth)
	}

	// Warm now: the next request goes straight through with no status poll.
	polls := f.polls
	rec = do(g, "POST", "/v1/chat/completions", "gw-key", `{"model":"qwen","stream":true}`)
	if strings.Contains(rec.Body.String(), "keep-alive") || f.polls != polls || !strings.Contains(rec.Body.String(), "data: hello") {
		t.Errorf("warm request: polls %d -> %d, body %q", polls, f.polls, rec.Body)
	}
}

func TestColdAnthropicStreamGetsPingEvents(t *testing.T) {
	f := &fakeUnit{phases: []agent.Phase{agent.PhaseLoading, agent.PhaseLoading, agent.PhaseReady}}
	g := newTestGateway(t, f)
	out := do(g, "POST", "/v1/messages", "gw-key", `{"model":"qwen","stream":true}`).Body.String()
	ping, start := strings.Index(out, "event: ping"), strings.Index(out, "event: message_start")
	if ping < 0 || start < 0 || ping > start || strings.Contains(out, ": keep-alive") {
		t.Errorf("want ping events before message_start, got %q", out)
	}
}

func TestColdNonStreamingRequestWaitsSilently(t *testing.T) {
	f := &fakeUnit{phases: []agent.Phase{agent.PhaseDownloading, agent.PhaseReady}}
	g := newTestGateway(t, f)
	rec := do(g, "POST", "/v1/chat/completions", "gw-key", `{"model":"qwen"}`)
	if rec.Code != 200 || rec.Body.String() != `{"ok":true}` || rec.Header().Get("X-Runpod-Worker-Id") != "w1" {
		t.Errorf("got %d %q headers %v", rec.Code, rec.Body, rec.Header())
	}
}

func TestFailedUnit(t *testing.T) {
	g := newTestGateway(t, &fakeUnit{phases: []agent.Phase{agent.PhaseFailed}})
	rec := do(g, "POST", "/v1/chat/completions", "gw-key", `{"model":"qwen"}`)
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "exited while loading") {
		t.Errorf("non-streaming: %d %s", rec.Code, rec.Body)
	}
	// A streaming client already has its 200, so the error arrives in the stream.
	out := do(g, "POST", "/v1/messages", "gw-key", `{"model":"qwen","stream":true}`)
	if out.Code != 200 || !strings.Contains(out.Body.String(), "event: error") || !strings.Contains(out.Body.String(), "exited while loading") {
		t.Errorf("streaming: %d %s", out.Code, out.Body)
	}
}

func TestColdStartTimeout(t *testing.T) {
	g := newTestGateway(t, &fakeUnit{phases: []agent.Phase{agent.PhaseDownloading}})
	g.models["qwen"].coldStart = 60 * time.Millisecond
	rec := do(g, "POST", "/v1/chat/completions", "gw-key", `{"model":"qwen"}`)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "not ready within") {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
}

func TestConcurrentRequestsShareOneWakeUp(t *testing.T) {
	f := &fakeUnit{phases: []agent.Phase{agent.PhaseLoading, agent.PhaseLoading, agent.PhaseReady}}
	g := newTestGateway(t, f)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := do(g, "POST", "/v1/chat/completions", "gw-key", `{"model":"qwen"}`); rec.Code != 200 {
				t.Errorf("got %d %s", rec.Code, rec.Body)
			}
		}()
	}
	wg.Wait()
	if n := g.models["qwen"].wakeCount; n != 1 {
		t.Errorf("wake-ups = %d, want 1", n)
	}
	if len(f.requests) != 6 {
		t.Errorf("forwarded %d requests, want 6", len(f.requests))
	}
}

func TestUnitThatWentColdIsWokenAgain(t *testing.T) {
	f := &fakeUnit{}
	g := newTestGateway(t, f)
	do(g, "POST", "/v1/chat/completions", "gw-key", `{"model":"qwen"}`) // first wake
	f.mu.Lock()
	f.goCold = 1 // the worker scaled down behind the gateway's back
	f.mu.Unlock()
	rec := do(g, "POST", "/v1/chat/completions", "gw-key", `{"model":"qwen"}`)
	if rec.Code != 200 || rec.Body.String() != `{"ok":true}` {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
	if n := g.models["qwen"].wakeCount; n != 2 {
		t.Errorf("wake-ups = %d, want 2", n)
	}
}

func TestClientDisconnectStopsWaiting(t *testing.T) {
	g := newTestGateway(t, &fakeUnit{phases: []agent.Phase{agent.PhaseDownloading}})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"qwen"}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer gw-key")
	done := make(chan struct{})
	go func() { g.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler kept waiting after the client went away")
	}
}
