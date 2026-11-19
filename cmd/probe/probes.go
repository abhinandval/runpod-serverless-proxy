package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"runpod-serverless-proxy/internal/runpod/infer"
	"runpod-serverless-proxy/internal/runpod/manage"
)

// ---- request payloads ----

const (
	plainPrompt = "Count from 1 to 40, one number per line, nothing else."
	toolPrompt  = "What is the weather in Paris right now? Use the get_weather tool."
)

var weatherParams = map[string]any{
	"type":       "object",
	"properties": map[string]any{"city": map[string]any{"type": "string"}},
	"required":   []string{"city"},
}

func openAIChatBody(stream, tools bool, maxTokens int) map[string]any {
	prompt := plainPrompt
	body := map[string]any{"model": servedModel, "max_tokens": maxTokens, "stream": stream}
	if tools {
		prompt = toolPrompt
		body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "get_weather", "description": "Get the current weather for a city", "parameters": weatherParams,
		}}}
	}
	body["messages"] = []any{map[string]any{"role": "user", "content": prompt}}
	if stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	return body
}

func anthropicBody(stream, tools bool, maxTokens int) map[string]any {
	prompt := plainPrompt
	body := map[string]any{"model": servedModel, "max_tokens": maxTokens, "stream": stream}
	if tools {
		prompt = toolPrompt
		body["tools"] = []any{map[string]any{
			"name": "get_weather", "description": "Get the current weather for a city", "input_schema": weatherParams,
		}}
	}
	body["messages"] = []any{map[string]any{"role": "user", "content": prompt}}
	return body
}

// chatRequest resolves URL, body and headers for one chat call against a target.
func chatRequest(t target, api string, stream, tools bool, maxTokens int) (string, map[string]any, map[string]string, error) {
	switch api {
	case "openai":
		return t.apiRoot() + "/chat/completions", openAIChatBody(stream, tools, maxTokens), nil, nil
	case "anthropic":
		return t.apiRoot() + "/messages", anthropicBody(stream, tools, maxTokens),
			map[string]string{"anthropic-version": "2023-06-01"}, nil
	}
	return "", nil, nil, fmt.Errorf("unknown -api %q (openai or anthropic)", api)
}

// ---- sampler: endpoint state once a second ----

