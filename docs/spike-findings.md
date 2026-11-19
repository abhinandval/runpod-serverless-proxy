# Spike findings

Status: **partly measured** on 2026-11-19. Queue and load-balancer behaviour for
vLLM is measured; llama.cpp is confirmed working on a load balancer; Strata, the
Claude Code session, `/runsync` expiry and the rollout probe are still open.

## Environment

- GPU pool `ADA_24` (RTX 4090, 24 GB), FlashBoot on, min workers 0, idle timeout 30 s.
- Target `a`: queue endpoint, `runpod/worker-v1-vllm:v2.31.0`, Qwen2.5 1.5B Instruct.
- Target `b`: load balancer, `vllm/vllm-openai:latest`, same model.
- Target `c`: load balancer, `ghcr.io/ggml-org/llama.cpp:server-cuda`,
  `unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL` (17.7 GB).
- Raw recordings are in `spike/results/` (git-ignored).

## Measured

### Cold start

| Target | Path | Time to headers | Time to first token | Outcome |
| --- | --- | --- | --- | --- |
| a | `/openai/v1` stream | 0.5 s | 175.6 s | 200, stream survived 175 s of silence |
| b | LB stream | 90.2 s | 90.2 s | 200 on the first attempt, no retry needed |
| b | LB non-stream (repeat) | 103.0 s | n/a | 200 |
| c | LB non-stream, 17.7 GB model | 87 s | n/a | 200 (model download about 43 s, load 5 s) |

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

### Answers to the open questions

| Question | Answer |
| --- | --- |
| Does `/openai/v1` bypass the queue? | **No.** Queue `/health` showed `inQueue: 1` for the whole cold wait. The worker README's "bypasses the queue" claim is wrong for this route. |
| Edge timeout on cold requests | **Not hit for streaming.** Headers arrive in 0.5 s and the stream survived 175 s with no bytes and no keep-alives. Not yet tested for non-streaming on the queue route. |
| Cold load balancer behaviour | **It waits, it does not fail fast.** The request is held with no response bytes at all until a worker is healthy. With no healthy worker it returns `400 {"detail":"timed out waiting for worker"}` after about **300 s** (docs say 2 minutes). |
| Latency added by `/stream` polling | Small when warm: 1.4 s end to end vs 1.3 s direct. Each poll takes 200 to 400 ms and returns a batch. |
| `/stream` chunk shape | Each item is `{"output": "<raw SSE text>"}`: several `data: {...}\n\n` lines concatenated. The `openai_route` job shape and the worker's native shape produce identical output. |
| `/status-sync` | Not useful: with `wait=30000` it held the call for the full 30 s although the job finished in under 1 s. |
| Anthropic `/messages` fidelity (vLLM) | Correct event order (`message_start` ... `message_delta` with stop reason and usage ... `message_stop`), proper `tool_use` blocks with streamed `input_json_delta`. Works on both the queue route and the load balancer. No `ping` events are sent. |
| Concurrency | 8 parallel requests on `a` all succeeded within 1.7 s; one worker ran 4 jobs at once. |
| Client disconnect | Work stops: the job left in-progress within about 3 s of the drop (full generation would have taken about 20 s). RunPod counts it as completed, not cancelled. |
| Health path on load balancers | **RunPod polls `GET /ping` and ignores `HEALTH_CHECK_PATH`**, even though the endpoint's `requestUrls.health` echoes the configured path. vLLM's access log shows only `/ping` polls. An engine without `/ping` never becomes healthy. |
| Unhealthy load-balancer workers | **They keep running and billing.** A worker that never passed its health check stayed `RUNNING` for over 10 minutes; the idle timeout did not apply. |
| Worker counts | The management API listed 5 to 7 worker records for endpoints with max workers 2, spread over several data centres, mostly `INITIALIZING`, `THROTTLED` or `IDLE` with no uptime. Whether any of these standby records are billed is unconfirmed. |
| Queue health counters | Coarse: the job stayed `inQueue` after a worker had picked it up and was loading the model. |
| Management rate limits | 180 per minute, 7 200 per hour, 86 400 per day (from `RateLimit-Policy`). |
| API key scopes | Management (REST API) and inference (`api.runpod.ai`) are separate permissions; a management-only key gets 403 on requests. Worker logs need serverless read on the REST API. |
| Resource tagging | Not enabled on this account (`403 Resource tagging is not enabled for this user`). |
| Load balancer response headers | Include `X-Runpod-Worker-Id`. All traffic passes through Cloudflare. |
| Model download speed | 17.7 GB from Hugging Face in about 43 s on the worker. |
| Schema surprises | None on create or patch. `args` is stored as `{"cmd":[...]}` JSON. A PATCH of entrypoint, cmd, disk and env produced endpoint version 2. |

### Still open

- Strata on serverless (needs a network volume, its own API key handling, and a `/ping` route).
- A Claude Code session against an endpoint.
- `/runsync` when the wait expires during a cold start; non-streaming cold request on the queue route.
- Rollout behaviour on an image change while workers are serving.
- Burst and disconnect on the load balancer.
- Whether standby worker records are billed (compare `/billing/serverless` with the timeline).

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
- **Model weights:** downloading at start is workable for models up to roughly
  20 GB (under a minute here). Larger models and engines that prepare data on
  first start (Strata) need a network volume. Still to be decided per engine.
