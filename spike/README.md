# Phase 0 spike: how RunPod Serverless actually behaves

The gateway design depends on a few things the documentation leaves unclear or
contradicts. This spike measures them on real endpoints. Results go into
[docs/spike-findings.md](../docs/spike-findings.md).

## Cost and safety

- Endpoints are created with **min workers 0**, so nothing is billed while idle.
  Workers bill per second from start (including model download and load) until
  they scale down after the idle timeout (30 s by default).
- The model is small (Qwen2.5 1.5B Instruct), so any 16 GB+ pool is enough.
- `probe down` deletes only endpoints tagged `managed-by=rsp-spike` or listed in
  `spike/state.json`. Run it when you finish, and confirm in the RunPod console.

## Setup

1. Create two scoped API keys in the RunPod console: one with serverless write
   access (management) and a narrower one for requests (inference).
2. Put them in `.env` (copy `.env.example`). They never go into git or chat.
3. Pick a GPU pool:

```bash
mise run probe -- catalog
```

4. Create the three test endpoints (`-pool` takes the POOL column value):

```bash
mise run probe -- up -pool <POOL>
```

| Target | Type | Image | Purpose |
| --- | --- | --- | --- |
| `a` | queue | official `worker-vllm` | `/run`, `/runsync`, `/openai/v1` routes |
| `b` | load balancer | stock `vllm/vllm-openai` | direct HTTP, same engine as `a` |
| `c` | load balancer | stock llama.cpp server | checks that any engine works unmodified |

```bash
mise run probe -- status
```

## Probes

Every probe writes a JSONL file to `spike/results/` and, by default, samples
worker state once a second alongside the request. Label runs `-label cold` or
`-label warm`. A run is **cold** when `status` shows no workers; wait about a
minute after the previous request for workers to scale down. It is **warm** when
repeated straight after a successful run.

Run 1 to 6 against each relevant target, cold then warm.

**1. OpenAI chat, streaming and not** (targets a, b, c)

```bash
mise run probe -- chat -target a -label cold
```

```bash
mise run probe -- chat -target a -label warm -stream=false
```

For load balancers a cold request is expected to fail, so retry until ready:

```bash
mise run probe -- chat -target b -label cold -retry-for 15m
```

**2. Anthropic messages, with and without tools** (targets a, b, c)

```bash
mise run probe -- chat -target a -api anthropic -label warm -show
```

```bash
mise run probe -- chat -target a -api anthropic -tools -label warm -show
```

**3. Raw queue API** (target a)

```bash
mise run probe -- queue -target a -mode run-stream -label cold
```

```bash
mise run probe -- queue -target a -mode status-sync -wait 30000 -label warm
```

Repeat `run-stream` with `-input native` to compare the worker's native input
shape against the `openai_route` job shape.

**4. runsync expiring during a cold start** (target a)

```bash
mise run probe -- queue -target a -mode runsync -wait 20000 -label cold
```

**5. Parallel requests** (targets a, b)

```bash
mise run probe -- burst -target a -n 8 -label cold
```

**6. Client disconnect mid-stream** (targets a, b)

```bash
mise run probe -- disconnect -target a -label warm
```

**7. Management cycle** (target a)

```bash
mise run probe -- patch -target a -max-workers 3
```

```bash
mise run probe -- patch -target a -image runpod/worker-v1-vllm:<another-tag>
```

**8. Claude Code against an endpoint** (manual, warm target)

Point Claude Code at whichever target passed probe 2 and try a task that needs
tool use. For a queue endpoint the base URL is
`https://api.runpod.ai/v2/<endpoint-id>/openai`; for a load balancer it is
`https://<endpoint-id>.api.runpod.ai`. Set `ANTHROPIC_BASE_URL` to it,
`ANTHROPIC_AUTH_TOKEN` to the inference key and `ANTHROPIC_MODEL` to `spike-model`.

## What to look for

| Question | Where the answer shows up |
| --- | --- |
| Does `/openai/v1` bypass the queue? | Probe 5 on `a`: `health` samples show jobs queued or not while requests wait |
| Is there an edge timeout on cold requests, and when? | Probe 1 cold on `a`: status 524 or a cut stream, and `total_ms` at the cut |
| Does a stream survive a long cold start once headers are sent? | Probe 1 cold streaming: `headers_ms` vs `first_data_ms`, `max_gap_ms` |
| What does a cold load balancer return, and for how long? | Probe 1 cold on `b`/`c`: status and body of each attempt, `ready_after_ms` |
| How much latency does `/stream` polling add? | Probe 3: per-poll timings and item counts vs probe 1 warm |
| Is `/messages` usable for Claude Code? | Probe 2: `sse events` order and tool-use blocks; probe 8 |
| Does a dropped client stop the work? | Probe 6: `workers` samples after `client_disconnect` |
| How do rollouts behave? | Probe 7: `rollout` events, version before and after |
| What are the management rate limits? | `manage_call` events: `ratelimit_policy` |

## Teardown

```bash
mise run probe -- down
```

`down -n` lists what would be deleted without deleting anything.
