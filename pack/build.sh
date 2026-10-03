#!/bin/sh
# Assembles the portable one-folder pack: vellum binary + mutool + tesseract
# + tessdata + ollama + models + config. Unpack anywhere and run ./vellum.sh
#
# Usage: pack/build.sh [--skip-models]
#   --skip-models   assemble without the ~2.7GB model blobs (offline test)
#
# Sources, in order of availability:
#   mutool      pack/bin/mutool (build with pack/build-mutool.sh) or PATH
#   tesseract   PATH (its shared libs are copied next to it)
#   ollama      $OLLAMA_BIN, PATH, or downloaded from ollama.com
#   models      copied from the running ollama's model store (~/.ollama)
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

echo "==> ollama"
if [ -n "$OLLAMA_BIN" ] && [ -x "$OLLAMA_BIN" ]; then
    OLLAMA="$OLLAMA_BIN"
elif command -v ollama >/dev/null; then
    OLLAMA="$(command -v ollama)"
else
    echo "    downloading ollama for linux/$ARCH..."
    mkdir -p "$STAGE/dl"
    curl -fsSL "https://ollama.com/download/ollama-linux-$ARCH.tgz" -o "$STAGE/dl/ollama.tgz"
    tar -xzf "$STAGE/dl/ollama.tgz" -C "$STAGE/dl"
    OLLAMA="$STAGE/dl/bin/ollama"
fi
cp "$OLLAMA" "$STAGE/bin/ollama"
rm -rf "$STAGE/dl"

echo "==> models"
if [ "$1" = "--skip-models" ]; then
    echo "    skipped (--skip-models)"
else
    SRC="${OLLAMA_MODELS:-$HOME/.ollama/models}"
    if [ -d "$SRC" ]; then
        cp -r "$SRC/." "$STAGE/models/"
    else
        echo "    no model store found at $SRC — populating via ollama pull"
        OLLAMA_MODELS="$PWD/$STAGE/models" "$STAGE/bin/ollama" pull qwen3:4b
        OLLAMA_MODELS="$PWD/$STAGE/models" "$STAGE/bin/ollama" pull nomic-embed-text
    fi
fi

echo "==> config + launcher"
cat > "$STAGE/config.yaml" <<EOF
# Vellum portable pack configuration. Paths are relative to this file.
library_dir: library
db: library.db

models:
  llm: qwen3:4b
  embed: nomic-embed-text

llm:
  think: false
  num_ctx: 8192
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
EOF
cp vocab.yaml "$STAGE/vocab.yaml"

cat > "$STAGE/vellum.sh" <<'EOF'
#!/bin/sh
# Vellum portable launcher: starts the bundled ollama if none is running,
# then hands off to the vellum binary. Everything stays inside this folder.
set -e
ROOT="$(cd "$(dirname "$0")" && pwd)"
export OLLAMA_MODELS="$ROOT/models"
export LD_LIBRARY_PATH="$ROOT/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export TESSDATA_PREFIX="$ROOT/tessdata"

up() { curl -fsS -o /dev/null http://localhost:11434/api/tags 2>/dev/null; }

if ! up; then
    "$ROOT/bin/ollama" serve >/dev/null 2>&1 &
    echo "vellum: started bundled ollama server (background, pid $!)"
    i=0
    while ! up; do
        i=$((i+1))
        [ "$i" -gt 120 ] && { echo "ollama did not come up" >&2; exit 1; }
        sleep 1
    done
fi

exec "$ROOT/vellum" --config "$ROOT/config.yaml" "$@"
EOF
chmod +x "$STAGE/vellum.sh"

echo "==> tarball"
tar -C pack/stage -czf "pack/vellum-$VERSION-linux-$ARCH.tar.gz" \
    "vellum-$VERSION-linux-amd64"
du -sh "$STAGE" "pack/vellum-$VERSION-linux-$ARCH.tar.gz"
echo "done: pack/vellum-$VERSION-linux-$ARCH.tar.gz"