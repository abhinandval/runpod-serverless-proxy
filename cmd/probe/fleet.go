package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/abhinandval/runpod-serverless-proxy/internal/runpod/manage"
)

// strataShim runs inside the Strata container, in front of the engine. The
// docstring of strata_shim.py explains why it is needed.
//
//go:embed strata_shim.py
var strataShim string

// dlBench is the download benchmark worker; see its docstring.
//
//go:embed dlbench.py
var dlBench string

// ---- catalog ----

func cmdCatalog(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("catalog", flag.ExitOnError)
	fs.Parse(args)
	mc, err := manageClient(nil)
	if err != nil {
		return err
	}
	gpus, err := mc.ListServerlessGPUs(ctx)
	if err != nil {
		return err
	}
	sort.Slice(gpus, func(i, j int) bool {
		if gpus[i].Price.Serverless != gpus[j].Price.Serverless {
			return gpus[i].Price.Serverless < gpus[j].Price.Serverless
		}
		return gpus[i].ID < gpus[j].ID
	})
	fmt.Printf("%-16s %-34s %6s %10s  %s\n", "POOL", "GPU TYPE", "VRAM", "$/GPU/hr", "AVAILABILITY")
	for _, g := range gpus {
		if g.Pool == nil {
			continue
		}
		fmt.Printf("%-16s %-34s %4dGB %10.2f  %s\n", *g.Pool, g.ID, g.Memory, g.Price.Serverless, g.Availability)
	}
	fmt.Println("\npass a POOL value to `probe up -pool <POOL>`")
	return nil
}

// ---- up ----

