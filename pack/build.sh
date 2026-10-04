#!/bin/sh
# Assembles the portable one-folder pack: vellum binary + mutool + tesseract
# + tessdata + llama-server + the two GGUF models + config. Unpack anywhere
# and run ./vellum.sh
#
# Usage: pack/build.sh [--skip-models]
#   --skip-models   assemble without the ~2.8GB model files (offline test)
#
# Sources, in order of availability:
#   mutool        pack/bin/mutool (pack/build-mutool.sh) or PATH
#   llama-server  pack/bin/llama-server (pack/build-llamacpp.sh)
#   tesseract     PATH (its shared libs are copied next to it)
#   GGUF models   $GGUF_DIR (two .gguf files), the running ollama's blob
#                 store, or the standard download URLs
set -e
cd "$(dirname "$0")/.."

VERSION="$(git describe --tags --always 2>/dev/null || echo dev)"
STAGE="pack/stage/vellum-$VERSION-linux-amd64"
ARCH="$(uname -m)"

echo "==> staging into $STAGE"
rm -rf "$STAGE"
mkdir -p "$STAGE/bin" "$STAGE/lib" "$STAGE/models" "$STAGE/library"

echo "==> building vellum (static)"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
    -o "$STAGE/vellum" ./cmd/vellum

echo "==> mutool"
if [ -x pack/bin/mutool ]; then
    cp pack/bin/mutool "$STAGE/bin/mutool"
elif command -v mutool >/dev/null; then
    cp "$(command -v mutool)" "$STAGE/bin/mutool"
else
    echo "no mutool — run pack/build-mutool.sh first" >&2
    exit 1
fi
command -v strip >/dev/null && strip "$STAGE/bin/mutool"

echo "==> llama-server (CPU + Vulkan prebuilts)"
if [ -x pack/bin/llama-server-cpu/llama-server ] && \
   [ -x pack/bin/llama-server-vulkan/llama-server ]; then
    mkdir -p "$STAGE/llm/cpu" "$STAGE/llm/vulkan"
    cp -r pack/bin/llama-server-cpu/.    "$STAGE/llm/cpu/"
    cp -r pack/bin/llama-server-vulkan/. "$STAGE/llm/vulkan/"
else
    echo "no llama-server prebuilts — run pack/build-llamacpp.sh first" >&2
    exit 1
fi

echo "==> tesseract (+ shared libs)"
TESS="$(command -v tesseract || true)"
if [ -z "$TESS" ]; then
    echo "tesseract not found in PATH" >&2
    exit 1
fi
cp "$TESS" "$STAGE/bin/tesseract"
# copy non-glibc deps (glibc is assumed present on the target system)
ldd "$TESS" | awk '{print $3}' | grep -v '^$' | while read -r lib; do
    case "$lib" in
        */libc.so*|*/libm.so*|*/libpthread.so*|*/ld-linux*|*/libdl.so*|*/libgcc_s.so*) ;;
        *) cp -L "$lib" "$STAGE/lib/" ;;
    esac
done

echo "==> tessdata"
cp -r tessdata "$STAGE/tessdata"

echo "==> chat template"
mkdir -p "$STAGE/templates"
cp templates/qwen3-nothink.jinja "$STAGE/templates/"

echo "==> models"
if [ "$1" = "--skip-models" ]; then
    echo "    skipped (--skip-models)"
