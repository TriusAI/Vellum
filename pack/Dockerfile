# Vellum Docker image: llama-server (models baked in as GGUFs) + mutool +
# tesseract + vellum. No ollama, no runners — CPU inference only.
# Build from the repo root:
#   pack/build-llamacpp.sh pack/docker/llama-server
#   mkdir -p pack/docker/models
#   cp <qwen3 gguf> pack/docker/models/qwen3-4b.gguf
#   cp <nomic gguf> pack/docker/models/nomic-embed-text-v1.5.gguf
#   CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o pack/docker/vellum ./cmd/vellum
#   docker build -f pack/Dockerfile -t vellum .
# On restricted networks, prefix registries with Google's official Docker Hub
# mirror:  docker build --build-arg REGISTRY_PREFIX=mirror.gcr.io/ ...
#
# Run (library lives in the mounted /data):
#   docker run --rm -v "$PWD:/data" vellum ingest /data/library
#   docker run --rm -v "$PWD:/data" vellum search "epistemic humility" --semantic
#
# The models are plain COPY layers, so image rebuilds after code changes are
# cheap (docker's content cache keys them by file digest).
ARG REGISTRY_PREFIX=""
FROM ${REGISTRY_PREFIX}ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
        mupdf-tools tesseract-ocr \
        tesseract-ocr-eng tesseract-ocr-chi-sim tesseract-ocr-fin \
        curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY pack/bin/llama-server-cpu/ /llm/cpu/
COPY pack/docker/models/*.gguf /models/
COPY templates/qwen3-nothink.jinja /templates/
COPY vocab.yaml /defaults/vocab.yaml
COPY pack/docker/vellum /usr/local/bin/vellum
COPY pack/docker-entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/vellum /llm/cpu/llama-server

ENV VELLUM_HOME=/data
WORKDIR /data
ENTRYPOINT ["/entrypoint.sh"]