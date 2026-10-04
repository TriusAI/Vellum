#!/bin/sh
# Vellum container entrypoint: start the two llama.cpp servers (chat on
# 8081, embeddings on 8082), make sure /data has a config + vocab, then
# exec vellum.
set -e

/llm/cpu/llama-server -m /models/qwen3-4b.gguf \
    --host 127.0.0.1 --port 8081 -c 8192 -np 1 --jinja \
    --chat-template-file /templates/qwen3-nothink.jinja \
    > /tmp/llm-server.log 2>&1 &
/llm/cpu/llama-server -m /models/nomic-embed-text-v1.5.gguf \
    --host 127.0.0.1 --port 8082 -np 1 --embeddings --ubatch-size 2048 \
    > /tmp/embed-server.log 2>&1 &

up() { curl -fsS -o /dev/null "http://127.0.0.1:$1/health" 2>/dev/null; }
i=0
while ! up 8081; do
    i=$((i+1))
    [ "$i" -gt 600 ] && { echo "chat server did not come up:" >&2; tail /tmp/llm-server.log >&2; exit 1; }
    sleep 1
done
i=0
while ! up 8082; do
    i=$((i+1))
    [ "$i" -gt 300 ] && { echo "embedding server did not come up:" >&2; tail /tmp/embed-server.log >&2; exit 1; }
    sleep 1
done

# First run: seed /data with defaults (tools resolve from PATH — apt installs
# mutool/tesseract inside the image; servers run on the ports above).
if [ ! -f /data/config.yaml ]; then
    cat > /data/config.yaml <<'EOF'
# Vellum config (container). Everything lives in /data on your host.
library_dir: library
db: library.db

models:
  llm: qwen3-4b.gguf
  embed: nomic-embed-text-v1.5.gguf

llm:
  think: false
  temperature: 0.3
  num_ctx: 8192

ocr:
  langs: eng+chi_sim+fin
  dpi: 300
  min_chars_per_page: 50
  workers: 3

tools:
  mutool: mutool
  tesseract: tesseract
  llm_url: http://127.0.0.1:8081
  embed_url: http://127.0.0.1:8082
EOF
fi
if [ ! -f /data/vocab.yaml ]; then
    cp /defaults/vocab.yaml /data/vocab.yaml
fi

exec vellum --config /data/config.yaml "$@"