else
    if [ -d "$GGUF_DIR" ] && ls "$GGUF_DIR"/*.gguf >/dev/null 2>&1; then
        cp "$GGUF_DIR"/*.gguf "$STAGE/models/"
    elif command -v python3 >/dev/null && [ -d "$HOME/.ollama/models" ]; then
        echo "    extracting GGUFs from the running ollama's blob store..."
        GGUF_DIR="$PWD/$STAGE/models" python3 - <<'PYEOF'
import json, os, shutil
store = os.path.expanduser('~/.ollama/models')
targets = {
    'manifests/registry.ollama.ai/library/qwen3/4b': 'qwen3-4b.gguf',
    'manifests/registry.ollama.ai/library/nomic-embed-text/latest': 'nomic-embed-text-v1.5.gguf',
}
outdir = os.environ['GGUF_DIR']
for manifest_rel, out_name in targets.items():
    with open(os.path.join(store, manifest_rel)) as f:
        m = json.load(f)
    for layer in m['layers']:
        if layer['mediaType'] == 'application/vnd.ollama.image.model':
            src = os.path.join(store, 'blobs', 'sha256-' + layer['digest'].split(':')[-1])
            shutil.copyfile(src, os.path.join(outdir, out_name))
            break
    else:
        raise SystemExit('model layer not found in ' + manifest_rel)
PYEOF
    else
        echo "    downloading GGUFs..."
        curl -fsSL -o "$STAGE/models/qwen3-4b.gguf" \
            "https://huggingface.co/Qwen/Qwen3-4B-GGUF/resolve/main/Qwen3-4B-Q4_K_M.gguf"
        curl -fsSL -o "$STAGE/models/nomic-embed-text-v1.5.gguf" \
            "https://huggingface.co/nomic-ai/nomic-embed-text-v1.5-GGUF/resolve/main/nomic-embed-text-v1.5.Q8_0.gguf"
    fi
fi

echo "==> config + launcher"
cat > "$STAGE/config.yaml" <<EOF
# Vellum portable pack configuration. Paths are relative to this file.
library_dir: library
db: library.db

# informational: which GGUF each llama-server hosts (see vellum.sh)
models:
  llm: qwen3-4b.gguf
  embed: nomic-embed-text-v1.5.gguf

llm:
  think: false             # qwen3 thinking mode: better, much slower on CPU
  temperature: 0.3

ocr:
  langs: eng+chi_sim+fin
  dpi: 300
  min_chars_per_page: 50
  workers: 3

tools:
  mutool: bin/mutool
  tesseract: bin/tesseract
  tessdata: tessdata
  llm_url: http://127.0.0.1:8081
  embed_url: http://127.0.0.1:8082
EOF
cp vocab.yaml "$STAGE/vocab.yaml"

cat > "$STAGE/vellum.sh" <<'EOF'
#!/bin/sh
# Vellum portable launcher. llama.cpp serves one model per process, so this
# starts two llama-servers (chat on 8081, embeddings on 8082) if they aren't
# already up, then hands off to the vellum binary. Everything stays inside
# this folder. Override via env:
#   VELLUM_LLM_GGUF (default qwen3-4b.gguf)   VELLUM_LLM_PORT (default 8081)
#   VELLUM_EMBED_GGUF (default nomic-embed-text-v1.5.gguf)
#   VELLUM_EMBED_PORT (default 8082)
#   VELLUM_BACKEND=cpu|vulkan  (default: auto)
set -e
ROOT="$(cd "$(dirname "$0")" && pwd)"
export LD_LIBRARY_PATH="$ROOT/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export TESSDATA_PREFIX="$ROOT/tessdata"
LLM_GGUF="${VELLUM_LLM_GGUF:-qwen3-4b.gguf}"
LLM_PORT="${VELLUM_LLM_PORT:-8081}"
EMBED_GGUF="${VELLUM_EMBED_GGUF:-nomic-embed-text-v1.5.gguf}"
EMBED_PORT="${VELLUM_EMBED_PORT:-8082}"
TEMPLATE="$ROOT/templates/qwen3-nothink.jinja"

up() { curl -fsS -o /dev/null "http://127.0.0.1:$1/health" 2>/dev/null; }

# pick_backend: start one llama-server, preferring vulkan (GPU) when
# available and falling back to the CPU build. Used for the CHAT model —
# the small embedding model always runs on the CPU backend to avoid
# GPU-memory contention with the big model on small GPUs.
start_llm_server() { # $1 port  $2 gguf  $3 logname  $4 extra-args...
    port="$1"; gguf="$2"; log="$3"; shift 3
    backend=""
    case "${VELLUM_BACKEND:-auto}" in
        vulkan) backend=vulkan ;;
        cpu) backend=cpu ;;
        auto)
            if [ -e /dev/dri ]; then backend=vulkan; else backend=cpu; fi
            ;;
    esac

    for attempt in 1 2; do
        dir="$ROOT/llm/$backend"
        if [ "$attempt" = 2 ] || [ ! -x "$dir/llama-server" ]; then
            backend=cpu
            dir="$ROOT/llm/cpu"
        fi
        "$dir/llama-server" -m "$ROOT/models/$gguf" \
            --host 127.0.0.1 --port "$port" -np 1 \
            "$@" > "$ROOT/$log" 2>&1 &
        pid=$!
        i=0
        while ! up "$port"; do
            i=$((i+1))
            if [ "$i" -gt 90 ]; then
                kill "$pid" 2>/dev/null
                if [ "$attempt" = 1 ] && [ "$backend" = "vulkan" ]; then
                    echo "vellum: $backend server failed (see $log) — falling back to cpu" >&2
                    backend=cpu
                    break
                fi
                echo "llama-server ($backend) did not come up (see $log)" >&2
                exit 1
            fi
            sleep 1
        done
        [ "$i" -le 90 ] && return 0
    done
    return 1
}

start_embed_server() { # $1 port  $2 gguf  $3 logname
    # the embedding model is tiny and CPU-only here: it is fast enough on
    # CPU and leaves the GPU (if any) entirely to the chat model
    "$ROOT/llm/cpu/llama-server" -m "$ROOT/models/$2" \
        --host 127.0.0.1 --port "$1" -np 1 --embeddings \
        > "$ROOT/$3" 2>&1 &
    pid=$!
    i=0
    while ! up "$1"; do
        i=$((i+1))
        [ "$i" -gt 60 ] && { echo "embedding server did not come up (see $3)" >&2; exit 1; }
        sleep 1
    done
}

if ! up "$LLM_PORT"; then
    # chat server: full 8192 context, qwen3 no-think template
    start_llm_server "$LLM_PORT" "$LLM_GGUF" "llm-server.log" \
        -c 8192 --jinja --chat-template-file "$TEMPLATE"
    echo "vellum: started chat server on :$LLM_PORT"
fi
if ! up "$EMBED_PORT"; then
    start_embed_server "$EMBED_PORT" "$EMBED_GGUF" "embed-server.log"
    echo "vellum: started embedding server on :$EMBED_PORT"
fi

exec "$ROOT/vellum" --config "$ROOT/config.yaml" "$@"
EOF
chmod +x "$STAGE/vellum.sh"

echo "==> tarball"
tar -C pack/stage -czf "pack/vellum-$VERSION-linux-$ARCH.tar.gz" \
    "vellum-$VERSION-linux-amd64"
du -sh "$STAGE" "pack/vellum-$VERSION-linux-$ARCH.tar.gz"
echo "done: pack/vellum-$VERSION-linux-$ARCH.tar.gz"