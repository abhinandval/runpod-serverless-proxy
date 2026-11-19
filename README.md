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

Starter engines: vLLM, llama.cpp and Strata, each from our own image.

## Status

**Phase 0: measurement spike.** Nothing here serves traffic yet. The repo holds
a RunPod client and a probe CLI that measure how the platform really behaves
(cold starts, timeouts, streaming, rollouts) before the gateway design is fixed.

- [spike/README.md](spike/README.md): how to run the spike
- [docs/spike-findings.md](docs/spike-findings.md): results and decisions
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
| `cmd/probe` | Spike CLI: creates test endpoints, runs probes, records JSONL |
| `internal/runpod/manage` | RunPod REST v2 management client (endpoints, tags, releases, catalog) |
| `internal/runpod/infer` | Request-side client (queue API, `/openai/v1`, load balancer) with timing |
| `spike/` | Spike instructions; results and local state are git-ignored |
| `docs/` | Findings and design notes |
