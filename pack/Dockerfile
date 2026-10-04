# Vellum Docker image: ollama (models baked in) + mutool + tesseract + vellum.
# Build from the repo root:
#   CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o pack/docker/vellum ./cmd/vellum
#   docker build -f pack/Dockerfile -t vellum .
# On restricted networks, prefix registries with Google's official Docker Hub
# mirror:  docker build --build-arg REGISTRY_PREFIX=mirror.gcr.io/ ...
#
# Run (library lives in the mounted /data):
#   docker run --rm -v "$PWD:/data" vellum ingest /data/library
#   docker run --rm -v "$PWD:/data" vellum search "epistemic humility" --semantic
#
# Layer order matters: the expensive model-pull layer comes before the
# frequently-changing vellum binary, so code changes rebuild only the tail.
ARG REGISTRY_PREFIX=""
FROM ${REGISTRY_PREFIX}ollama/ollama:latest AS ollama

FROM ${REGISTRY_PREFIX}ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
        mupdf-tools tesseract-ocr \
        tesseract-ocr-eng tesseract-ocr-chi-sim tesseract-ocr-fin \
        curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# ollama binary + its inference runtime. The main binary goes to
# /usr/local/bin; llama-server and its shared libs (RUNPATH $ORIGIN, so they
# must stay together) go to /usr/local/lib/ollama, which is one of ollama's
# search paths. CUDA/Vulkan backends are skipped — CPU image.
COPY --from=ollama /bin/ollama /usr/local/bin/ollama
COPY --from=ollama /lib/ollama/llama-server /usr/local/lib/ollama/
COPY --from=ollama /lib/ollama/libggml*.so* /usr/local/lib/ollama/
COPY --from=ollama /lib/ollama/libllama*.so* /usr/local/lib/ollama/
COPY --from=ollama /lib/ollama/libmtmd*.so* /usr/local/lib/ollama/
COPY --from=ollama /lib/ollama/libgomp*.so* /usr/local/lib/ollama/

# Bake the models into the image (~3GB). EXPENSIVE LAYER — placed before the
# vellum binary so it stays cached across code changes.
RUN /bin/sh -c 'ollama serve & \
    for i in $(seq 1 60); do curl -fsS -o /dev/null http://localhost:11434/ && break; sleep 1; done; \
    ollama pull qwen3:4b && ollama pull nomic-embed-text'

COPY vocab.yaml /defaults/vocab.yaml
COPY pack/docker/vellum /usr/local/bin/vellum
COPY pack/docker-entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/vellum

ENV VELLUM_HOME=/data
WORKDIR /data
ENTRYPOINT ["/entrypoint.sh"]