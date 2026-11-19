// Command probe is the Phase 0 measurement tool. It creates tagged test endpoints
// on RunPod Serverless, runs timing probes against them, and records everything
// as JSONL under spike/results/. See spike/README.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"runpod-serverless-proxy/internal/runpod/infer"
	"runpod-serverless-proxy/internal/runpod/manage"
)

const (
	stateFile  = "spike/state.json"
	resultsDir = "spike/results"
	// ownership tag: `down` only ever deletes endpoints carrying it.
	tagKey   = "managed-by"
	tagValue = "rsp-spike"
	// served model name, identical on every test endpoint.
	servedModel = "spike-model"
)

type command struct {
	name string
	help string
	run  func(ctx context.Context, args []string) error
}

var commands []command

func init() {
	commands = []command{
		{"catalog", "list serverless GPU pools with VRAM, price and availability", cmdCatalog},
		{"up", "create the tagged test endpoints (billable while workers run)", cmdUp},
		{"status", "show test endpoints, their workers and queue health", cmdStatus},
		{"down", "delete every endpoint tagged " + tagKey + "=" + tagValue, cmdDown},
		{"chat", "one chat request (OpenAI or Anthropic format), timed, with optional retry", cmdChat},
		{"burst", "N parallel streaming chat requests", cmdBurst},
		{"disconnect", "drop a stream mid-response, then observe workers", cmdDisconnect},
		{"queue", "raw queue API: run+stream polling, status-sync, or runsync", cmdQueue},
		{"watch", "sample health and workers once a second", cmdWatch},
		{"patch", "patch scaling or image, then follow the release rollout", cmdPatch},
	}
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		usage()
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	for _, c := range commands {
		if c.name == os.Args[1] {
			if err := c.run(ctx, os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
	usage()
	os.Exit(2)
}

func usage() {
	fmt.Println("usage: probe <command> [flags]   (probe <command> -h for flags)")
	fmt.Println()
	for _, c := range commands {
		fmt.Printf("  %-11s %s\n", c.name, c.help)
	}
	fmt.Println()
	fmt.Println("env: RUNPOD_MANAGEMENT_KEY (create/patch/delete), RUNPOD_INFERENCE_KEY (requests;")
	fmt.Println("     falls back to the management key)")
}

// ---- keys and clients ----

func manageClient(rec *recorder) (*manage.Client, error) {
	key := os.Getenv("RUNPOD_MANAGEMENT_KEY")
	if key == "" {
		return nil, errors.New("RUNPOD_MANAGEMENT_KEY is not set (see .env.example)")
	}
	c := manage.New(key)
	if rec != nil {
		c.Trace = func(call manage.Call) {
			f := fields{
				"method": call.Method, "path": call.Path, "status": call.Status,
				"ms": float64(call.Duration.Microseconds()) / 1000,
			}
			if call.RateLimit != "" {
				f["ratelimit"] = call.RateLimit
			}
			if call.RateLimitPolicy != "" {
				f["ratelimit_policy"] = call.RateLimitPolicy
			}
			if call.RetryAfter != "" {
				f["retry_after"] = call.RetryAfter
			}
			if call.Err != nil {
				f["err"] = call.Err.Error()
			}
			rec.event("manage_call", f)
		}
	}
	return c, nil
}

func inferClient() (*infer.Client, error) {
	key := os.Getenv("RUNPOD_INFERENCE_KEY")
	if key == "" {
		key = os.Getenv("RUNPOD_MANAGEMENT_KEY")
	}
	if key == "" {
		return nil, errors.New("set RUNPOD_INFERENCE_KEY (or RUNPOD_MANAGEMENT_KEY)")
	}
	return infer.New(key), nil
}

// ---- local state: which endpoints `up` created ----

type target struct {
	Key  string              `json:"key"` // a, b, c
	ID   string              `json:"id"`
	Name string              `json:"name"`
	Type manage.EndpointType `json:"type"`
	URLs map[string]string   `json:"urls"`
	Note string              `json:"note"`
}

// base is the root URL for requests to this endpoint.
func (t target) base() string {
	if t.Type == manage.TypeLoadBalancer {
		if u := t.URLs["base"]; u != "" {
			return strings.TrimRight(u, "/")
		}
		return infer.LBBase(t.ID)
	}
	if u := t.URLs["run"]; strings.HasSuffix(u, "/run") {
		return strings.TrimSuffix(u, "/run")
	}
	return infer.QueueBase(t.ID)
}

// apiRoot is where OpenAI/Anthropic-style paths (/chat/completions, /messages) hang.
func (t target) apiRoot() string {
	if t.Type == manage.TypeLoadBalancer {
		return t.base() + "/v1"
	}
	return t.base() + "/openai/v1"
}

// healthURL is the queue /health endpoint or the load balancer's health path.
func (t target) healthURL() string {
	if u := t.URLs["health"]; u != "" {
		return u
	}
	if t.Type == manage.TypeLoadBalancer {
		return t.base() + "/ping"
	}
	return t.base() + "/health"
}

type state struct {
	TagID   string            `json:"tag_id"`
	Targets map[string]target `json:"targets"`
}

func loadState() (*state, error) {
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s not found: run `probe up` first", stateFile)
		}
		return nil, err
	}
	var s state
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", stateFile, err)
	}
	return &s, nil
}

