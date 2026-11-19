# runpod-serverless-proxy

A team LLM gateway for RunPod Serverless, in the spirit of
[llama-swap](https://github.com/mostlygeek/llama-swap): one OpenAI- and
Anthropic-compatible API in front of many models and engines, where the units
being swapped are serverless endpoints instead of local processes.

The gateway is meant to:

- absorb cold starts, delays and failures so ordinary SDKs and tools keep working;
- route by model name across replicas and fallbacks;
- deploy and update the fleet from a declarative config, given only RunPod keys;
- give a team scoped keys, usage accounting and rate limits.

Starter engines: llama.cpp and Strata, each from our own image.

## Status

**Phase 1 in progress.** The measurement spike is done and the design is fixed.
The unit agent, the llama.cpp and Strata engine images and the gateway core
exist. Request records (SQLite) and the fleet reconciler are not built yet, so
units are still deployed by hand.

- [spike/README.md](spike/README.md): how to run the spike
- [docs/design.md](docs/design.md): Phase 1 design
- [docs/spike-findings.md](docs/spike-findings.md): what was measured, and the decisions that followed
- [CONTRIBUTING.md](CONTRIBUTING.md): setup, tasks, commit rules

## Quick start

```bash
mise trust && mise install
```

```bash
mise run test
```

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/gateway` | The gateway: client API, model routing, cold-start bridge |
| `internal/gateway` | Request lifecycle, shared wake-up, keep-alives, response relay |
| `internal/config` | YAML configuration with `${env.NAME}` substitution |
| `cmd/agent` | Entrypoint of every engine image: downloads the model, starts the engine, reports status, proxies requests |
| `internal/agent` | The agent's lifecycle, downloader and engine drivers |
| `images/` | Engine images (llama.cpp, Strata) |
| `cmd/probe` | Spike CLI: creates test endpoints, runs probes, records JSONL |
| `internal/runpod/manage` | RunPod REST v2 management client (endpoints, tags, releases, catalog) |
| `internal/runpod/infer` | Request-side client (queue API, `/openai/v1`, load balancer) with timing |
| `spike/` | Spike instructions; results and local state are git-ignored |
| `docs/` | Findings and design notes |
