---
name: vellum
description: Use when the user wants to manage their personal library (books, papers, PDFs) with Vellum — ingest and index files, OCR scans, summarize with a local LLM, tag against the controlled vocabulary, curate tags, or search (keyword/semantic). Covers the `vellum` CLI (use --json for machine-readable output) and the local JSON API served by `vellum serve`. Trigger words: vellum, library, ingest, tag, summarize my books/papers, search my library.
---

# Vellum — personal library manager (agent guide)

Vellum runs **fully locally** (llama.cpp models, SQLite with FTS5). The
authoritative, always-current documentation ships inside the binary:

    vellum agent

Run that first — it prints the complete instruction sheet (commands, JSON
schemas, API endpoints, workflows, gotchas) for the binary you actually
have. This skill is only the orientation.

## Mental model

- **ingest** = text extraction (+ OCR for scanned pages). Fast, no LLM.
- **process** = local LLM writes a summary paragraph and picks tags that
  are **grammar-constrained to the vocabulary** (`vocab.yaml`) — it cannot
  emit out-of-vocabulary spellings; genuinely missing topics land in
  "suggested" tags for review. Slow on CPU (minutes/document): batch it,
  never loop document-by-document.
- **search** = FTS5 keyword, or `--semantic` (embeddings).

## Rules of thumb

1. Always add `--json` when you (the agent) run a command and need to
   parse the result; leave it off for human-readable output.
2. After `process`, check for suggested tags with
   `vellum vocab review --json` and ask the user which to promote:
   `vellum vocab promote NAME "description"`.
3. If `process` errors about the model server, the llama-servers aren't
   up. With the portable pack: run via `./vellum.sh`. From source: see
   the "Build from source" section of the README.
4. If `process` refuses because the vocabulary is empty, add tags first
   (`vellum vocab add NAME "description"`). Never invent tags client-side
   — tagging only works through the process pass or the API.
5. `vellum serve` exposes the same operations as a JSON API
   (`/api/...`, docs in `vellum agent`) plus a human web UI — useful when
   the user wants to browse or curate interactively.