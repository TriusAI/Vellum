# Vellum — notes for agents working on this repo

Vellum is a local-first personal library manager: Go static binary,
llama.cpp (llama-server) for the LLM + embeddings, MuPDF (mutool) for PDF
text extraction, Tesseract for OCR, one SQLite file (FTS5) for storage.

## Layout

- `cmd/vellum/` — CLI (subcommands + global flag prescan in main.go;
  `serve` and the `agent` doc sheet live here too)
- `internal/` — config, db (schema + FTS5 triggers), extract (mutool +
  tesseract subprocesses), llm (llama-server client), summarize
  (map-reduce + constrained tagging), search (FTS5 + cosine), ingest
  (pipeline + the ordered `Enrich` used by the watcher), api (JSON API +
  embedded web UI in internal/api/web/; `internal/api/watch.go` is the
  filesystem watcher run by `serve`)
- `templates/qwen3-nothink.jinja` — chat template that disables Qwen3's
  thinking pass (empty think-block prefill); shipped in the pack and image
- `pack/` — portable bundle + Docker image build scripts
- `skills/vellum/` — installable agent skill (thin; points to `vellum agent`)
- `tests/` — e2e (`go test ./tests/`), self-contained: generates its own
  fixtures (testutil PDF writer), throwaway config/db per run

## Testing

    go test ./tests/ -v
    # with LLM pipeline (starts its own llama-servers):
    MUTOOL=... LLAMA_SERVER_BIN=... QWEN_GGUF=... NOMIC_GGUF=... \
        go test -timeout 30m ./tests/ -v

The e2e asserts: OCR'd scan indexed, summaries sane, tags strictly
in-vocabulary, semantic search hits. LLM part skips cleanly without a
server.

## Building the bundles

    pack/build-mutool.sh     # pinned MuPDF from source
    pack/build-llamacpp.sh   # pinned official llama.cpp prebuilts (CPU+Vulkan)
    GGUF_DIR=... pack/build.sh
    # Docker: see pack/Dockerfile header (needs pack/docker/{models,vellum})

## Invariants worth knowing

- The tag enum is enforced by grammar-constrained decoding — the
  load-bearing feature. Anything that touches `internal/summarize`'s
  schema or `internal/llm` must keep response_format intact.
- SQLite schema must stay compatible with existing libraries (no
  destructive migrations; additive columns only, update documentColumns
  projections).
- llama.cpp serves one model per process: chat (`-c 8192`) and embed
  (nomic, 2048-token context — never pass -c to it) on separate ports.
  The embed model runs on the CPU backend by design (small-VRAM GPU
  contention crashes it).
- `process` results (tags, metadata) come from an LLM: sanitize junk
  values ("unknown", hedging) at the boundary, never trust them raw.

## Environment quirks observed on this machine (2026-10)

- No sudo: mutool is built from source under /tmp; tesseract languages
  live in repo `tessdata/` via TESSDATA_PREFIX.
- Docker Hub is blocked by the local proxy; use
  `--build-arg REGISTRY_PREFIX=mirror.gcr.io/`.
- zsh: no word-splitting of unquoted `$VARS` in command lines; avoid
  pkill patterns that match your own shell; `rm -f dir/glob*` fails when
  no match — use explicit names.
- Stale llama-servers (or a leftover `vellum serve`) on fixed ports make
  new servers fail to bind SILENTLY — clients then talk to the stale one
  (wrong flags, wrong model). Before runs/after manual probes: `ss -tln |
  grep -E '808[12]|1808[12]|8090'` and kill leftovers. The e2e helper now
  uses ephemeral ports and a process-liveness check by design.