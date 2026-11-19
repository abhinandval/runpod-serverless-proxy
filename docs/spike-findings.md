# Spike findings

Status: **measured** on 2026-11-19. Queue and load-balancer behaviour is measured
for vLLM; llama.cpp and Strata are confirmed working on a load balancer; a
Claude Code session ran against Strata. Two minor items remain open (below).

## Environment

- GPU pool `ADA_24` (RTX 4090, 24 GB), FlashBoot on, min workers 0, idle timeout 30 s.
- Target `a`: queue endpoint, `runpod/worker-v1-vllm:v2.31.0`, Qwen2.5 1.5B Instruct.
- Target `b`: load balancer, `vllm/vllm-openai:latest`, same model.
- Target `c`: load balancer, `ghcr.io/ggml-org/llama.cpp:server-cuda`,
  `unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL` (17.7 GB).
- Target `s`: load balancer, `ghcr.io/niko1221/strata:0.2.7-cuda13`, `FAMILY=qwen`
  `MODEL=IQ2_XS`, pool `AMPERE_48` (A40, 48 GB VRAM, 46 GB container RAM limit),
  200 GB container disk, no network volume, FlashBoot off, behind the shim in
  `cmd/probe/strata_shim.py`.
- Raw recordings are in `spike/results/` (git-ignored).

## Measured

### Cold start

| Target | Path | Time to headers | Time to first token | Outcome |
| --- | --- | --- | --- | --- |
| a | `/openai/v1` stream | 0.5 s | 175.6 s | 200, stream survived 175 s of silence |
| b | LB stream | 90.2 s | 90.2 s | 200 on the first attempt, no retry needed |
| b | LB non-stream (repeat) | 103.0 s | n/a | 200 |
| c | LB non-stream, 17.7 GB model | 87 s | n/a | 200 (model download about 43 s, load 5 s) |

| a | `/openai/v1` non-stream (later run) | 81.5 s | n/a | 200 |
| a | `/openai/v1` non-stream, first request after an image change | 111.1 s | n/a | 200, no edge timeout |
| b | LB, 8 parallel streams | about 80 s each | about 80 s | all 200, served by one worker |
| s | LB non-stream, Strata IQ2_XS | 734 s in total | n/a | two `400 timed out waiting for worker` (300 s each), then 200 on the third attempt |

Timeline of the Strata cold start (container start to ready: **11 min 46 s**):

| Phase | Duration |
| --- | --- |
| Download model files (39.2 GB of the 68.9 GB plan, 16 ranged connections) | about 2 min 10 s |
| Verify the files (SHA-256) | about 2 min 35 s |
| Prepare the model and download the 5 GB draft layer | about 6 min 20 s |
| Start the engine and load the model | 34 s |

Timeline of the cold queue request (`a`): workers allocated and idle by about
30 s, job picked up at about 50 s, model download and load until 175 s.

### Warm

| Target | Path | Time to first token | Total |
| --- | --- | --- | --- |
| a | `/openai/v1` stream | 0.7 s | 1.3 s |
| a | `/openai/v1` non-stream | n/a | 1.1 s |
| a | `/openai/v1/messages` stream | 0.6 s | 1.3 s |
| a | `/openai/v1/messages` stream with tool | 0.5 s | 0.7 s |
| a | `/run` + `/stream` polling | first poll 0.2 s after submit | 1.4 s |
| b | LB stream | 0.5 s | 1.0 s |
| b | LB `/v1/messages` stream with tool | 0.5 s | 0.6 s |
| s | LB stream (Strata, through the shim) | 0.4 s | 1.3 s |
| s | LB `/v1/messages` stream | 0.3 s | 1.2 s |
| s | LB `/v1/messages` stream with tool | 0.4 s | 1.3 s |
| s | 4 parallel streams | 0.8 s | 5.2 to 5.7 s each |

### Answers to the open questions

