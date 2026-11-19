package agent

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"time"
)

// Engine knows how to start one inference server and tell when it is ready.
type Engine interface {
	Name() string
	// Command builds the process to run. paths maps each role to the local
	// files downloaded for it, in the order they were configured.
	Command(ctx context.Context, paths map[Role][]string) (*exec.Cmd, error)
	// Ready reports whether the engine can answer requests on its private port.
	Ready(ctx context.Context) bool
	// Authorize adjusts a request that is about to be proxied to the engine,
	// for engines that want credentials of their own. The caller was already
	// authenticated by RunPod's load balancer.
	Authorize(r *http.Request)
}

// EngineConfig is what every engine needs from the agent's configuration.
type EngineConfig struct {
	Port      int      // private port the engine listens on (loopback only)
	ModelName string   // name the engine serves the model under
	ExtraArgs []string // engine-specific flags rendered by the gateway
	SpecType  string   // speculative decoding type when a draft file is given
}

// NewEngine returns the engine with the given name.
func NewEngine(name string, cfg EngineConfig) (Engine, error) {
	switch name {
	case "llama.cpp", "llamacpp":
		return &llamaCpp{cfg: cfg, bin: "/app/llama-server", http: &http.Client{Timeout: 3 * time.Second}}, nil
	case "strata":
		return newStrata(cfg)
	}
	return nil, fmt.Errorf("unknown engine %q", name)
}

// llamaCpp runs llama-server from the upstream llama.cpp image.
type llamaCpp struct {
	cfg  EngineConfig
	bin  string
	http *http.Client
}

func (e *llamaCpp) Name() string { return "llama.cpp" }

// Authorize does nothing: llama-server runs without a key on loopback.
func (e *llamaCpp) Authorize(*http.Request) {}

func (e *llamaCpp) Command(ctx context.Context, paths map[Role][]string) (*exec.Cmd, error) {
	models := paths[RoleModel]
	if len(models) == 0 {
		return nil, fmt.Errorf("llama.cpp needs a file with role %q", RoleModel)
	}
	// For a model split into shards llama-server takes the first one and finds the rest.
	args := []string{"-m", models[0], "--host", "127.0.0.1", "--port", strconv.Itoa(e.cfg.Port), "--jinja"}
	if e.cfg.ModelName != "" {
		args = append(args, "--alias", e.cfg.ModelName)
	}
	if p := paths[RoleProjector]; len(p) > 0 {
		args = append(args, "--mmproj", p[0])
	}
	if d := paths[RoleDraft]; len(d) > 0 {
		args = append(args, "--spec-draft-model", d[0])
		if e.cfg.SpecType != "" {
			args = append(args, "--spec-type", e.cfg.SpecType)
		}
	}
	args = append(args, e.cfg.ExtraArgs...)
	return exec.CommandContext(ctx, e.bin, args...), nil
}

// Ready polls llama-server's /health, which answers 503 while the model loads.
func (e *llamaCpp) Ready(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", e.cfg.Port), nil)
	if err != nil {
		return false
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
