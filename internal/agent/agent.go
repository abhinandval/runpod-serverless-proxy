package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"
)

// Config is the agent's configuration, rendered into the container's
// environment by the gateway.
type Config struct {
	Listen      string        // address the agent serves on, e.g. ":8080"
	EnginePort  int           // private port for the engine
	Files       []File        // model files to download before starting the engine
	ColdStart   time.Duration // longest the unit may take to become ready
	FailLinger  time.Duration // how long a failed agent keeps answering before it exits
	WorkerID    string
	ModelName   string
	StatusEvery time.Duration // how often download progress is sampled
}

// ConfigFromEnv reads the configuration from environment variables.
func ConfigFromEnv(getenv func(string) string) (Config, string, EngineConfig, error) {
	cfg := Config{
		Listen:      ":" + envOr(getenv, "PORT", "8080"),
		EnginePort:  8090,
		ColdStart:   15 * time.Minute,
		FailLinger:  60 * time.Second,
		WorkerID:    getenv("RUNPOD_POD_ID"),
		ModelName:   getenv("AGENT_MODEL_NAME"),
		StatusEvery: time.Second,
	}
	engine := getenv("AGENT_ENGINE")
	if engine == "" {
		return cfg, "", EngineConfig{}, errors.New("AGENT_ENGINE is not set")
	}
	if v := getenv("AGENT_ENGINE_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return cfg, "", EngineConfig{}, fmt.Errorf("AGENT_ENGINE_PORT: %w", err)
		}
		cfg.EnginePort = p
	}
	if v := getenv("AGENT_COLD_START_SECONDS"); v != "" {
		s, err := strconv.Atoi(v)
		if err != nil || s <= 0 {
			return cfg, "", EngineConfig{}, fmt.Errorf("AGENT_COLD_START_SECONDS: %q is not a positive number", v)
		}
		cfg.ColdStart = time.Duration(s) * time.Second
	}
	if v := getenv("AGENT_FILES"); v != "" {
		if err := json.Unmarshal([]byte(v), &cfg.Files); err != nil {
			return cfg, "", EngineConfig{}, fmt.Errorf("AGENT_FILES: %w", err)
		}
	}
	ec := EngineConfig{Port: cfg.EnginePort, ModelName: cfg.ModelName, SpecType: getenv("AGENT_SPEC_TYPE")}
	if v := getenv("AGENT_ENGINE_ARGS"); v != "" {
		if err := json.Unmarshal([]byte(v), &ec.ExtraArgs); err != nil {
			return cfg, "", EngineConfig{}, fmt.Errorf("AGENT_ENGINE_ARGS must be a JSON array of strings: %w", err)
		}
	}
	return cfg, engine, ec, nil
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// Agent runs one unit: download, start the engine, serve.
type Agent struct {
	cfg      Config
	engine   Engine
	dl       Downloader
	track    *tracker
	inFlight atomic.Int64
	proxy    http.Handler
	// start runs the engine process and returns a channel that yields when it
	// exits. Replaced in tests.
	start func(cmd *exec.Cmd) (<-chan error, error)
	sleep func(context.Context, time.Duration)
}

// New builds an agent. transport is used to reach the engine; nil means the default.
func New(cfg Config, engine Engine, dl Downloader, transport http.RoundTripper) *Agent {
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(cfg.EnginePort)}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1 // write through at once so streamed tokens are not buffered
	rp.Transport = transport
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]string{"type": "engine_unreachable", "message": err.Error()}})
	}
	return &Agent{
		cfg: cfg, engine: engine, dl: dl, proxy: rp,
		track: newTracker(engine.Name(), cfg.ModelName, cfg.WorkerID, time.Now),
		start: startProcess,
		sleep: func(ctx context.Context, d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		},
	}
}

func startProcess(cmd *exec.Cmd) (<-chan error, error) {
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done, nil
}

// Status returns the current status.
func (a *Agent) Status() Status { return a.track.snapshot(a.inFlight.Load()) }