// sample records queue /health (queue endpoints), the management API's worker
// list (every other tick, to stay well inside the rate limit) and, only when
// ping is set, the load balancer health URL. Pinging a load balancer may itself
// wake a worker, so it is opt-in.
func sample(ctx context.Context, rec *recorder, t target, ic *infer.Client, mc *manage.Client, ping bool) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for i := 0; ; i++ {
		if t.Type == manage.TypeQueue || ping {
			hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			h := ic.Do(hctx, http.MethodGet, t.healthURL(), nil, nil, nil)
			cancel()
			if ctx.Err() == nil {
				rec.event("health", fields{"status": h.Status, "ms": h.TotalMs, "body": strings.TrimSpace(firstN(h.BodyHead, 400)), "err": h.Err})
			}
		}
		if mc != nil && i%2 == 0 {
			if w, err := mc.ListWorkers(ctx, t.ID); err == nil {
				rec.event("workers", workerCounts(w))
			} else if ctx.Err() == nil {
				rec.event("workers", fields{"err": err.Error()})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// startSampler runs sample in the background and returns a stop function.
func startSampler(ctx context.Context, rec *recorder, t target, ic *infer.Client, ping bool) (func(), error) {
	mc, err := manageClient(nil) // untraced: sampler calls would drown the log
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sample(sctx, rec, t, ic, mc, ping)
	}()
	return func() { cancel(); <-done }, nil
}

type common struct {
	target *string
	label  *string
	watch  *bool
	ping   *bool
}

func commonFlags(fs *flag.FlagSet) common {
	return common{
		target: fs.String("target", "a", "test endpoint: a, b or c"),
		label:  fs.String("label", "", "free-form label for this run, e.g. cold or warm"),
		watch:  fs.Bool("watch", true, "sample health and workers during the run"),
		ping:   fs.Bool("ping", false, "also sample the load balancer health URL (may wake a worker)"),
	}
}

// setup resolves the target, opens a recorder and optionally starts the sampler.
func setup(ctx context.Context, probe string, c common) (target, *recorder, *infer.Client, func(), error) {
	st, err := loadState()
	if err != nil {
		return target{}, nil, nil, nil, err
	}
	t, err := st.get(*c.target)
	if err != nil {
		return target{}, nil, nil, nil, err
	}
	ic, err := inferClient()
	if err != nil {
		return target{}, nil, nil, nil, err
	}
	rec, err := newRecorder(probe, t.Key, *c.label)
	if err != nil {
		return target{}, nil, nil, nil, err
	}
	stop := func() {}
	if *c.watch {
		if stop, err = startSampler(ctx, rec, t, ic, *c.ping); err != nil {
			rec.close()
			return target{}, nil, nil, nil, err
		}
	}
	rec.event("start", fields{"endpoint_id": t.ID, "type": t.Type, "note": t.Note})
	cleanup := func() { stop(); rec.close() }
	return t, rec, ic, cleanup, nil
}

// ---- chat ----

func cmdChat(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	c := commonFlags(fs)
	api := fs.String("api", "openai", "openai (/chat/completions) or anthropic (/messages)")
	stream := fs.Bool("stream", true, "request a streaming response")
	tools := fs.Bool("tools", false, "send a tool definition and a prompt that should trigger it")
	maxTokens := fs.Int("max-tokens", 256, "max tokens to generate")
	timeout := fs.Duration("timeout", 15*time.Minute, "per-attempt timeout")
	retryFor := fs.Duration("retry-for", 0, "keep retrying non-2xx responses for this long (load balancer cold starts)")
	retryEvery := fs.Duration("retry-every", 5*time.Second, "delay between retries")
	show := fs.Bool("show", false, "print the response body")
	fs.Parse(args)

	t, rec, ic, cleanup, err := setup(ctx, "chat", c)
	if err != nil {
		return err
	}
	defer cleanup()
	url, body, headers, err := chatRequest(t, *api, *stream, *tools, *maxTokens)
	if err != nil {
		return err
	}
	rec.event("request", fields{"api": *api, "stream": *stream, "tools": *tools, "url": url})

	began := time.Now()
	for attempt := 1; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, *timeout)
		tm := ic.Do(actx, http.MethodPost, url, body, headers, nil)
		cancel()
		f := timingFields(tm)
		f["attempt"] = attempt
		f["since_start_ms"] = float64(time.Since(began).Microseconds()) / 1000
		rec.event("attempt", f)
		summarize(fmt.Sprintf("attempt %d:", attempt), tm)
		if len(tm.SSEEvents) > 0 {
			fmt.Println("  sse events:", strings.Join(tm.SSEEvents, " -> "))
		}
		if *show {
			fmt.Println(tm.BodyHead)
			if tm.BodyTail != "" {
				fmt.Println("...\n" + tm.BodyTail)
			}
		}
		if tm.OK() {
			rec.event("done", fields{"attempts": attempt, "ready_after_ms": float64(time.Since(began).Microseconds()) / 1000})
			return nil
		}
		if ctx.Err() != nil || time.Since(began) >= *retryFor {
			return errors.New("request did not succeed (see recorded attempts)")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(*retryEvery):
		}
	}
}

// ---- burst ----

