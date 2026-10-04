# Third-party notices

Vellum is licensed under the GNU AGPL-3.0 (see LICENSE). The distributable
pack additionally contains these third-party components:

## mutool (MuPDF) — AGPL-3.0
- Copyright Artifex Software, Inc.
- Source: https://mupdf.com / https://github.com/ArtifexSoftware/mupdf
- Built from the tag pinned in `pack/build-mutool.sh` (no modifications);
  the exact corresponding source is that upstream tag.
- MuPDF bundles AGPL/GPL-compatible third-party libraries (freetype, lcms2,
  harfbuzz, jbig2dec, openjpeg, zlib, ...) whose notices are in the
  corresponding source tree under `thirdparty/`.

## llama.cpp (llama-server) — MIT
- Copyright the llama.cpp authors
- Source: https://github.com/ggml-org/llama.cpp
- Built from the release tag pinned in `pack/build-llamacpp.sh` (no
  modifications; CPU-only build flags).

## Ollama (model provenance only) — MIT
- The bundled GGUF files were obtained from the Ollama registry during the
  pack build (or are downloaded directly from Hugging Face); the Ollama
- software itself is NOT part of the pack.
- Source: https://github.com/ollama/ollama

## Tesseract OCR — Apache-2.0
- Copyright Google LLC / contributors
- Source: https://github.com/tesseract-ocr/tesseract
- Depends on Leptonica (BSD-like) and bundled language data files
  (`tessdata/`, from https://github.com/tesseract-ocr/tessdata_fast, which
  inherit the upstream tessdata licensing — check per language).

## modernc.org/sqlite — BSD-3-Clause
- Copyright the modernc.org authors
- A transpilation of SQLite (public domain) to Go.
- Source: https://gitlab.com/cznic/sqlite

## gopkg.in/yaml.v3 — Apache-2.0
- Copyright the Go YAML authors
- Source: https://github.com/go-yaml/yaml

## Models
- `qwen3:4b` — Apache-2.0, https://huggingface.co/Qwen
- `nomic-embed-text` — see its model card on Ollama/Hugging Face for the
  current license terms.