// ServeHTTP implements the unit contract:
//
//	GET /ping          200 as soon as the agent is up (RunPod's health poll)
//	GET /agent/status  the status JSON, in every phase
//	anything else      proxied to the engine when ready, otherwise 503 + status
func (a *Agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/ping":
		w.WriteHeader(http.StatusOK)
		return
	case "/agent/status":
		writeJSON(w, http.StatusOK, a.Status())
		return
	}
	if a.track.phase() != PhaseReady {
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":  map[string]string{"type": "unit_not_ready", "message": "the engine is not ready yet"},
			"status": a.Status(),
		})
		return
	}
	a.inFlight.Add(1)
	defer a.inFlight.Add(-1)
	a.engine.Authorize(r)
	a.proxy.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// Run brings the unit up and blocks until the engine exits, the cold-start
// timeout passes, or ctx is cancelled. It returns nil only on cancellation.
// The HTTP server is the caller's; Run only drives the lifecycle.
func (a *Agent) Run(ctx context.Context) error {
	err := a.run(ctx)
	if err == nil || ctx.Err() != nil {
		return nil
	}
	a.track.fail(err)
	log.Printf("agent: failed: %v", err)
	// Keep answering for a while so the gateway can read why, then exit so a
	// broken unit stops billing instead of running forever.
	a.sleep(ctx, a.cfg.FailLinger)
	return err
}

func (a *Agent) run(ctx context.Context) error {
	startCtx, cancel := context.WithTimeout(ctx, a.cfg.ColdStart)
	defer cancel()

	paths, err := a.download(startCtx)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	a.track.set(PhaseLoading)
	// The engine outlives the start-up deadline, so it gets the outer context.
	cmd, err := a.engine.Command(ctx, paths)
	if err != nil {
		return err
	}
	log.Printf("agent: starting %s", a.engine.Name())
	exited, err := a.start(cmd)
	if err != nil {
		return fmt.Errorf("start %s: %w", a.engine.Name(), err)
	}

	for !a.engine.Ready(startCtx) {
		select {
		case err := <-exited:
			return fmt.Errorf("%s exited while loading: %v", a.engine.Name(), err)
		case <-startCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%s was not ready within %s", a.engine.Name(), a.cfg.ColdStart)
		default:
			a.sleep(startCtx, 500*time.Millisecond)
		}
	}
	a.track.set(PhaseReady)
	log.Printf("agent: ready after %.1fs", a.Status().UptimeSeconds)

	select {
	case err := <-exited:
		return fmt.Errorf("%s exited: %v", a.engine.Name(), err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// download fetches every configured file, one after another, and reports
// progress from the bytes on disk.
func (a *Agent) download(ctx context.Context) (map[Role][]string, error) {
	paths := map[Role][]string{}
	if len(a.cfg.Files) == 0 {
		return paths, nil
	}
	a.track.set(PhaseDownloading)
	prog := Download{Files: len(a.cfg.Files)}
	for _, f := range a.cfg.Files {
		prog.TotalBytes += a.dl.Size(ctx, f)
	}
	a.track.setDownload(prog)

	var finished int64 // bytes of files already completed
	for i, f := range a.cfg.Files {
		before := dirSize(a.dl.Dir(f))
		stop := make(chan struct{})
		sampled := make(chan struct{})
		go func() {
			defer close(sampled)
			t := time.NewTicker(a.cfg.StatusEvery)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					p := prog
					p.FilesDone = i
					p.DoneBytes = finished + dirSize(a.dl.Dir(f)) - before
					a.track.setDownload(p)
				}
			}
		}()
		log.Printf("agent: downloading %s %s (%s)", f.Repo, f.File, f.Role)
		path, err := a.dl.Fetch(ctx, f)
		close(stop)
		<-sampled
		if err != nil {
			return nil, err
		}
		if info, err := os.Stat(path); err == nil {
			finished += info.Size()
		}
		prog.FilesDone, prog.DoneBytes = i+1, finished
		a.track.setDownload(prog)
		paths[f.Role] = append(paths[f.Role], path)
	}
	return paths, nil
}
