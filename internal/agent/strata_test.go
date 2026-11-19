package agent

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStrata(t *testing.T, env ...string) (*strata, string) {
	t.Helper()
	root := t.TempDir()
	e, err := newStrata(EngineConfig{Port: 8090})
	if err != nil {
		t.Fatal(err)
	}
	e.dataDir = filepath.Join(root, "data", "Strata-data")
	e.draftDir = filepath.Join(root, "draft", "mtp")
	e.environ = func() []string { return env }
	return e, root
}

func envOf(env []string, key string) string {
	out := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			out = strings.TrimPrefix(kv, key+"=") // the last one wins, as in exec
		}
	}
	return out
}

func TestStrataCommand(t *testing.T) {
	e, root := newTestStrata(t, "MODEL=IQ2_XS", "FAMILY=qwen", "PORT=8080")
	// A prepared draft layer, as the image ships it.
	if err := os.MkdirAll(filepath.Join(e.draftDir, "rt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.draftDir, "rt", "experts.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	shards := filepath.Join(root, "models", "org", "repo", "IQ2_XS")
	proj := filepath.Join(root, "models", "org", "repo", "mmproj.gguf")
	cmd, err := e.Command(context.Background(), map[Role][]string{
		RoleModel:     {filepath.Join(shards, "m-00001-of-00002.gguf"), filepath.Join(shards, "m-00002-of-00002.gguf")},
		RoleProjector: {proj},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmd.Args, " "); got != "/opt/strata/entrypoint.sh run" {
		t.Errorf("args = %q", got)
	}
	for key, want := range map[string]string{
		"PORT": "8090", "MODEL_PATH": shards, "API_KEY": e.key, "MODEL": "IQ2_XS", "FAMILY": "qwen", "VISION": "on",
	} {
		if got := envOf(cmd.Env, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if len(e.key) < 32 {
		t.Errorf("key %q is too short for Strata", e.key)
	}
	if got, _ := os.Readlink(filepath.Join(e.dataDir, "mtp")); got != e.draftDir {
		t.Errorf("draft layer link -> %q, want %q", got, e.draftDir)
	}
	if got, _ := os.Readlink(filepath.Join(e.dataDir, "models", "mmproj.gguf")); got != proj {
		t.Errorf("projector link -> %q, want %q", got, proj)
	}
	// A second start in the same container must not trip over the links.
	if _, err := e.Command(context.Background(), map[Role][]string{RoleModel: {filepath.Join(shards, "m-00001-of-00002.gguf")}}); err != nil {
		t.Errorf("second Command: %v", err)
	}
}

func TestStrataCommandWithoutBakedDraftLayer(t *testing.T) {
	e, root := newTestStrata(t, "VISION=off")
	cmd, err := e.Command(context.Background(), map[Role][]string{RoleModel: {filepath.Join(root, "m", "a.gguf")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(e.dataDir, "mtp")); !os.IsNotExist(err) {
		t.Errorf("draft link made without a baked layer: %v", err)
	}
	if got := envOf(cmd.Env, "VISION"); got != "off" {
		t.Errorf("VISION = %q, the unit's own setting must stay", got)
	}
}

func TestStrataCommandRejectsBadFiles(t *testing.T) {
	e, _ := newTestStrata(t)
	for name, paths := range map[string]map[Role][]string{
		"no model":      {},
		"draft file":    {RoleModel: {"/m/a.gguf"}, RoleDraft: {"/m/d.gguf"}},
		"split folders": {RoleModel: {"/m/a.gguf", "/n/b.gguf"}},
	} {
		if _, err := e.Command(context.Background(), paths); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestStrataReadyAndAuthorize(t *testing.T) {
	e, _ := newTestStrata(t)
	code := http.StatusServiceUnavailable
	var seen *http.Request
	e.http = &http.Client{Transport: engineTransport(func(r *http.Request) (*http.Response, error) {
		seen = r
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	if e.Ready(context.Background()) {
		t.Error("ready on 503")
	}
	code = http.StatusOK
	if !e.Ready(context.Background()) {
		t.Error("not ready on 200")
	}
	if seen.URL.String() != "http://127.0.0.1:8090/v1/models" || seen.Header.Get("Authorization") != "Bearer "+e.key {
		t.Errorf("readiness probe = %s with %q", seen.URL, seen.Header.Get("Authorization"))
	}

	r, _ := http.NewRequest(http.MethodPost, "http://unit/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer runpod-key")
	r.Header.Set("x-api-key", "client-key")
	e.Authorize(r)
	if r.Header.Get("Authorization") != "Bearer "+e.key || r.Header.Get("x-api-key") != e.key {
		t.Errorf("headers after Authorize = %v", r.Header)
	}
}
