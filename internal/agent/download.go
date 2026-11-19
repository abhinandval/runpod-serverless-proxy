package agent

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Role says what a model file is for; the engine decides where it goes.
type Role string

const (
	RoleModel     Role = "model"     // main weights, one file or several shards
	RoleProjector Role = "projector" // vision projector (mmproj)
	RoleDraft     Role = "draft"     // MTP head or draft model for speculative decoding
)

// File is one file to fetch from a Hugging Face repository.
type File struct {
	Role Role   `json:"role"`
	Repo string `json:"repo"`
	File string `json:"file"`
}

// Downloader fetches model files to local disk.
type Downloader interface {
	// Size returns the file's size in bytes, or 0 if it cannot be determined.
	Size(ctx context.Context, f File) int64
	// Fetch downloads the file and returns its local path.
	Fetch(ctx context.Context, f File) (path string, err error)
	// Dir is the directory Fetch writes into; the agent watches its size to
	// report progress while Fetch runs.
	Dir(f File) string
}

// HFDownloader downloads with the Hugging Face CLI in high-performance mode,
// which measured about twice as fast as aria2c on RunPod workers.
type HFDownloader struct {
	Root     string // files land in Root/<repo>/<file>
	Token    string // optional; needed for gated or private repositories
	Endpoint string // defaults to https://huggingface.co
	Bin      string // defaults to "hf"
	HTTP     *http.Client
}

func (d *HFDownloader) endpoint() string {
	if d.Endpoint != "" {
		return strings.TrimRight(d.Endpoint, "/")
	}
	return "https://huggingface.co"
}

// Dir implements Downloader.
func (d *HFDownloader) Dir(f File) string { return filepath.Join(d.Root, filepath.FromSlash(f.Repo)) }

// Size implements Downloader. The Hub answers a HEAD on a resolve URL with the
// real size in X-Linked-Size (large files) or Content-Length.
func (d *HFDownloader) Size(ctx context.Context, f File) int64 {
	client := d.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // the size headers are on the first response
		}}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, d.endpoint()+"/"+f.Repo+"/resolve/main/"+f.File, nil)
	if err != nil {
		return 0
	}
	if d.Token != "" {
		req.Header.Set("Authorization", "Bearer "+d.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	for _, h := range []string{"X-Linked-Size", "Content-Length"} {
		if n, err := strconv.ParseInt(resp.Header.Get(h), 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// command builds the CLI invocation; split out so it can be tested without running it.
func (d *HFDownloader) command(ctx context.Context, f File) *exec.Cmd {
	bin := d.Bin
	if bin == "" {
		bin = "hf"
	}
	cmd := exec.CommandContext(ctx, bin, "download", f.Repo, f.File, "--local-dir", d.Dir(f))
	cmd.Env = append(os.Environ(),
		"HF_XET_HIGH_PERFORMANCE=1",
		"HF_HUB_DISABLE_PROGRESS_BARS=1",
		"HF_HOME="+filepath.Join(d.Root, ".hf-home"),
	)
	if d.Endpoint != "" {
		cmd.Env = append(cmd.Env, "HF_ENDPOINT="+d.endpoint())
	}
	if d.Token != "" {
		cmd.Env = append(cmd.Env, "HF_TOKEN="+d.Token)
	}
	return cmd
}

// Fetch implements Downloader.
func (d *HFDownloader) Fetch(ctx context.Context, f File) (string, error) {
	if err := os.MkdirAll(d.Dir(f), 0o755); err != nil {
		return "", err
	}
	out, err := d.command(ctx, f).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("hf download %s %s: %w: %s", f.Repo, f.File, err, tail(string(out), 400))
	}
	path := filepath.Join(d.Dir(f), filepath.FromSlash(f.File))
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("hf download %s %s reported success but %s is missing", f.Repo, f.File, path)
	}
	return path, nil
}

// dirSize returns the bytes on disk under dir, including partial downloads.
func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	return n
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