| Question | Answer |
| --- | --- |
| Does `/openai/v1` bypass the queue? | **No.** Queue `/health` showed `inQueue: 1` for the whole cold wait. The worker README's "bypasses the queue" claim is wrong for this route. |
| Edge timeout on cold requests | **Not observed.** A streaming request got headers in 0.5 s and survived 175 s with no bytes and no keep-alives. A non-streaming request on the queue route waited 111 s with no response at all and still returned 200, so the reported 100 s cutoff did not apply. The load balancer's own limit is 300 s. |
| Cold load balancer behaviour | **It waits, it does not fail fast.** The request is held with no response bytes at all until a worker is healthy. With no healthy worker it returns `400 {"detail":"timed out waiting for worker"}` after about **300 s** (docs say 2 minutes). |
| Latency added by `/stream` polling | Small when warm: 1.4 s end to end vs 1.3 s direct. Each poll takes 200 to 400 ms and returns a batch. |
| `/stream` chunk shape | Each item is `{"output": "<raw SSE text>"}`: several `data: {...}\n\n` lines concatenated. The `openai_route` job shape and the worker's native shape produce identical output. |
| `/status-sync` | Not useful: with `wait=30000` it held the call for the full 30 s although the job finished in under 1 s. |
| Anthropic `/messages` fidelity (vLLM) | Correct event order (`message_start` ... `message_delta` with stop reason and usage ... `message_stop`), proper `tool_use` blocks with streamed `input_json_delta`. Works on both the queue route and the load balancer. No `ping` events are sent. |
| FlashBoot | After the worker had scaled down for 150 s, a `/runsync` request completed in 2.9 s with `delayTime` 1.6 s: FlashBoot restored a worker in under two seconds. First starts and starts after an image change are still full cold starts. |
| Rollout on image change | `PATCH image` moved the endpoint from version 1 to 2 at once. Standby workers were replaced progressively with no traffic: 0, 29, 43 then 57 percent on the latest version within 100 s. The next request waited 111 s for a worker on the new image. |
| Claude Code | A headless session against the Strata endpoint (base URL = the load balancer URL, auth token = the RunPod inference key) completed a task using the Bash and Read tools in 3 turns and 3.7 s. Claude Code prints an `unrecognized_model` notice for the custom model name; it is harmless. |
| Billing of standby workers | Day totals from `/billing/serverless` match the time workers were actually running; the extra `IDLE`, `INITIALIZING` and `THROTTLED` worker records do not appear to be billed. Billing data lags, so re-check the next day. |
| Docker HEALTHCHECK | RunPod runs the image's own `HEALTHCHECK` (seen as periodic `/v1/models` calls inside the Strata container), separately from the load balancer's `/ping` polling. |
| Concurrency | 8 parallel requests on `a` all succeeded within 1.7 s; one worker ran 4 jobs at once. |
| Client disconnect | On the queue endpoint, work stops: the job left in-progress within about 3 s of the drop (full generation would have taken about 20 s). RunPod counts it as completed, not cancelled. On a load balancer the worker state gives no signal either way (the worker just idled out after its timeout), so this is unconfirmed there. |
| Health path on load balancers | **RunPod polls `GET /ping` and ignores `HEALTH_CHECK_PATH`**, even though the endpoint's `requestUrls.health` echoes the configured path. vLLM's access log shows only `/ping` polls. An engine without `/ping` never becomes healthy. |
| Unhealthy load-balancer workers | **They keep running and billing.** A worker that never passed its health check stayed `RUNNING` for over 10 minutes; the idle timeout did not apply. |
| Worker counts | The management API listed 5 to 7 worker records for endpoints with max workers 2, spread over several data centres, mostly `INITIALIZING`, `THROTTLED` or `IDLE` with no uptime. Whether any of these standby records are billed is unconfirmed. |
| Queue health counters | Coarse: the job stayed `inQueue` after a worker had picked it up and was loading the model. |
| Management rate limits | 180 per minute, 7 200 per hour, 86 400 per day (from `RateLimit-Policy`). |
| API key scopes | Management (REST API) and inference (`api.runpod.ai`) are separate permissions; a management-only key gets 403 on requests. Worker logs need serverless read on the REST API. |
| Resource tagging | Not enabled on this account (`403 Resource tagging is not enabled for this user`). |
| Load balancer response headers | Include `X-Runpod-Worker-Id`. All traffic passes through Cloudflare. |
| Model download speed | 17.7 GB from Hugging Face in about 43 s on the worker. |
| Strata on serverless | **Works with the stock image plus a shim.** The shim answers `/ping` (204 while loading, 200 when `/v1/models` answers) and injects Strata's own API key, so callers only need the RunPod key. Strata ran without the Docker memlock and shm options. It ignores the request's `model` field and reports `qwen3.8-flash-next-iq2_xs`. About 157 tokens/s on an A40. |
| Strata Anthropic fidelity | Correct event order with `thinking` blocks before `tool_use`, streamed `input_json_delta`, `: keep-alive` SSE comments, and cache usage fields in `message_delta`. |
| Strata downloader | Its own stdlib downloader (not `huggingface_hub`): pinned Hugging Face `resolve` URLs, 16 ranged connections for files over 256 MB, resumable, SHA-256 verified. `HF_ENDPOINT` selects a mirror. |
| FlashBoot limit | This plan allows FlashBoot on 3 endpoints; a fourth create fails with 400 until FlashBoot is off. |
| Container memory | The A40 worker has a 46 GB memory limit; Strata warns about it but IQ2_XS loaded. Host RAM per pool is in the catalog (`minRam`): 16 GB pools have about 31 GB, so they cannot run Strata's standard sizes. |
| Schema surprises | None on create or patch. `args` is stored as `{"cmd":[...]}` JSON. A PATCH of entrypoint, cmd, disk and env produced endpoint version 2. |

### Still open

- `/runsync` when the wait expires before the job finishes: the test job completed
  inside the minimum 1 s wait, so the expiry path was not exercised.