func cmdBurst(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("burst", flag.ExitOnError)
	c := commonFlags(fs)
	n := fs.Int("n", 8, "number of parallel requests")
	api := fs.String("api", "openai", "openai or anthropic")
	maxTokens := fs.Int("max-tokens", 256, "max tokens to generate")
	timeout := fs.Duration("timeout", 15*time.Minute, "per-request timeout")
	fs.Parse(args)

	t, rec, ic, cleanup, err := setup(ctx, "burst", c)
	if err != nil {
		return err
	}
	defer cleanup()
	url, body, headers, err := chatRequest(t, *api, true, false, *maxTokens)
	if err != nil {
		return err
	}
	rec.event("request", fields{"api": *api, "n": *n, "url": url})

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 1; i <= *n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, *timeout)
			defer cancel()
			tm := ic.Do(rctx, http.MethodPost, url, body, headers, nil)
			f := timingFields(tm)
			f["request"] = i
			rec.event("result", f)
			mu.Lock()
			defer mu.Unlock()
			if tm.OK() {
				ok++
			}
			summarize(fmt.Sprintf("request %d:", i), tm)
		}(i)
	}
	wg.Wait()
	rec.event("done", fields{"ok": ok, "n": *n})
	fmt.Printf("%d/%d succeeded\n", ok, *n)
	return nil
}

// ---- disconnect ----

func cmdDisconnect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("disconnect", flag.ExitOnError)
	c := commonFlags(fs)
	api := fs.String("api", "openai", "openai or anthropic")
	after := fs.Duration("after", 2*time.Second, "drop the connection this long after the first token")
	observe := fs.Duration("observe", 60*time.Second, "keep sampling workers this long after the drop")
	fs.Parse(args)

	t, rec, ic, cleanup, err := setup(ctx, "disconnect", c)
	if err != nil {
		return err
	}
	defer cleanup()
	// A long generation, so the model is certainly still working when we hang up.
	url, body, headers, err := chatRequest(t, *api, true, false, 4096)
	if err != nil {
		return err
	}
	body["messages"] = []any{map[string]any{"role": "user", "content": "Write a very long story about a lighthouse keeper. Do not stop early."}}

	rctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	var once sync.Once
	tm := ic.Do(rctx, http.MethodPost, url, body, headers, func(_ time.Duration, line string) {
		if strings.HasPrefix(line, "data:") {
			once.Do(func() {
				rec.event("first_token", nil)
				time.AfterFunc(*after, func() {
					rec.event("client_disconnect", nil)
					cancel()
				})
			})
		}
	})
	rec.event("result", timingFields(tm))
	summarize("stream:", tm)
	if !tm.Canceled {
		fmt.Println("note: the stream ended before the client dropped it; raise -max-tokens or lower -after")
	}
	fmt.Printf("observing workers for %s (look for RUNNING -> IDLE in the recording)\n", *observe)
	select {
	case <-ctx.Done():
	case <-time.After(*observe):
	}
	return nil
}

// ---- queue ----