func saveState(s *state) error {
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(stateFile, append(raw, '\n'), 0o644)
}

func (s *state) get(key string) (target, error) {
	t, ok := s.Targets[strings.ToLower(key)]
	if !ok {
		keys := make([]string, 0, len(s.Targets))
		for k := range s.Targets {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return target{}, fmt.Errorf("unknown target %q (have: %s)", key, strings.Join(keys, ", "))
	}
	return t, nil
}

func (s *state) sorted() []target {
	out := make([]target, 0, len(s.Targets))
	for _, t := range s.Targets {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ---- JSONL recorder ----

type fields map[string]any

type recorder struct {
	mu    sync.Mutex
	f     *os.File
	path  string
	start time.Time
	base  fields
}

// newRecorder opens spike/results/<utc>-<probe>[-<target>][-<label>].jsonl.
func newRecorder(probe, targetKey, label string) (*recorder, error) {
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	parts := []string{now.Format("20060102T150405Z"), probe}
	base := fields{"probe": probe}
	if targetKey != "" {
		parts = append(parts, targetKey)
		base["target"] = targetKey
	}
	if label != "" {
		parts = append(parts, label)
		base["label"] = label
	}
	path := filepath.Join(resultsDir, strings.Join(parts, "-")+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &recorder{f: f, path: path, start: now, base: base}, nil
}

func (r *recorder) event(name string, f fields) {
	row := fields{}
	for k, v := range r.base {
		row[k] = v
	}
	for k, v := range f {
		row[k] = v
	}
	now := time.Now().UTC()
	row["ts"] = now.Format(time.RFC3339Nano)
	row["t_ms"] = float64(now.Sub(r.start).Microseconds()) / 1000
	row["event"] = name
	raw, err := json.Marshal(row)
	if err != nil {
		raw = []byte(fmt.Sprintf(`{"event":"marshal_error","err":%q}`, err.Error()))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.f.Write(append(raw, '\n'))
}

func (r *recorder) close() {
	r.f.Close()
	fmt.Println("recorded:", r.path)
}

// timingFields flattens a Timing into an event payload.
func timingFields(t infer.Timing) fields {
	raw, _ := json.Marshal(t)
	f := fields{}
	_ = json.Unmarshal(raw, &f)
	return f
}

func summarize(prefix string, t infer.Timing) {
	line := fmt.Sprintf("%s status=%d headers=%.0fms first_byte=%.0fms first_data=%.0fms total=%.0fms bytes=%d data_lines=%d max_gap=%.0fms",
		prefix, t.Status, t.HeadersMs, t.FirstByteMs, t.FirstDataMs, t.TotalMs, t.Bytes, t.DataLines, t.MaxGapMs)
	if t.Err != "" {
		line += " err=" + t.Err
	}
	fmt.Println(line)
	if !t.OK() && t.BodyHead != "" {
		fmt.Println("  body:", strings.TrimSpace(firstN(t.BodyHead, 300)))
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func intPtr(v int) *int { return &v }
