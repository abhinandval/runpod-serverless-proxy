# Phase 1 design

Status: **draft for review.** Based on the measurements in
[spike-findings.md](spike-findings.md). Nothing here is built yet.

## Goal

A gateway that OpenAI- and Anthropic-style clients (including Claude Code) can
point at, serving one model per serverless unit, where:

- a cold start looks to the client like a wait, never an error;
- units are deployed from a config file by the gateway itself;
- every request, unit event and cost is recorded for later display.

Out of scope for Phase 1: several units per model, failover, per-user keys and
quotas, a UI, vLLM, queue endpoints, any broker (Valkey), more than one gateway
instance.

## Decisions carried into this design

| Topic | Decision |
| --- | --- |
| Engines | llama.cpp and Strata |
| Endpoint type | RunPod load balancer |
| Images | our own, public on GitHub Container Registry |
| Model weights | downloaded at start by the agent, the same way for every engine; no network volume |
| Persistence | SQLite, written asynchronously; live state in memory |
| Timeouts | per unit, with per-engine baselines |
| Name | `runpod-serverless-proxy` for now |

## Components

```
client ──> gateway ──> RunPod load balancer ──> agent ──> engine
              │                                   (one container per worker)
              ├── SQLite (history, cost, fleet state)
              └── RunPod management API (deploy, workers, billing)
```

- **Gateway** (`cmd/gateway`): one Go binary. Serves the client API, tracks
  units and requests, runs the fleet reconciler, writes the records.
- **Agent** (`cmd/agent`): one small static Go binary copied into every engine
  image. The only thing the gateway talks to inside a unit.
- **Engine images** (`images/llamacpp`, `images/strata`): upstream image plus
  the agent as entrypoint.

## The unit contract (agent)

The agent listens on the public port and runs the engine on a private port.

| Route | Behaviour |
| --- | --- |
| `GET /ping` | 200 as soon as the agent is up. RunPod polls this; answering early lets the gateway reach the agent during a cold start. |
| `GET /agent/status` | JSON: `phase` (`starting`, `downloading`, `verifying`, `preparing`, `loading`, `ready`, `failed`), progress where known, engine model name, in-flight requests, last error, worker id. |
| anything else | If `ready`: proxied to the engine, streaming, with engine-specific auth injected. Otherwise `503` with the status body, so a request that races a cold start fails fast and identifiably. |

Agent duties:

- download the model itself (see "Model download") and place the files where
  the engine expects them, then start the engine pointing at them;
- derive the later phases from the engine (log lines for Strata, `/health` for
  llama.cpp);
- exit non-zero if the engine exits or exceeds the unit's cold-start timeout, so a
  broken unit stops billing instead of running forever;
- never see the client's credentials: RunPod authenticates the caller, the agent
  holds the engine's own key if it needs one.

### Model download

The agent downloads model files from Hugging Face before starting the engine,
instead of each engine using its own downloader.

- One strategy and one progress format for every engine: `downloading` is its
  own phase with exact bytes done and total, timed and recorded separately from
  engine loading.
- An optional Hugging Face token (`fleet.hf_token`, passed to the unit as a
  RunPod secret, never as a plain environment variable) allows gated and
  private repositories and higher rate limits.
- Files land in a fixed layout on the container disk (`/models/<repo>/<file>`),
  and the engine is started with a local path: `-m` for llama.cpp, `MODEL_PATH`
  for Strata.
- The source is a setting, so a mirror or our own bucket can replace
  Hugging Face later without touching the engines.

Strata caveat: in the spike the main model download was only about 2 of its 10
to 12 minutes. The rest was Strata verifying the files (about 2.5 min),
downloading its 5 GB draft layer (4.5 to 6 min) and loading. `MODEL_PATH` covers
the main files only, so the agent must also place the draft layer where Strata
looks for it, or most of the wait remains. That needs a look at Strata's data
layout and is the main risk in this change.