func cmdUp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	pool := fs.String("pool", os.Getenv("SPIKE_GPU_POOL"), "serverless GPU pool id (see `probe catalog`); comma-separated for several")
	only := fs.String("only", "a,b,c", "which test endpoints to create")
	suffix := fs.String("name-suffix", "", "appended to endpoint names; RunPod requires unique names, and a deleted endpoint's name stays taken for a while")
	exclude := fs.String("exclude-types", "", "comma-separated GPU type ids to remove from the pools, e.g. \"NVIDIA L4\" (applies to every endpoint)")
	maxWorkers := fs.Int("max-workers", 2, "max workers per endpoint")
	idle := fs.Int("idle-timeout", 30, "seconds before an idle worker scales down")
	flashboot := fs.String("flashboot", "FLASHBOOT", "OFF | FLASHBOOT | PRIORITY_FLASHBOOT")
	hfModel := fs.String("model", "Qwen/Qwen2.5-1.5B-Instruct", "Hugging Face model for the vLLM endpoints")
	ggufModel := fs.String("gguf", "unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL", "Hugging Face GGUF repo:quant for llama.cpp (17.7 GB, needs 24 GB VRAM)")
	imgWorker := fs.String("image-worker-vllm", "runpod/worker-v1-vllm:v2.31.0", "image for A (queue)")
	imgVLLM := fs.String("image-vllm", "vllm/vllm-openai:latest", "image for B (load balancer)")
	imgLlama := fs.String("image-llamacpp", "ghcr.io/ggml-org/llama.cpp:server-cuda", "image for C (load balancer)")
	imgAgentLlama := fs.String("image-agent-llamacpp", "ghcr.io/abhinandval/runpod-serverless-proxy/llamacpp:latest", "our llama.cpp image for L (load balancer)")
	imgAgentStrata := fs.String("image-agent-strata", "ghcr.io/abhinandval/runpod-serverless-proxy/strata:latest", "our Strata image for T (load balancer)")
	agentArgs := fs.String("agent-args", `["-ngl","99","-c","16384","-np","2"]`, "llama-server flags for L, as a JSON array")
	imgStrata := fs.String("image-strata", "ghcr.io/niko1221/strata:0.2.7-cuda13", "image for S (load balancer); cuda13 needs driver 580+")
	strataPool := fs.String("strata-pool", "AMPERE_48", "GPU pool for S; IQ2_XS needs about 39 GB of VRAM and host RAM combined")
	strataFamily := fs.String("strata-family", "qwen", "Strata FAMILY")
	strataModel := fs.String("strata-model", "IQ2_XS", "Strata MODEL size")
	strataDisk := fs.Int("strata-disk", 200, "container disk in GB for S; the model is downloaded on every cold start")
	fs.Parse(args)
	if *pool == "" && strings.TrimSpace(strings.ToLower(*only)) != "s" {
		return errors.New("-pool is required: run `probe catalog` and pick a pool with 24 GB of VRAM")
	}
	// Strata insists on an API key of its own. Callers never see it: the shim
	// injects it, and RunPod's key guards the endpoint from outside.
	keyBytes := make([]byte, 24)
	if _, err := rand.Read(keyBytes); err != nil {
		return err
	}
	strataKey := hex.EncodeToString(keyBytes)
	pools := strings.Split(*pool, ",")

	rec, err := newRecorder("up", "", "")
	if err != nil {
		return err
	}
	defer rec.close()
	mc, err := manageClient(rec)
	if err != nil {
		return err
	}

	workers := func() *manage.Workers {
		return &manage.Workers{Min: intPtr(0), Max: intPtr(*maxWorkers), IdleTimeout: intPtr(*idle), InitTimeout: intPtr(900)}
	}
	// Pools are the unit of selection; a pool is narrowed to specific cards by
	// excluding the others.
	var excluded []string
	for _, t := range strings.Split(*exclude, ",") {
		if t = strings.TrimSpace(t); t != "" {
			excluded = append(excluded, t)
		}
	}
	gpu := func() *manage.GPUConfig { return &manage.GPUConfig{Pools: pools, ExcludedTypes: excluded, Count: 1} }

	// The benchmark worker gets the Hugging Face token only if one is set locally.
	benchEnv := map[string]string{"PORT": "8080", "PORT_HEALTH": "8080", "BENCH_PY": dlBench}
	if tok := os.Getenv("HF_TOKEN"); tok != "" {
		benchEnv["HF_TOKEN"] = tok
	}

	// Our own engine image: everything is configured through the agent's environment.
	agentEnv := map[string]string{
		"PORT": "8080", "PORT_HEALTH": "8080",
		"AGENT_MODEL_NAME":  servedModel,
		"AGENT_FILES":       `[{"role":"model","repo":"unsloth/Qwen3.8-27B-GGUF","file":"Qwen3.8-27B-UD-Q4_K_XL.gguf"}]`,
		"AGENT_ENGINE_ARGS": *agentArgs,
	}
	if tok := os.Getenv("HF_TOKEN"); tok != "" {
		agentEnv["HF_TOKEN"] = tok
	}

	// Our Strata image: the agent downloads the shards, Strata's own settings
	// stay plain environment variables.
	strataRepo := "ISTA-DASLab/Qwen3.8-Flash-Next-GSQ-RCO-GGUF"
	strataShard := *strataModel + "/Qwen3.8-Flash-Next-GSQ-RCO-" + *strataModel + "-%05d-of-00002.gguf"
	strataFiles, _ := json.Marshal([]map[string]string{
		{"role": "model", "repo": strataRepo, "file": fmt.Sprintf(strataShard, 1)},
		{"role": "model", "repo": strataRepo, "file": fmt.Sprintf(strataShard, 2)},
	})
	agentStrataEnv := map[string]string{
		"PORT": "8080", "PORT_HEALTH": "8080",
		"FAMILY": *strataFamily, "MODEL": *strataModel,
		"AGENT_FILES": string(strataFiles), "AGENT_COLD_START_SECONDS": "1500",
	}
	if tok := os.Getenv("HF_TOKEN"); tok != "" {
		agentStrataEnv["HF_TOKEN"] = tok
	}

	specs := map[string]struct {
		note string
		req  manage.CreateEndpointRequest
	}{
		"a": {
			note: "queue + official worker-vllm (/run, /runsync, /openai/v1)",
			req: manage.CreateEndpointRequest{
				Name: namePrefix + "a-queue-vllm" + *suffix,
				Type: manage.TypeQueue,
				Container: manage.Container{
					Image: *imgWorker,
					Disk:  30,
					Env: map[string]string{
						"MODEL_NAME":                        *hfModel,
						"OPENAI_SERVED_MODEL_NAME_OVERRIDE": servedModel,
						"MAX_MODEL_LEN":                     "8192",
						"ENABLE_AUTO_TOOL_CHOICE":           "true",
						"TOOL_CALL_PARSER":                  "hermes",
					},
				},
				GPU: gpu(), Workers: workers(),
				Scaling: manage.Scaling{Type: manage.ScalingQueueDelay, QueueDelay: 4},
				Timeout: 300000, Flashboot: *flashboot,
			},
		},
		"b": {
			note: "load balancer + stock vLLM server",
			req: manage.CreateEndpointRequest{
				Name: namePrefix + "b-lb-vllm" + *suffix,
				Type: manage.TypeLoadBalancer,
				Container: manage.Container{
					Image: *imgVLLM,
					Disk:  30,
					Cmd: []string{
						"--model", *hfModel, "--served-model-name", servedModel,
						"--host", "0.0.0.0", "--port", "8000", "--max-model-len", "8192",
						"--enable-auto-tool-choice", "--tool-call-parser", "hermes",
					},
					// vLLM serves /ping itself, which is what RunPod polls.
					Env:   map[string]string{"PORT": "8000", "PORT_HEALTH": "8000"},
					Ports: []string{"8000/http"},
				},
				GPU: gpu(), Workers: workers(),
				Scaling: manage.Scaling{Type: manage.ScalingRequestCount, RequestCount: 4},
				Timeout: 300000, Flashboot: *flashboot,
			},
		},
		"c": {
			note: "load balancer + stock llama.cpp server",
			req: manage.CreateEndpointRequest{
				Name: namePrefix + "c-lb-llamacpp" + *suffix,
				Type: manage.TypeLoadBalancer,
				Container: manage.Container{
					Image: *imgLlama,
					Disk:  40,
					// RunPod's load balancer polls GET /ping and ignores
					// HEALTH_CHECK_PATH (measured in the spike). llama-server has no
					// /ping route, so serve one as a static file via --path.
					Entrypoint: []string{"/bin/sh", "-c"},
					Cmd: []string{
						"mkdir -p /tmp/pub && echo ok > /tmp/pub/ping && exec /app/llama-server" +
							" -hf " + *ggufModel + " --alias " + servedModel +
							" --host 0.0.0.0 --port 8080 --path /tmp/pub --jinja -ngl 99 -c 16384 -np 2",
					},
					Env:   map[string]string{"PORT": "8080", "PORT_HEALTH": "8080"},
					Ports: []string{"8080/http"},
				},
				GPU: gpu(), Workers: workers(),
				Scaling: manage.Scaling{Type: manage.ScalingRequestCount, RequestCount: 4},
				Timeout: 300000, Flashboot: *flashboot,
			},
		},
		"l": {
			note: "load balancer + our llama.cpp image (agent entrypoint)",
			req: manage.CreateEndpointRequest{
				Name: namePrefix + "l-lb-agent-llamacpp" + *suffix,
				Type: manage.TypeLoadBalancer,
				Container: manage.Container{
					Image: *imgAgentLlama,
					Disk:  40,
					Env:   agentEnv,
					Ports: []string{"8080/http"},
				},
				GPU:     gpu(),
				Workers: &manage.Workers{Min: intPtr(0), Max: intPtr(1), IdleTimeout: intPtr(*idle), InitTimeout: intPtr(900)},
				Scaling: manage.Scaling{Type: manage.ScalingRequestCount, RequestCount: 4},
				Timeout: 300000, Flashboot: *flashboot,
			},
		},
		"t": {
			note: "load balancer + our Strata image (agent entrypoint, prebuilt draft layer)",
			req: manage.CreateEndpointRequest{
				Name: namePrefix + "t-lb-agent-strata" + *suffix,
				Type: manage.TypeLoadBalancer,
				Container: manage.Container{
					Image: *imgAgentStrata,
					Disk:  *strataDisk,
					Env:   agentStrataEnv,
					Ports: []string{"8080/http"},
				},
				GPU:     &manage.GPUConfig{Pools: strings.Split(*strataPool, ","), ExcludedTypes: excluded, Count: 1, MinCudaVersion: "13.0"},
				Workers: &manage.Workers{Min: intPtr(0), Max: intPtr(1), IdleTimeout: intPtr(*idle), InitTimeout: intPtr(14400)},
				Scaling: manage.Scaling{Type: manage.ScalingRequestCount, RequestCount: 4},
				Timeout: 300000, Flashboot: *flashboot,
			},
		},
		"d": {
			note: "download benchmark worker (aria2c vs hf vs curl)",
			req: manage.CreateEndpointRequest{
				Name: namePrefix + "d-lb-dlbench" + *suffix,
				Type: manage.TypeLoadBalancer,
				Container: manage.Container{
					Image:      "python:3.12-slim",
					Disk:       60,
					Entrypoint: []string{"/bin/sh", "-c"},
					Cmd:        []string{`printf '%s' "$BENCH_PY" > /tmp/bench.py; exec python3 /tmp/bench.py`},
					Env:        benchEnv,
					Ports:      []string{"8080/http"},
				},
				GPU:     gpu(),
				Workers: &manage.Workers{Min: intPtr(0), Max: intPtr(1), IdleTimeout: intPtr(120), InitTimeout: intPtr(900)},
				Scaling: manage.Scaling{Type: manage.ScalingRequestCount, RequestCount: 4},
				Timeout: 300000, Flashboot: "OFF",
			},
		},
		"s": {
			note: "load balancer + Strata behind a /ping and auth shim, no network volume",
			req: manage.CreateEndpointRequest{
				Name: namePrefix + "s-lb-strata" + *suffix,
				Type: manage.TypeLoadBalancer,
				Container: manage.Container{
					Image: *imgStrata,
					Disk:  *strataDisk,
					// Start the shim on the public port, then run Strata's own
					// entrypoint on a private port. Strata opens its port only
					// once the model is downloaded, prepared and loaded.
					Entrypoint: []string{"/bin/sh", "-c"},
					Cmd: []string{
						`PY=/opt/strata/terminal/python/bin/python3; [ -e "$PY" ] || PY=python3; ` +
							`printf '%s' "$SHIM_PY" > /tmp/shim.py; "$PY" /tmp/shim.py & ` +
							`export PORT=8090; exec /opt/strata/entrypoint.sh run`,
					},
					Env: map[string]string{
						"PORT": "8080", "PORT_HEALTH": "8080",
						"SHIM_PORT": "8080", "SHIM_UPSTREAM_PORT": "8090", "SHIM_PY": strataShim,
						"FAMILY": *strataFamily, "MODEL": *strataModel, "API_KEY": strataKey,
					},
					Ports: []string{"8080/http"},
				},
				GPU: &manage.GPUConfig{Pools: strings.Split(*strataPool, ","), ExcludedTypes: excluded, Count: 1, MinCudaVersion: "13.0"},
				// One worker, a long idle timeout and the maximum init timeout: a
				// cold start downloads about 70 GB before the engine can answer.
				Workers: &manage.Workers{Min: intPtr(0), Max: intPtr(1), IdleTimeout: intPtr(300), InitTimeout: intPtr(14400)},
				Scaling: manage.Scaling{Type: manage.ScalingRequestCount, RequestCount: 4},
				Timeout: 300000, Flashboot: *flashboot,
			},
		},
	}

	st, _ := loadState()
	if st == nil {
		st = &state{Targets: map[string]target{}}
	}
	// Tagging is an account-level feature and may be off (403). Ownership then
	// rests on the name prefix and the local state file.
	tag, err := mc.EnsureTag(ctx, tagKey, tagValue)
	switch {
	case err == nil:
		st.TagID = tag.ID
	case isForbidden(err):
		tag = nil
		fmt.Printf("note: resource tagging is unavailable on this account (%v); tracking endpoints by the %q name prefix instead\n", err, namePrefix)
		rec.event("tagging_unavailable", fields{"err": err.Error()})
	default:
		return fmt.Errorf("ensure tag: %w", err)
	}

	for _, key := range strings.Split(strings.ToLower(*only), ",") {
		key = strings.TrimSpace(key)
		spec, ok := specs[key]
		if !ok {
			return fmt.Errorf("unknown endpoint %q (a, b, c, d, l, s or t)", key)
		}
		if existing, ok := st.Targets[key]; ok {
			if _, err := mc.GetEndpoint(ctx, existing.ID); err == nil {
				fmt.Printf("%s: already exists (%s), skipping\n", key, existing.ID)
				continue
			}
		}
		ep, err := mc.CreateEndpoint(ctx, spec.req)
		if err != nil {
			saveState(st)
			return fmt.Errorf("create %s: %w", key, err)
		}
		st.Targets[key] = target{Key: key, ID: ep.ID, Name: ep.Name, Type: ep.Type, URLs: ep.RequestURLs, Note: spec.note}
		// Save before tagging so a tagging failure never orphans an untracked endpoint.
		if err := saveState(st); err != nil {
			return err
		}
		if tag != nil {
			if err := mc.AttachTag(ctx, tag.ID, ep.ID); err != nil {
				return fmt.Errorf("created %s (%s) but tagging failed, `down` will still delete it from state: %w", key, ep.ID, err)
			}
		}
		rec.event("endpoint_created", fields{"target": key, "id": ep.ID, "type": ep.Type, "urls": ep.RequestURLs, "version": ep.Version})
		fmt.Printf("%s: created %s  %s  (%s)\n", key, ep.ID, ep.Name, spec.note)
	}
	fmt.Println("\nmin workers is 0, so nothing is billed until a request arrives. Run `probe down` when finished.")
	return nil
}

