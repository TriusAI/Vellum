#!/bin/sh
# Fetches the official llama.cpp release binaries (CPU + Vulkan) into
# pack/bin/, for bundling in the portable pack. The official prebuilts are
# built on an old-Ubuntu toolchain (good glibc portability) and carry
# optimized CPU variants (alderlake/zen4/...) next to the binary.
#
# TAG is pinned to a release that was actually tested; bump deliberately.
# The Vulkan build also contains the CPU backends, so it degrades to CPU
# on machines without a usable GPU — the launcher still ships the plain
# CPU build as a safe fallback.
#
# Usage: pack/build-llamacpp.sh
set -e
cd "$(dirname "$0")/.."

TAG=b11382
BASE="https://github.com/ggml-org/llama.cpp/releases/download/$TAG"

echo "==> llama.cpp $TAG: CPU + Vulkan prebuilts"
rm -rf pack/bin/llama-server-cpu pack/bin/llama-server-vulkan
mkdir -p pack/bin/llama-server-cpu pack/bin/llama-server-vulkan

curl -fsSL -o /tmp/llama-cpu.tar.gz   "$BASE/llama-$TAG-bin-ubuntu-x64.tar.gz"
curl -fsSL -o /tmp/llama-vulkan.tar.gz "$BASE/llama-$TAG-bin-ubuntu-vulkan-x64.tar.gz"

tar -xzf /tmp/llama-cpu.tar.gz    -C pack/bin/llama-server-cpu    --strip-components=1
tar -xzf /tmp/llama-vulkan.tar.gz -C pack/bin/llama-server-vulkan --strip-components=1

du -sh pack/bin/llama-server-cpu pack/bin/llama-server-vulkan
echo "done."