func cmdQueue(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("queue", flag.ExitOnError)
	c := commonFlags(fs)
	mode := fs.String("mode", "run-stream", "run-stream | status-sync | runsync")
	input := fs.String("input", "openai", "job input shape: openai (openai_route/openai_input) or native (worker-vllm messages)")
	wait := fs.Int("wait", 0, "wait in ms for runsync / status-sync (0 = platform default)")
	maxTokens := fs.Int("max-tokens", 256, "max tokens to generate")
	limit := fs.Duration("limit", 15*time.Minute, "give up after this long")
	fs.Parse(args)

	t, rec, ic, cleanup, err := setup(ctx, "queue-"+*mode, c)
	if err != nil {
		return err
	}
	defer cleanup()
	switch *mode {
	case "run-stream", "status-sync", "runsync":
	default:
		return fmt.Errorf("unknown -mode %q", *mode)
	}
	if t.Type != manage.TypeQueue {
		return fmt.Errorf("target %s is a %s endpoint; the queue probe needs a QUEUE endpoint (a)", t.Key, t.Type)
	}
	var jobInput map[string]any
	switch *input {
	case "openai":
		jobInput = map[string]any{"openai_route": "/v1/chat/completions", "openai_input": openAIChatBody(true, false, *maxTokens)}
	case "native":
		jobInput = map[string]any{
			"messages":        []any{map[string]any{"role": "user", "content": plainPrompt}},
			"sampling_params": map[string]any{"max_tokens": *maxTokens},
			"stream":          true,
		}
	default:
		return fmt.Errorf("unknown -input %q", *input)
	}
	rec.event("request", fields{"mode": *mode, "input": *input, "wait": *wait})
	ctx, cancel := context.WithTimeout(ctx, *limit)
	defer cancel()
	base := t.base()

	jobEvent := func(name string, job infer.Job, tm infer.Timing) {
		f := timingFields(tm)
		f["job_id"], f["job_status"] = job.ID, job.Status
		f["delay_time"], f["execution_time"] = job.DelayTime, job.ExecutionTime
		f["stream_items"] = job.StreamItems()
		rec.event(name, f)
	}

	if *mode == "runsync" {
		job, tm := ic.RunSync(ctx, base, jobInput, *wait)
		jobEvent("runsync", job, tm)
		fmt.Printf("runsync: http=%d job=%s status=%s after %.0fms (delayTime=%.0f executionTime=%.0f)\n",
			tm.Status, job.ID, job.Status, tm.TotalMs, job.DelayTime, job.ExecutionTime)
		// If the wait expired first, keep polling so the eventual outcome is recorded.
		for job.ID != "" && !job.Terminal() && ctx.Err() == nil {
			time.Sleep(2 * time.Second)
			job, tm = ic.Status(ctx, base, job.ID)
			jobEvent("status", job, tm)
			fmt.Printf("status: %s\n", job.Status)
		}
		return nil
	}

	job, tm := ic.Run(ctx, base, jobInput)
	jobEvent("run", job, tm)
	fmt.Printf("run: http=%d job=%s status=%s in %.0fms\n", tm.Status, job.ID, job.Status, tm.TotalMs)
	if job.ID == "" {
		return errors.New("no job id returned: " + strings.TrimSpace(firstN(tm.BodyHead, 300)))
	}
	id := job.ID
	began := time.Now()
	lastStatus, polls, items := "", 0, 0
	for ctx.Err() == nil {
		if *mode == "run-stream" {
			job, tm = ic.Stream(ctx, base, id)
		} else {
			job, tm = ic.StatusSync(ctx, base, id, *wait)
		}
		polls++
		items += job.StreamItems()
		jobEvent("poll", job, tm)
		if job.Status != lastStatus || job.StreamItems() > 0 {
			fmt.Printf("  +%6.0fms poll %d: http=%d status=%s items=%d (call took %.0fms)\n",
				float64(time.Since(began).Milliseconds()), polls, tm.Status, job.Status, job.StreamItems(), tm.TotalMs)
			lastStatus = job.Status
		}
		if job.Terminal() || !tm.OK() {
			break
		}
		// /stream returns immediately when there is nothing new; do not spin.
		if tm.TotalMs < 200 {
			time.Sleep(250 * time.Millisecond)
		}
	}
	final, ftm := ic.Status(ctx, base, id)
	jobEvent("final_status", final, ftm)
	fmt.Printf("final: status=%s delayTime=%.0fms executionTime=%.0fms polls=%d stream_items=%d wall=%.0fms\n",
		final.Status, final.DelayTime, final.ExecutionTime, polls, items, float64(time.Since(began).Milliseconds()))
	return nil
}

// ---- watch ----

func cmdWatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	c := commonFlags(fs)
	duration := fs.Duration("duration", 5*time.Minute, "how long to sample")
	fs.Parse(args)
	*c.watch = true
	_, _, _, cleanup, err := setup(ctx, "watch", c)
	if err != nil {
		return err
	}
	defer cleanup()
	fmt.Printf("sampling for %s (ctrl-c to stop)\n", *duration)
	select {
	case <-ctx.Done():
	case <-time.After(*duration):
	}
	return nil
}
