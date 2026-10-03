# Vellum Docker image: ollama (models baked in) + mutool + tesseract + vellum.
# Build from the repo root:
#   CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o pack/docker/vellum ./cmd/vellum
#   docker build -f pack/Dockerfile -t vellum .
#
# Run (library lives in the mounted /data):
#   docker run --rm -v "$PWD:/data" vellum ingest /data/library
#   docker run --rm -v "$PWD:/data" vellum search "epistemic humility" --semantic
FROM ollama/ollama:latest AS ollama

FROM ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
        mupdf-tools tesseract-ocr \
        tesseract-ocr-eng tesseract-ocr-chi-sim tesseract-ocr-fin \
        curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=ollama /bin/ollama /usr/local/bin/ollama
COPY pack/docker/vellum /usr/local/bin/vellum
COPY pack/docker-entrypoint.sh /entrypoint.sh
COPY vocab.yaml /defaults/vocab.yaml
RUN chmod +x /entrypoint.sh /usr/local/bin/vellum

# Bake the models into the image (~3GB).
RUN /bin/sh -c 'ollama serve & \
    for i in $(seq 1 60); do curl -fsS -o /dev/null http://localhost:11434/ && break; sleep 1; done; \
    ollama pull qwen3:4b && ollama pull nomic-embed-text'

ENV VELLUM_HOME=/data
WORKDIR /data
ENTRYPOINT ["/entrypoint.sh"]