// ---- status ----

func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	fs.Parse(args)
	st, err := loadState()
	if err != nil {
		return err
	}
	mc, err := manageClient(nil)
	if err != nil {
		return err
	}
	ic, err := inferClient()
	if err != nil {
		return err
	}
	for _, t := range st.sorted() {
		fmt.Printf("%s  %s  %-14s %s\n   %s\n", t.Key, t.ID, t.Type, t.Name, t.Note)
		w, err := mc.ListWorkers(ctx, t.ID)
		if err != nil {
			fmt.Println("   workers: error:", err)
		} else {
			fmt.Printf("   workers: %s\n", workerSummary(w))
		}
		if t.Type == manage.TypeQueue {
			h := ic.Do(ctx, http.MethodGet, t.healthURL(), nil, nil, nil)
			fmt.Printf("   health:  %d %s\n", h.Status, strings.TrimSpace(firstN(h.BodyHead, 240)))
		}
	}
	return nil
}

func workerSummary(w *manage.WorkersResponse) string {
	if len(w.Workers) == 0 {
		return "none"
	}
	counts := map[string]int{}
	stale := 0
	for _, wk := range w.Workers {
		counts[wk.Status]++
		if wk.IsStale {
			stale++
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	if stale > 0 {
		parts = append(parts, fmt.Sprintf("stale=%d", stale))
	}
	return strings.Join(parts, " ")
}

func workerCounts(w *manage.WorkersResponse) fields {
	f := fields{"total": len(w.Workers)}
	stale := 0
	for _, wk := range w.Workers {
		key := strings.ToLower(wk.Status)
		n, _ := f[key].(int)
		f[key] = n + 1
		if wk.IsStale {
			stale++
		}
	}
	f["stale"] = stale
	return f
}

// ---- down ----

func cmdDown(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	dry := fs.Bool("n", false, "dry run: list what would be deleted")
	fs.Parse(args)
	rec, err := newRecorder("down", "", "")
	if err != nil {
		return err
	}
	defer rec.close()
	mc, err := manageClient(rec)
	if err != nil {
		return err
	}

	// Union of tagged endpoints, endpoints named with our prefix, and endpoints
	// recorded in local state, so nothing this tool created can be left behind.
	doomed := map[string]string{}
	tagged, err := mc.ListEndpoints(ctx, tagKey, tagValue)
	if err != nil && !isForbidden(err) {
		return fmt.Errorf("list tagged endpoints: %w", err)
	}
	for _, ep := range tagged {
		doomed[ep.ID] = ep.Name
	}
	all, err := mc.ListEndpoints(ctx, "", "")
	if err != nil {
		return fmt.Errorf("list endpoints: %w", err)
	}
	for _, ep := range all {
		if strings.HasPrefix(ep.Name, namePrefix) {
			doomed[ep.ID] = ep.Name
		}
	}
	st, _ := loadState()
	if st != nil {
		for _, t := range st.Targets {
			doomed[t.ID] = t.Name
		}
	}
	if len(doomed) == 0 {
		fmt.Println("nothing to delete")
		return nil
	}
	var failed int
	for id, name := range doomed {
		if *dry {
			fmt.Printf("would delete %s  %s\n", id, name)
			continue
		}
		err := mc.DeleteEndpoint(ctx, id)
		var apiErr *manage.APIError
		switch {
		case err == nil:
			fmt.Printf("deleted %s  %s\n", id, name)
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
			fmt.Printf("already gone %s  %s\n", id, name)
		default:
			failed++
			fmt.Printf("FAILED to delete %s  %s: %v\n", id, name, err)
		}
	}
	if *dry {
		return nil
	}
	if failed > 0 {
		return fmt.Errorf("%d endpoint(s) could not be deleted; check the RunPod console", failed)
	}
	if err := os.Remove(stateFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ---- patch + rollout ----

func cmdPatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("patch", flag.ExitOnError)
	tk := fs.String("target", "a", "test endpoint: a, b or c")
	maxWorkers := fs.Int("max-workers", -1, "new max workers")
	idle := fs.Int("idle-timeout", -1, "new idle timeout in seconds")
	image := fs.String("image", "", "new image reference (creates a release)")
	follow := fs.Duration("follow", 3*time.Minute, "how long to follow the rollout")
	fs.Parse(args)
	st, err := loadState()
	if err != nil {
		return err
	}
	t, err := st.get(*tk)
	if err != nil {
		return err
	}
	rec, err := newRecorder("patch", t.Key, "")
	if err != nil {
		return err
	}
	defer rec.close()
	mc, err := manageClient(rec)
	if err != nil {
		return err
	}

	var req manage.UpdateEndpointRequest
	if *maxWorkers >= 0 || *idle >= 0 {
		req.Workers = &manage.Workers{}
		if *maxWorkers >= 0 {
			req.Workers.Max = intPtr(*maxWorkers)
		}
		if *idle >= 0 {
			req.Workers.IdleTimeout = intPtr(*idle)
		}
	}
	req.Image = *image
	if req.Workers == nil && req.Image == "" {
		return errors.New("nothing to patch: pass -max-workers, -idle-timeout or -image")
	}

	before, err := mc.GetEndpoint(ctx, t.ID)
	if err != nil {
		return err
	}
	after, err := mc.UpdateEndpoint(ctx, t.ID, req)
	if err != nil {
		return err
	}
	rec.event("patched", fields{"request": req, "version_before": before.Version, "version_after": after.Version,
		"image_before": before.Image, "image_after": after.Image, "workers_after": after.Workers})
	fmt.Printf("patched %s: version %s -> %s, image %s\n", t.ID, verStr(before.Version), verStr(after.Version), after.Image)

	deadline := time.Now().Add(*follow)
	for time.Now().Before(deadline) {
		rel, err := mc.ListReleases(ctx, t.ID)
		if err != nil {
			return err
		}
		rec.event("rollout", fields{"in_progress": rel.Rollout.InProgress, "on_latest": rel.Rollout.WorkersOnLatest,
			"total": rel.Rollout.WorkersTotal, "percent": rel.Rollout.PercentOnLatest, "releases": len(rel.Releases)})
		fmt.Printf("rollout: in_progress=%v %d/%d workers on latest (%d%%), %d releases\n",
			rel.Rollout.InProgress, rel.Rollout.WorkersOnLatest, rel.Rollout.WorkersTotal, rel.Rollout.PercentOnLatest, len(rel.Releases))
		if !rel.Rollout.InProgress {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
	return nil
}

// isForbidden reports whether err is a 403 from the management API.
func isForbidden(err error) bool {
	var apiErr *manage.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden
}

func verStr(v *int) string {
	if v == nil {
		return "?"
	}
	return fmt.Sprint(*v)
}
