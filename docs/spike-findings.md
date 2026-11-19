# Spike findings

Status: **not yet run.** Everything under "Measured" is empty until the probes
in [spike/README.md](../spike/README.md) have been run against real endpoints.

## Known before measuring

From RunPod's documentation and the REST v2 OpenAPI spec (read 2026-11-19).
Treat these as claims to confirm, not facts.

- Management is REST v2 at `https://api.runpod.io/v2`; v1 is retired and GraphQL deprecated.
- An endpoint's `type` (`QUEUE` or `LOAD_BALANCER`) and its GPU/CPU family are immutable.
- Load balancers must scale on `REQUEST_COUNT` and are configured through the
  `PORT`, `PORT_HEALTH` and `HEALTH_CHECK_PATH` environment variables.
- GPU pools have no priority order; `PATCH gpu.pools` without `excludedTypes` clears exclusions.
- `PATCH env` replaces the whole map. An image change creates a release that rolls out gradually.
- Tags cannot be set at create time; they are attached with a second call.
- Model caching has no REST field.
- `/runsync` waits 90 s by default (`?wait=` up to 570 000 ms); `/stream` is polled, not SSE.
- Sync routes are reported to sit behind an edge timeout of about 100 s.
- Load balancer requests are capped at 5.5 minutes and fail when no worker is available.

## Measured

Environment: date, GPU pool, FlashBoot mode, image tags, model.

### Cold start timings

| Target | Path | Time to headers | Time to first token | Total | Outcome |
| --- | --- | --- | --- | --- | --- |
| a | `/openai/v1` stream | | | | |
| a | `/openai/v1` non-stream | | | | |
| a | `/run` + `/stream` | | | | |
| b | LB stream (with retries) | | | | |
| c | LB stream (with retries) | | | | |

### Warm timings

| Target | Path | Time to first token | Total | Notes |
| --- | --- | --- | --- | --- |
| a | `/openai/v1` stream | | | |
| a | `/run` + `/stream` | | | |
| b | LB stream | | | |
| c | LB stream | | | |

### Open questions

| Question | Answer | Evidence (results file) |
| --- | --- | --- |
| Does `/openai/v1` bypass the queue? | | |
| Edge timeout on cold requests: status, timing, streaming vs not | | |
| Cold load balancer: error status and body, time until ready | | |
| Latency added by `/stream` polling; chunk shape | | |
| `/runsync` behaviour when the wait expires | | |
| `/messages` event order and tool use per engine | | |
| Claude Code session works against which targets | | |
| Does a client disconnect stop generation and billing? | | |
| Rollout behaviour on image change | | |
| Management rate limits (`RateLimit-Policy`) | | |
| Does sampling a load balancer health URL wake a worker? | | |
| Anything that differed from the documented schema | | |

## Decisions

Filled in once the table above is complete.

- **Primary endpoint type:**
- **Cold-start strategy** (keep-alive plus queue wait, or keep-alive plus retries):
- **Model weights** (baked into image, downloaded at start, or network volume):
- **Changes to the data model or reconciler design:**
