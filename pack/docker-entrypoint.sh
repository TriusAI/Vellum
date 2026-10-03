#!/bin/sh
# Vellum container entrypoint: start the in-container ollama server, make
# sure /data has a config + vocab, then exec vellum.
set -e

ollama serve >/dev/null 2>&1 &
for i in $(seq 1 120); do
    curl -fsS -o /dev/null http://localhost:11434/ && break
    sleep 1
done

# First run: seed /data with defaults (config with tools resolved from PATH —
# apt installs mutool/tesseract inside the image).
if [ ! -f /data/config.yaml ]; then
    cat > /data/config.yaml <<'EOF'
# Vellum config (container). Everything lives in /data on your host.
library_dir: library
db: library.db

models:
  llm: qwen3:4b
  embed: nomic-embed-text

ocr:
  langs: eng+chi_sim+fin
  dpi: 300
  min_chars_per_page: 50
  workers: 3

tools:
  mutool: mutool
  tesseract: tesseract
EOF
fi
if [ ! -f /data/vocab.yaml ]; then
    cp /defaults/vocab.yaml /data/vocab.yaml
fi

exec vellum --config /data/config.yaml "$@"