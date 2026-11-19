package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// strata runs the Strata server through the upstream image's own entrypoint.
//
// Strata differs from llama.cpp in three ways the agent has to cover:
//   - it is configured through environment variables (MODEL, FAMILY, CONTEXT,
//     PARALLEL, VISION ...), which the child inherits from the container;
//   - it refuses to serve without an API key of its own, so the agent makes one
//     per start and puts it on every proxied request;
//   - its draft layer is not a file on Hugging Face. Our image carries it
//     prepared, and the agent links it to where Strata looks.
type strata struct {
	cfg        EngineConfig
	entrypoint string // upstream entrypoint script
	dataDir    string // Strata's data folder: models/, packs/, mtp/
	draftDir   string // prepared draft layer baked into the image; may be absent
	key        string // Strata's API key for this start
	environ    func() []string
	http       *http.Client
}

func newStrata(cfg EngineConfig) (*strata, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("strata api key: %w", err)
	}
	return &strata{
		cfg: cfg, entrypoint: "/opt/strata/entrypoint.sh", dataDir: "/data/Strata-data",
		draftDir: "/opt/strata-draft/mtp", key: hex.EncodeToString(b),
		environ: os.Environ, http: &http.Client{Timeout: 3 * time.Second},
	}, nil
}

func (e *strata) Name() string { return "strata" }

func (e *strata) Command(ctx context.Context, paths map[Role][]string) (*exec.Cmd, error) {
	models := paths[RoleModel]
	if len(models) == 0 {
		return nil, fmt.Errorf("strata needs files with role %q", RoleModel)
	}
	if len(paths[RoleDraft]) > 0 {
		return nil, errors.New("strata's draft layer ships in the image; remove the file with role \"draft\"")
	}
	// Strata takes the folder that holds the shards of one model.
	modelDir := filepath.Dir(models[0])
	for _, m := range models[1:] {
		if filepath.Dir(m) != modelDir {
			return nil, fmt.Errorf("strata needs all model shards in one folder, got %s and %s", modelDir, filepath.Dir(m))
		}
	}
	env := append(e.environ(),
		"PORT="+strconv.Itoa(e.cfg.Port),
		"API_KEY="+e.key,
		"MODEL_PATH="+modelDir,
	)
	if p := paths[RoleProjector]; len(p) > 0 {
		// Setup looks for the projector by name in the data folder's models/.
		if err := link(p[0], filepath.Join(e.dataDir, "models", filepath.Base(p[0]))); err != nil {
			return nil, fmt.Errorf("strata projector: %w", err)
		}
		if !hasEnv(env, "VISION") {
			env = append(env, "VISION=on")
		}
	}
	// Setup skips fetching and packing the draft layer when mtp/rt/experts.bin
	// is already in the data folder.
	if _, err := os.Stat(filepath.Join(e.draftDir, "rt", "experts.bin")); err == nil {
		if err := link(e.draftDir, filepath.Join(e.dataDir, "mtp")); err != nil {
			return nil, fmt.Errorf("strata draft layer: %w", err)
		}
	}

	cmd := exec.CommandContext(ctx, e.entrypoint, "run")
	cmd.Env = env
	// The entrypoint unloads the model cleanly on SIGTERM; give it time before
	// the default kill.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	return cmd, nil
}

// link makes dst a symlink to src unless dst already exists.
func link(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Symlink(src, dst)
}

func hasEnv(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") && len(kv) > len(key)+1 {
			return true
		}
	}
	return false
}

// Ready asks for the model list: Strata opens its port only once the model is
// downloaded, prepared and loaded.
func (e *strata) Ready(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/models", e.cfg.Port), nil)
	if err != nil {
		return false
	}
	e.Authorize(req)
	resp, err := e.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Authorize replaces the caller's credentials with Strata's key, in both the
// OpenAI and the Anthropic header.
func (e *strata) Authorize(r *http.Request) {
	r.Header.Set("Authorization", "Bearer "+e.key)
	r.Header.Set("X-Api-Key", e.key)
}
