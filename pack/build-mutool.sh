#!/bin/sh
# Builds a release mutool from the pinned MuPDF tag, for bundling in the
# portable pack (and for local dev — see tests/e2e_test.go MUTOOL=...).
#
# Usage: pack/build-mutool.sh [output-binary] [workdir]
set -e

VERSION=1.28.5
OUT="${1:-pack/bin/mutool}"
WORK="${2:-build/mupdf}"

mkdir -p "$(dirname "$OUT")"
if [ ! -x "$WORK/build/release/mutool" ]; then
    if [ ! -d "$WORK/.git" ]; then
        git clone --depth 1 --branch "$VERSION" \
            --recurse-submodules --shallow-submodules \
            https://github.com/ArtifexSoftware/mupdf.git "$WORK"
    fi
    make -C "$WORK" build=release HAVE_X11=no HAVE_GLUT=no HAVE_CURL=no \
        -j"$(nproc)" tools
fi

cp "$WORK/build/release/mutool" "$OUT"
echo "mutool -> $OUT"
ldd "$OUT" || true