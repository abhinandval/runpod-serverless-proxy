package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeEngine struct {
	ready   atomic.Bool
	gotPath map[Role][]string
}

func (e *fakeEngine) Name() string { return "fake" }
func (e *fakeEngine) Command(_ context.Context, p map[Role][]string) (*exec.Cmd, error) {
	e.gotPath = p
	return exec.Command("true"), nil
}
func (e *fakeEngine) Ready(context.Context) bool { return e.ready.Load() }
func (e *fakeEngine) Authorize(r *http.Request)  { r.Header.Set("Authorization", "Bearer engine-key") }

// fakeDownloader writes a small file per request instead of calling the Hub.
type fakeDownloader struct {
	root string
	err  error
}

func (d *fakeDownloader) Dir(f File) string                { return filepath.Join(d.root, f.Repo) }
func (d *fakeDownloader) Size(context.Context, File) int64 { return 5 }
func (d *fakeDownloader) Fetch(_ context.Context, f File) (string, error) {
	if d.err != nil {
		return "", d.err
	}
	path := filepath.Join(d.Dir(f), f.File)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte("12345"), 0o644)
}

// engineTransport stands in for the engine's HTTP server.
type engineTransport func(*http.Request) (*http.Response, error)

func (f engineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newTestAgent(t *testing.T, eng *fakeEngine, dl Downloader, files []File) (*Agent, chan error) {
	t.Helper()
	cfg := Config{EnginePort: 8090, Files: files, ColdStart: 2 * time.Second, FailLinger: time.Millisecond,
		ModelName: "m", WorkerID: "w1", StatusEvery: 5 * time.Millisecond}
	a := New(cfg, eng, dl, engineTransport(func(r *http.Request) (*http.Response, error) {
		body := "data: one\n\ndata: two\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Seen-Path": {r.URL.Path}, "X-Seen-Auth": {r.Header.Get("Authorization")}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	exited := make(chan error, 1)
	a.start = func(*exec.Cmd) (<-chan error, error) { return exited, nil }
	return a, exited
}

func get(a *Agent, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
	return rec
}

func waitPhase(t *testing.T, a *Agent, want Phase) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for a.Status().Phase != want {
		if time.Now().After(deadline) {
			t.Fatalf("phase = %s, want %s (error: %s)", a.Status().Phase, want, a.Status().Error)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestLifecycleFromColdToReady(t *testing.T) {
	eng := &fakeEngine{}
	files := []File{{Role: RoleModel, Repo: "org/model", File: "m.gguf"}, {Role: RoleProjector, Repo: "org/model", File: "mmproj.gguf"}}
	a, _ := newTestAgent(t, eng, &fakeDownloader{root: t.TempDir()}, files)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// RunPod's health poll and the status route answer in every phase.
	if rec := get(a, "/ping"); rec.Code != http.StatusOK {
		t.Errorf("/ping = %d before ready, want 200", rec.Code)
	}
	waitPhase(t, a, PhaseLoading)

	// A request that races the cold start fails fast and says why.
	rec := get(a, "/v1/chat/completions")
	var notReady struct {
		Error  struct{ Type string }
		Status Status
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &notReady); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" ||
		notReady.Error.Type != "unit_not_ready" || notReady.Status.Phase != PhaseLoading {
		t.Errorf("not-ready response = %d %s", rec.Code, rec.Body.String())
	}

	eng.ready.Store(true)
	waitPhase(t, a, PhaseReady)
	rec = get(a, "/v1/chat/completions")
	if rec.Code != 200 || rec.Header().Get("X-Seen-Path") != "/v1/chat/completions" || !strings.Contains(rec.Body.String(), "data: two") {
		t.Errorf("proxied response = %d %q", rec.Code, rec.Body.String())
	}
	// The engine's own credentials replace the caller's on the way in.
	if got := rec.Header().Get("X-Seen-Auth"); got != "Bearer engine-key" {
		t.Errorf("engine saw Authorization %q", got)
	}

	st := a.Status()
	if st.Download.Files != 2 || st.Download.FilesDone != 2 || st.Download.DoneBytes != 10 || st.Download.TotalBytes != 10 {
		t.Errorf("download = %+v", st.Download)
	}
	for _, p := range []Phase{PhaseStarting, PhaseDownloading, PhaseLoading} {
		if _, ok := st.PhaseSeconds[string(p)]; !ok {
			t.Errorf("no duration recorded for phase %s: %v", p, st.PhaseSeconds)
		}
	}
	if st.Engine != "fake" || st.Model != "m" || st.WorkerID != "w1" || st.InFlight != 0 {
		t.Errorf("status = %+v", st)
	}
	if len(eng.gotPath[RoleModel]) != 1 || len(eng.gotPath[RoleProjector]) != 1 {
		t.Errorf("engine got paths %v", eng.gotPath)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run after cancel = %v, want nil", err)
	}
}

func TestRunFailsWhenEngineExitsWhileLoading(t *testing.T) {
	a, exited := newTestAgent(t, &fakeEngine{}, &fakeDownloader{root: t.TempDir()}, nil)
	exited <- context.DeadlineExceeded // any error: the process died
	err := a.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exited while loading") {
		t.Fatalf("Run = %v", err)
	}
	if st := a.Status(); st.Phase != PhaseFailed || st.Error == "" {
		t.Errorf("status = %+v", st)
	}
	if rec := get(a, "/v1/models"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("request after failure = %d, want 503", rec.Code)
	}
}

func TestRunFailsOnColdStartTimeout(t *testing.T) {
	a, _ := newTestAgent(t, &fakeEngine{}, &fakeDownloader{root: t.TempDir()}, nil)
	a.cfg.ColdStart = 30 * time.Millisecond
	err := a.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not ready within") {
		t.Fatalf("Run = %v", err)
	}
}

func TestRunFailsWhenDownloadFails(t *testing.T) {
	dl := &fakeDownloader{root: t.TempDir(), err: context.Canceled}
	a, _ := newTestAgent(t, &fakeEngine{}, dl, []File{{Role: RoleModel, Repo: "o/m", File: "m.gguf"}})
	if err := a.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "download") {
		t.Fatalf("Run = %v", err)
	}
}

func TestRunFailsWhenReadyEngineExits(t *testing.T) {
	eng := &fakeEngine{}
	eng.ready.Store(true)
	a, exited := newTestAgent(t, eng, &fakeDownloader{root: t.TempDir()}, nil)
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	waitPhase(t, a, PhaseReady)
	exited <- nil
	if err := <-done; err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("Run = %v", err)
	}
}

func TestLlamaCppCommand(t *testing.T) {
	e, err := NewEngine("llama.cpp", EngineConfig{Port: 8090, ModelName: "qwen", SpecType: "draft-mtp", ExtraArgs: []string{"-c", "16384"}})
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := e.Command(context.Background(), map[Role][]string{
		RoleModel: {"/models/a/m-00001.gguf", "/models/a/m-00002.gguf"}, RoleProjector: {"/models/a/mmproj.gguf"}, RoleDraft: {"/models/b/mtp.gguf"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cmd.Args, " ")
	want := "/app/llama-server -m /models/a/m-00001.gguf --host 127.0.0.1 --port 8090 --jinja --alias qwen " +
		"--mmproj /models/a/mmproj.gguf --spec-draft-model /models/b/mtp.gguf --spec-type draft-mtp -c 16384"
	if got != want {
		t.Errorf("args:\n got %s\nwant %s", got, want)
	}
	if _, err := e.Command(context.Background(), nil); err == nil {
		t.Error("want an error when no model file is given")
	}
	if _, err := NewEngine("nope", EngineConfig{}); err == nil {
		t.Error("want an error for an unknown engine")
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{
		"PORT": "9000", "AGENT_ENGINE": "llama.cpp", "AGENT_ENGINE_PORT": "9001", "AGENT_COLD_START_SECONDS": "300",
		"AGENT_MODEL_NAME": "qwen", "RUNPOD_POD_ID": "pod1", "AGENT_SPEC_TYPE": "draft-mtp",
		"AGENT_FILES":       `[{"role":"model","repo":"o/m","file":"m.gguf"}]`,
		"AGENT_ENGINE_ARGS": `["-c","8192"]`,
	}
	cfg, engine, ec, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9000" || cfg.EnginePort != 9001 || cfg.ColdStart != 5*time.Minute || cfg.WorkerID != "pod1" ||
		len(cfg.Files) != 1 || cfg.Files[0].Role != RoleModel || engine != "llama.cpp" ||
		ec.Port != 9001 || ec.ModelName != "qwen" || ec.SpecType != "draft-mtp" || len(ec.ExtraArgs) != 2 {
		t.Errorf("cfg=%+v engine=%s ec=%+v", cfg, engine, ec)
	}
	if _, _, _, err := ConfigFromEnv(func(string) string { return "" }); err == nil {
		t.Error("want an error when AGENT_ENGINE is missing")
	}
	env["AGENT_ENGINE_ARGS"] = `{"c":8192}`
	if _, _, _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Error("want an error when AGENT_ENGINE_ARGS is not an array")
	}
}

func TestHFDownloaderCommandAndSize(t *testing.T) {
	d := &HFDownloader{Root: "/models", Token: "tok", HTTP: &http.Client{Transport: engineTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodHead || r.URL.String() != "https://huggingface.co/o/m/resolve/main/sub/m.gguf" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("size request = %s %s auth=%q", r.Method, r.URL, r.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"X-Linked-Size": {"17659407264"}, "Content-Length": {"1092"}}, Body: http.NoBody}, nil
	})}}
	f := File{Role: RoleModel, Repo: "o/m", File: "sub/m.gguf"}
	if n := d.Size(context.Background(), f); n != 17659407264 {
		t.Errorf("Size = %d", n)
	}
	cmd := d.command(context.Background(), f)
	if got := strings.Join(cmd.Args, " "); got != "hf download o/m sub/m.gguf --local-dir /models/o/m" {
		t.Errorf("args = %s", got)
	}
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{"HF_XET_HIGH_PERFORMANCE=1", "HF_TOKEN=tok", "HF_HOME=/models/.hf-home"} {
		if !strings.Contains(env, want) {
			t.Errorf("env is missing %s", want)
		}
	}
}