**Open: which downloader.** Either the agent downloads natively in Go (parallel
ranged requests, as Strata's own downloader does), or it shells out to
Hugging Face's `hf` CLI. To be decided by measurement on a worker.

**To verify first, on a real endpoint:** RunPod routes as soon as `/ping`
returns 200, and status polls keep a worker alive through a long download.
If either fails, fall back to `/ping` = 204 until ready (the spike's behaviour)
with the gateway retrying RunPod's 300 s `timed out waiting for worker`.

## Config

```yaml
gateway:
  listen: ":8080"
  api_keys: ["${env.GATEWAY_KEY}"]        # static keys in Phase 1
  database: "/data/gateway.db"

fleet:
  provider: runpod
  management_key: ${env.RUNPOD_MANAGEMENT_KEY}
  inference_key:  ${env.RUNPOD_INFERENCE_KEY}
  hf_token: ${env.HF_TOKEN}                # optional; gated or private models
  name_prefix: "rsp-"                      # ownership: prefix + stored ids (tags are optional)

units:
  qwen27-a5000:
    engine: llama.cpp                      # llama.cpp | strata
    engine_version: "0.1.0"                # tag of our image for that engine
    model:
      source: huggingface
      repo: unsloth/Qwen3.8-27B-GGUF
      files: ["Qwen3.8-27B-UD-Q4_K_XL.gguf"]
    engine_args: {ctx_size: 16384, parallel: 2}
    spec:
      gpu: {pools: [AMPERE_24], exclude_types: ["NVIDIA L4"], count: 1}
      disk_gb: 40
      flashboot: on
    scaling: {min_workers: 0, max_workers: 1, concurrency_per_worker: 2}
    timeouts: {cold_start: 5m, idle: 60s, request: 5m}   # optional; engine baselines apply

  strata-a5000:
    engine: strata
    engine_version: "0.1.0"
    model: {family: qwen, size: IQ2_XS}
    spec:
      gpu: {pools: [AMPERE_24], exclude_types: ["NVIDIA L4"], count: 1, min_cuda: "13.0"}
      disk_gb: 200
      flashboot: off
    scaling: {min_workers: 0, max_workers: 1, concurrency_per_worker: 4}

models:
  qwen-27b:
    unit: qwen27-a5000
    aliases: [gpt-4o-mini]
  qwen-flash:
    unit: strata-a5000
    aliases: [claude-sonnet, claude-haiku]
```

Timeout baselines when a unit sets none:

| Setting | Meaning | llama.cpp | Strata |
| --- | --- | --- | --- |
| `cold_start` | longest a request waits for the unit to become ready | 5 min | 20 min |
| `idle` | how long a worker stays up without traffic | 60 s | 15 min |
| `request` | longest one request may run once the unit is ready | 5 min | 5 min |

`request` is bounded by RunPod's documented 5.5 minute cap per load-balancer
request, which is unmeasured; test a long streaming generation early.

## Request lifecycle

Every request gets a gateway id (`X-Request-Id` on the response) and moves
through one state machine, independent of endpoint type:

```
received -> waking -> waiting_for_unit -> streaming -> completed
                                      \-> failed | cancelled | timed_out
```

1. Authenticate with a gateway key; resolve `model` (or alias) to a unit.
2. If the unit is `ready`, forward at once.
3. Otherwise mark it `waking` and poll `/agent/status` through the load
   balancer (the first poll wakes a worker). Requests for the same unit share
   one wake-up.
4. While waiting:
   - **streaming request:** reply `200` immediately and send a keep-alive every
     10 s (`: keep-alive` for OpenAI, a `ping` event for Anthropic);
   - **non-streaming request:** hold the connection silently.
5. When the agent reports `ready`, forward the request with `model` rewritten to
   the engine's name, and relay the response unbuffered.
6. If `cold_start` expires or the agent reports `failed`:
   - non-streaming: `503` with `Retry-After` and the request id;
   - streaming (200 already sent): an error event in the stream's own format.
7. A client disconnect cancels the upstream request.

Retries happen only before the first upstream byte reaches the client.

## Live state and records

- **Live state is in memory:** units (state, phase, workers, in-flight count)
  and in-flight requests. The request path reads and writes only this.
- **SQLite is the durable record**, written off the request path: events go
  into a buffered channel and a single writer flushes them in batches (about
  once a second, and on shutdown). A request never waits on the database.
- **Exception:** fleet state (endpoint ids) is written synchronously when an
  endpoint is created or deleted. It is rare, not on the request path, and
  losing it would orphan a billable endpoint.
- On start the gateway rebuilds unit state from the RunPod API and the agents,
  so a crash loses at most the last second of history, never fleet state.

Tables:

| Table | Holds |
| --- | --- |
| `requests` | id, key label, model, unit, worker id, format, streaming, lifecycle timestamps, input/output tokens, status, error |
| `unit_events` | unit, worker id, from/to state or phase, timestamp (gives cold-start history) |
| `cost_samples` | unit, period, worker-seconds, price per hour, estimated cost, billed cost once RunPod reports it |
| `fleet` | unit, endpoint id, applied spec hash, image, version, timestamps |

Prompt and response bodies are **not** stored; only metadata. Body capture is a
later, explicit opt-in.

Token counts come from the response's usage data. For OpenAI-format streaming
the gateway adds `stream_options.include_usage` and strips the extra chunk if
the client did not ask for it.

## Fleet reconciler

- `gateway fleet plan` shows the diff between config and RunPod; `apply` makes it so.
- Creates endpoints with inline container config; names are `name_prefix + unit`.
- Mutable changes use `PATCH` (always the full `env` and full `gpu` block).
  Immutable changes (endpoint type, GPU/CPU family) are reported and refused in
  Phase 1.
- Never touches endpoints it does not own; removing a unit from config needs
  `apply --prune`.
- Handles the known rejections with clear messages: FlashBoot quota, reserved
  names, unknown pools.

## Build order

1. **Agent + images:** agent binary, llama.cpp and Strata images, CI publishing
   to the registry, and the early-`/ping` verification on a real endpoint.
2. **Gateway core:** config, auth, routing, lifecycle, keep-alive bridge,
   async SQLite writer, `/v1/models`; tested against a scripted fake unit, then
   live with the OpenAI and Anthropic SDKs and Claude Code.
3. **Reconciler:** `plan` and `apply`, replacing the spike's `up` and `down`.

Small interfaces sit around the request queue, the unit-state store and the
record writer, so a shared store (Valkey) can be added when a second gateway
instance or asynchronous jobs are needed.

## Open items

- Which downloader the agent uses, and how to pre-place Strata's draft layer.
- GitHub owner and repository name (image names, Go module path).
- Whether the early-`/ping` design holds on RunPod (first task of step 1).
- The 5.5 minute request cap on long generations.
- Account balance: the API has a balance schema that has not been called yet.