- Whether a client disconnect stops generation on a load balancer (check engine logs).
- Scale-out to a second worker under sustained load.

### Cheaper GPUs (pool `AMPERE_24`, $0.75 per GPU-hour)

Both engines were redeployed on `AMPERE_24` with the L4 and the MIG slice excluded,
which leaves the RTX A5000 and RTX 3090. Both endpoints were placed on an RTX A5000
(24 GB VRAM, about 50 GB host RAM) within seconds, despite the catalog rating
availability as low.

| Engine and model | GPU | $/hour | Cold start | Speed |
| --- | --- | --- | --- | --- |
| llama.cpp, Qwen3.8-27B UD-Q4_K_XL | RTX 4090 | 1.34 | 87 s | not captured |
| llama.cpp, Qwen3.8-27B UD-Q4_K_XL | RTX A5000 | 0.75 | 77 s | 33.6 tokens/s |
| Strata, IQ2_XS | A40 (48 GB) | 1.22 | 11 min 46 s | 156.8 tokens/s |
| Strata, IQ2_XS | RTX A5000 | 0.75 | about 10 min | 88.8 tokens/s |

- Strata IQ2_XS loads and serves on a 24 GB card: it keeps part of the model in
  host RAM, at a little over half the A40's speed.
- Per dollar the two are close for a single stream (about 118 tokens/s per $/hour
  on the A5000, 129 on the A40); the A5000 is cheaper per cold start (about $0.13
  against $0.24) and the A40 is faster for the user.
- No RTX 3090 was allocated, so the 3090 (about 30 GB host RAM) is untested; with
  less RAM it is the riskier card for Strata.
- A pool cannot be restricted to named cards directly; it is narrowed with
  `gpu.excludedTypes`, so a card added to the pool later becomes eligible.
- Endpoint names must be unique, and a deleted endpoint's name stays reserved for
  a while (`400 endpoint template names must be unique`).

### Download tools (A/B on one worker)

One RTX A5000 worker (96 host cores, container disk) downloaded the same 17.7 GB
file (`unsloth/Qwen3.8-27B-GGUF`, UD-Q4_K_XL) with each tool, in the order
shown, without a Hugging Face token. `cmd/probe/dlbench.py` is the harness.

| Tool | Run 1 | Run 2 |
| --- | --- | --- |
| `aria2c` 1.37, 16 connections | 32.4 s (545 MB/s) | 40.1 s (441 MB/s) |
| `hf download` 2.4.0 (Xet) | 36.4 s (485 MB/s) | 17.0 s (1,041 MB/s) |
| `hf download` with `HF_XET_HIGH_PERFORMANCE=1` | 15.0 s (1,179 MB/s) | 16.0 s (1,107 MB/s) |
| `curl`, one stream | 72.2 s (245 MB/s) | not run |

- The Hugging Face CLI in high-performance mode is the fastest and the most
  consistent, a little over twice as fast as `aria2c`.
- Plain `hf download` varied by a factor of two between runs; two samples are
  not enough to say why.
- Not yet run with a token, and not yet tried on a worker with fewer cores.

## Decisions

- **Primary endpoint type: load balancer.** It needs no handler code, works with
  any engine's own HTTP server, passes OpenAI and Anthropic requests through
  unchanged, and cold-started in about half the time of the queue endpoint here.
  The queue adapter stays as a second type; it is the only one that returns
  headers immediately and holds requests longer than 5 minutes.
- **Every engine image must serve `GET /ping`** on its port (200 when ready,
  204 while loading). This is the main reason to build our own images.
- **Cold-start strategy:** the gateway answers the client immediately and sends
  keep-alives itself, since the load balancer sends nothing while waiting. It
  retries a `400 timed out waiting for worker`, because cold starts with large
  models can exceed 300 s.
- **Unit health watchdog:** the gateway must detect a unit that is running but
  never healthy and scale it to zero, or it bills indefinitely.
- **Ownership without tags:** the reconciler identifies its endpoints by name
  prefix plus stored ids; tags are optional.
- **Startup key check:** verify that the inference key can reach each endpoint
  and the management key can read logs, and report scope problems clearly.
- **Model weights: download at start, no network volume.** Decided by the
  project owner; the cold-start delay is accepted. Measured cost: under a minute
  for a 17.7 GB GGUF, about 12 minutes for Strata IQ2_XS, most of which is
  verification and preparation rather than download.
- **Cold starts longer than 300 s are normal for large engines**, so the
  gateway's retry of `400 timed out waiting for worker` is required, not an
  edge case. Each retry must keep the client connection alive.
- **Every engine image carries a small shim** (as prototyped for Strata) that
  serves `/ping` from real readiness and handles engine-specific auth, so the
  gateway talks to all engines the same way.
- **FlashBoot is a per-plan quota**, so the fleet config needs it per unit and
  the reconciler must report the limit clearly.
