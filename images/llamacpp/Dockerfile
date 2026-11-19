# syntax=docker/dockerfile:1
# llama.cpp engine image: the upstream server image with our agent as entrypoint.
# Build from the repository root:
#   docker build -f images/llamacpp/Dockerfile -t llamacpp .

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/agent ./cmd/agent
COPY internal/agent ./internal/agent
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agent ./cmd/agent

# Pinned by digest so an upstream rebuild cannot change the engine under us.
FROM ghcr.io/ggml-org/llama.cpp:server-cuda@sha256:ae96e52f8b2106ea82bad786ee2d08bb9b0a8c93403df9c27ae1bf065a118089

# The agent downloads models with the Hugging Face CLI, which needs Python.
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-pip ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && pip3 install --no-cache-dir --break-system-packages "huggingface_hub==2.4.0" hf_xet \
 && hf version

COPY --from=build /out/agent /usr/local/bin/agent

# RunPod's load balancer polls GET /ping on PORT; the agent answers it and
# runs llama-server on a private port.
ENV AGENT_ENGINE=llama.cpp \
    AGENT_MODELS_DIR=/models \
    PORT=8080 \
    PORT_HEALTH=8080
EXPOSE 8080

# The upstream health check polls llama-server directly; readiness is the agent's job.
HEALTHCHECK NONE
ENTRYPOINT ["/usr/local/bin/agent"]
