# syntax=docker/dockerfile:1
# Strata engine image: the upstream image with our agent as entrypoint and the
# draft layer prepared at build time.
# Build from the repository root:
#   docker build -f images/strata/Dockerfile -t strata .

# Pinned by digest so an upstream rebuild cannot change the engine under us.
# cuda13 needs NVIDIA driver 580 or newer on the host.
ARG STRATA_IMAGE=ghcr.io/niko1221/strata:0.2.7-cuda13@sha256:db96cd9b111c930f7deb77444f38f66e53db43af57644a33a9e8a2f05f038360

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/agent ./cmd/agent
COPY internal/agent ./internal/agent
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agent ./cmd/agent

# The draft layer (speculative decoding, about twice the output speed) is not a
# file on Hugging Face. Strata's own tools read its 31 tensors (about 5 GB) out
# of the 360 GB Qwen checkpoint with range requests, check each against a pinned
# SHA-256, and pack them. On a worker that costs minutes of every cold start, so
# it is done here once; the raw tensors are dropped and only the packed result
# is kept. Strata's setup skips the step when it finds mtp/rt/experts.bin.
FROM ${STRATA_IMAGE} AS draft
ENV HOME=/tmp \
    STRATA_GGUF_PY=/opt/strata/seed/third_party/llama.cpp/gguf-py
RUN set -eu; \
    PY=/opt/strata/terminal/python/bin/python3; T=/opt/strata/terminal/tools; \
    "$PY" "$T/mtp_fetch.py" fetch --out /draft/mtp; \
    "$PY" "$T/mtp_fetch.py" verify --out /draft/mtp; \
    "$PY" "$T/mtp_pack.py" --src /draft/mtp --experts q2_0 --out /draft/mtp/mtp-q2_0.gguf; \
    "$PY" "$T/mtp_rt.py" --gguf /draft/mtp/mtp-q2_0.gguf --out /draft/mtp/rt; \
    rm -rf /draft/mtp/tensors; \
    test -s /draft/mtp/rt/experts.bin; \
    du -ah /draft/mtp
# The tensors are a derivative of Qwen/Qwen3.8-Flash-Next (Qwen Community
# License 1.0), which asks for its notice to travel with every copy.
ADD --checksum=sha256:a0dc422560841fd68e06d974907f8b4c709bca44a67daad2b528437bdf676c08 \
    https://huggingface.co/Qwen/Qwen3.8-Flash-Next/resolve/de4b8e4d43b917e7706784d8bb445c9af86a3540/LICENSE \
    /draft/mtp/LICENSE.qwen

FROM ${STRATA_IMAGE}

# The agent downloads models with the Hugging Face CLI, which needs Python.
# Strata's bundled interpreter is left alone.
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-pip ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && pip3 install --no-cache-dir --break-system-packages "huggingface_hub==2.4.0" hf_xet \
 && hf version

# Not under /data: that is a declared volume, so files put there at build time
# are lost. The agent links this folder to /data/Strata-data/mtp at start.
COPY --from=draft /draft/mtp /opt/strata-draft/mtp
COPY --from=build /out/agent /usr/local/bin/agent

# RunPod's load balancer polls GET /ping on PORT; the agent answers it and runs
# Strata on a private port. The unit sets MODEL, FAMILY and the other Strata
# settings as plain environment variables.
ENV AGENT_ENGINE=strata \
    AGENT_MODELS_DIR=/models \
    PORT=8080 \
    PORT_HEALTH=8080
EXPOSE 8080

LABEL org.opencontainers.image.licenses="MIT AND LicenseRef-Qwen-Community-1.0"

# The upstream health check polls Strata directly; readiness is the agent's job.
HEALTHCHECK NONE
# tini stays PID 1 so the server Strata starts in the background is reaped.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/agent"]
CMD []
