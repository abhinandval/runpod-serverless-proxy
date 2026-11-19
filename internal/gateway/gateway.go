// Package gateway is the client-facing server. It accepts OpenAI- and
// Anthropic-style requests, picks the unit that serves the requested model,
// brings that unit up if it is cold while keeping the client connection alive,
// and relays the engine's response unchanged.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/abhinandval/runpod-serverless-proxy/internal/config"
)

const maxBody = 64 << 20 // largest request body accepted

// Gateway serves the client API. Create it with New.
type Gateway struct {
	// Tunables; the defaults suit production and tests shorten them.
	ReadyTTL       time.Duration // how long a unit seen ready is trusted without re-checking
	PollEvery      time.Duration // delay between agent status polls during a cold start
	KeepAliveEvery time.Duration // delay between keep-alives sent to a waiting streaming client

	apiKeys      []string
	inferenceKey string
	client       *http.Client
	models       map[string]*unit // model name or alias -> unit
	modelNames   []string         // primary names, for GET /v1/models
	now          func() time.Time
}

// New builds a gateway from configuration. transport reaches the units; nil
// means the default transport.
func New(cfg *config.Config, transport http.RoundTripper) (*Gateway, error) {
	g := &Gateway{
		ReadyTTL: 15 * time.Second, PollEvery: 2 * time.Second, KeepAliveEvery: 10 * time.Second,
		apiKeys: cfg.Gateway.APIKeys, inferenceKey: cfg.Fleet.InferenceKey,
		client: &http.Client{Transport: transport}, // no overall timeout: streams run long
		models: map[string]*unit{}, now: time.Now,
	}
	units := map[string]*unit{}
	for name, uc := range cfg.Units {
		if uc.BaseURL() == "" {
			return nil, fmt.Errorf("unit %s has no endpoint_id or url yet", name)
		}
		units[name] = &unit{name: name, engine: uc.Engine, baseURL: uc.BaseURL(),
			coldStart: time.Duration(uc.Timeouts.ColdStart), request: time.Duration(uc.Timeouts.Request)}
	}
	for name, m := range cfg.Models {
		g.modelNames = append(g.modelNames, name)
		for _, n := range append([]string{name}, m.Aliases...) {
			g.models[n] = units[m.Unit]
		}
	}
	sort.Strings(g.modelNames)
	return g, nil
}

// ServeHTTP routes client requests.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api := apiOf(r.URL.Path)
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}
	id := newRequestID()
	w.Header().Set("X-Request-Id", id)
	if !g.authorized(r) {
		writeError(w, api, http.StatusUnauthorized, "authentication_error", "missing or invalid API key")
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		g.listModels(w)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/"):
		g.handle(w, r, api)
	default:
		writeError(w, api, http.StatusNotFound, "not_found_error", "no such route: "+r.Method+" "+r.URL.Path)
	}
}

// authorized accepts the gateway key as a bearer token (OpenAI clients) or in
// x-api-key (Anthropic clients).
func (g *Gateway) authorized(r *http.Request) bool {
	key := r.Header.Get("x-api-key")
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		key = strings.TrimPrefix(h, "Bearer ")
	}
	ok := false
	for _, k := range g.apiKeys {
		if subtle.ConstantTimeCompare([]byte(key), []byte(k)) == 1 {
			ok = true
		}
	}
	return ok
}

func (g *Gateway) listModels(w http.ResponseWriter) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Type    string `json:"type"` // Anthropic clients read this
		OwnedBy string `json:"owned_by"`
	}
	data := make([]model, 0, len(g.modelNames))
	for _, n := range g.modelNames {
		data = append(data, model{ID: n, Object: "model", Type: "model", OwnedBy: "runpod-serverless-proxy"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handle runs one inference request through its lifecycle:
// resolve the unit, wake it if needed, forward, relay.
func (g *Gateway) handle(w http.ResponseWriter, r *http.Request, api apiKind) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, api, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body is too large or unreadable")
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		writeError(w, api, http.StatusBadRequest, "invalid_request_error", "request body is not a JSON object")
		return
	}
	var modelName string
	json.Unmarshal(fields["model"], &modelName)
	u := g.models[modelName]
	if u == nil {
		writeError(w, api, http.StatusNotFound, "not_found_error", fmt.Sprintf("unknown model %q", modelName))
		return
	}
	var stream bool
	json.Unmarshal(fields["stream"], &stream)

	if api == apiAnthropic && u.engine == "llama.cpp" {
		demoteSystemMessages(fields)
	}

	ctx := r.Context()
	out := &relay{w: w, api: api, stream: stream}

	// Fast path: the unit was ready a moment ago, so send the request straight
	// away. If the unit turns out to have gone cold, nothing has been written
	// to the client yet and the request falls through to the waiting path.
	if g.knownReady(u) {
		resp, err := g.forward(ctx, u, r, fields)
		if err == nil && !unitGone(resp) {
			out.copy(resp)
			return
		}
		if err == nil {
			resp.Body.Close()
		} else if ctx.Err() != nil {
			return // the client went away
		}
		u.markNotReady()
	}

	// Waiting path: wake the unit, keeping a streaming client's connection alive.
	stop := out.keepAlive(ctx, g.KeepAliveEvery)
	err = g.ensureReady(ctx, u)
	stop()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.Header().Set("Retry-After", "30")
		out.fail(http.StatusServiceUnavailable, "overloaded_error", err.Error())
		return
	}
	resp, err := g.forward(ctx, u, r, fields)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		u.markNotReady()
		out.fail(http.StatusBadGateway, "api_error", "the unit did not answer: "+err.Error())
		return
	}
	if unitGone(resp) {
		resp.Body.Close()
		u.markNotReady()
		w.Header().Set("Retry-After", "10")
		out.fail(http.StatusServiceUnavailable, "overloaded_error", "the unit stopped being ready; retry")
		return
	}
	out.copy(resp)
}

// hopHeaders are not forwarded in either direction. Credentials are dropped
// too: the unit is called with the fleet's inference key, never the client's.
var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
	"Authorization": true, "X-Api-Key": true, "Content-Length": true, "Host": true, "Accept-Encoding": true,
}

// forward sends the request to the unit with the model name rewritten to the
// one the engine serves. The response body is the caller's to close.
func (g *Gateway) forward(ctx context.Context, u *unit, r *http.Request, fields map[string]json.RawMessage) (*http.Response, error) {
	if name := u.engineModel(); name != "" {
		raw, _ := json.Marshal(name)
		fields["model"] = raw
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	target := u.baseURL + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	// The per-request limit covers the whole exchange, so the cancel function
	// travels with the body and runs when the relay closes it.
	rctx, cancel := context.WithTimeout(ctx, u.request)
	req, err := http.NewRequestWithContext(rctx, r.Method, target, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	for k, vs := range r.Header {
		if !hopHeaders[http.CanonicalHeaderKey(k)] {
			req.Header[k] = vs
		}
	}
	req.Header.Set("Authorization", "Bearer "+g.inferenceKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// unitGone reports whether a response means "the unit is not ready" rather
// than an answer from the engine: the agent's 503 before ready, or RunPod's
// 400 when no worker came up in time.
func unitGone(resp *http.Response) bool {
	if resp.StatusCode != http.StatusServiceUnavailable && resp.StatusCode != http.StatusBadRequest {
		return false
	}
	peek, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek), resp.Body), resp.Body}
	s := string(peek)
	return strings.Contains(s, "unit_not_ready") || strings.Contains(s, "timed out waiting for worker")
}

func newRequestID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "req_" + fmt.Sprint(time.Now().UnixNano())
	}
	return "req_" + hex.EncodeToString(b)
}
