# Vellum

A small, **fully local** library manager for books, papers, and literature.
It indexes your files, OCRs whatever needs OCR, writes a summary paragraph,
and tags every document against **your own controlled vocabulary** — so you
never end up with `ml`, `ML`, `machine learning`, and `machinelearning` as
four different tags.

Everything runs locally on CPU: [llama.cpp](https://github.com/ggml-org/llama.cpp)
(`llama-server`) serves a small LLM (default Qwen3-4B) and an embedding
model as two tiny processes; MuPDF (`mutool`) extracts text; Tesseract
handles OCR. Storage is a single SQLite file with FTS5 full-text search.
`vellum` is one static Go binary (AGPL-3.0).

## How it works

```
ingest ──▶ text extraction (mutool)      born-digital PDFs, EPUB (via conversion), TXT, MD
        └▶ OCR fallback (Tesseract)     pages with little text but images
process ──▶ summarize (map-reduce)       long docs: per-chunk summaries, then one paragraph
         └▶ tag (constrained decoding)  the LLM *cannot* spell a tag outside your vocabulary
search ───▶ FTS5 keyword + semantic      embeddings via nomic-embed-text, cosine search
```

### Why tags don't drift

Tagging uses llama.cpp's **structured output**: the JSON schema sent with the
request contains

```json
"tags": { "type": "array", "items": { "type": "string", "enum": ["epistemology", "cryptography", ...] } }
```

Grammar-constrained decoding makes it impossible for the model to emit
anything outside the `enum`. The schema also has a freeform `tags_other`
field (capped at 3 entries): when a document clearly belongs to a topic you
haven't defined yet, the LLM names it there. Review those with
`vellum vocab review` and promote the keepers into `vocab.yaml` — your
vocabulary grows deliberately instead of drifting.

## Build from source

Prerequisites: Go ≥ 1.24, `mutool` (MuPDF tools) and `tesseract` on PATH for
extraction/OCR, a llama.cpp `llama-server` for the models.

```bash
go build -o vellum ./cmd/vellum
go test ./tests/   # e2e; MUTOOL/LLAMA_SERVER_BIN/QWEN_GGUF/NOMIC_GGUF to test the LLM part
```

llama.cpp serves one model per process — two small servers:

```bash
llama-server -m qwen3-4b.gguf --port 8081 -c 8192 -np 1 --jinja \
    --chat-template-file templates/qwen3-nothink.jinja &   # chat: summarize + tag
llama-server -m nomic-embed-text-v1.5.gguf --port 8082 -np 1 --embeddings &  # semantic search
```

Tesseract language data lives in `tessdata/` (project-local, so adding
languages needs no root): currently `eng`, `chi_sim`, `fin` (fast variants).
To add a language, drop its `.traineddata` there and extend `ocr.langs`.

## Usage

```bash
# 1. index things
vellum ingest ~/papers/that-scan.pdf ~/books/
vellum process [ID...]     # summarize + tag pending (all, or specific docs by id)

# 2. find things
vellum search "quantum error correction"     # FTS5 keyword
vellum search "papers arguing against tabula rasa" --semantic
# filters (on search and "show all"): --kind --category --tag (repeatable)
vellum show all --kind paper --category ai-papers
vellum search "attention" --tag transformer --kind paper
# auto-categorization: processing files unshelved documents into
# categories (reusing your existing shelves; user-set shelves are kept)
vellum category 3 theology      # pin or re-file manually

# 3. inspect
vellum show all
vellum show 42

# 4. curate the vocabulary
vellum vocab list
vellum vocab review                          # what the LLM suggested beyond the vocabulary
vellum vocab promote marine-biology "Study of ocean life"   # adopt a suggested tag
vellum vocab add my-new-tag "what it covers"

# 5. browse: local web UI (search, read summaries/chunks, edit metadata,
#    curate tags, trigger ingest/process — all in the browser; ingest picks
#    files with a built-in filesystem picker)
vellum serve --open
```

Add `--json` to any command for machine-readable output; `vellum agent`
prints the full AI-agent instruction sheet (commands, JSON schemas, the
`/api/` JSON surface, workflows, gotchas).

## AI agents

Vellum is agent-friendly out of the box, in the spirit of lark-cli:

- `vellum agent` — the authoritative instruction sheet, shipped inside
  the binary so it can never go stale relative to the installed version.
- `--json` on every command (works anywhere in the argument list).
- a local JSON API with the same operations as the CLI when
  `vellum serve` is running (`/api/...`, documented by `vellum agent`).
- an installable skill at `skills/vellum/` — copy it into your agent's
  skills directory (e.g. `~/.claude/skills/` or `~/.codex/skills/`),
  and the agent will know when and how to drive Vellum.

## Download pack (one folder, everything included)

`pack/build.sh` assembles a portable, self-contained folder —
Go binary + mutool + tesseract (+ libs) + traineddata + llama.cpp's
llama-server (official prebuilts: CPU 44MB + Vulkan 86MB, auto-selected
with CPU fallback) + the two GGUF models (~2.8 GB, dominated by qwen3-4b):

```bash
pack/build-mutool.sh      # builds mutool from the pinned MuPDF tag
pack/build-llamacpp.sh    # builds llama-server from the pinned llama.cpp tag
pack/build.sh             # -> pack/vellum-<ver>-linux-amd64.tar.gz
```

Unpack and run — `vellum.sh` starts the two bundled llama-servers (chat +
embeddings) if they aren't already up, then runs the vellum binary:

```bash
tar -xzf vellum-*-linux-amd64.tar.gz
cd vellum-*-linux-amd64
./vellum.sh ingest ~/books/
```

## Docker image

Same content as a container (models baked into the image; ~3 GB — the
ollama base and its GPU runners are gone, inference is llama.cpp):

```bash
pack/build-llamacpp.sh pack/docker/llama-server
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o pack/docker/vellum ./cmd/vellum
mkdir -p pack/docker/models
cp <qwen3 gguf>   pack/docker/models/qwen3-4b.gguf
cp <nomic gguf>   pack/docker/models/nomic-embed-text-v1.5.gguf
docker build -f pack/Dockerfile -t vellum .

mkdir vellum-data && cd vellum-data
docker run --rm -v "$PWD:/data" vellum ingest /data/library
docker run --rm -v "$PWD:/data" vellum process
docker run --rm -v "$PWD:/data" vellum search "tabula rasa" --semantic
```

`config.yaml`, `vocab.yaml`, `library.db`, and the `library/` folder all
live in the mounted `/data`.

## Backends: bundled, your own llama.cpp, or Ollama
The pipeline (summarize + grammar-constrained tagging + embeddings)
talks to any of:

- the **bundled** llama.cpp servers (default; the launcher starts them);
- **your own llama.cpp** — set `llm: {external: true}` (+ `embed:
  {external: true}` as wanted); nothing is launched, your
  `tools.llm_url`/`tools.embed_url` must be reachable (GPU-hosted
  llama-servers are the fast option; set `llm.num_ctx` to its `-c`);
- **Ollama** — `llm: {backend: ollama, "model": "..."}` (+
  `embed: {provider: ollama, "model": ...}`): tags stay grammar-bound
  via Ollama's structured outputs (same schema, same guarantee).

## Ask an LLM (chat about an document)
The detail pane has an "Ask an LLM" tab: a streaming chat served by an
EXTERNAL provider of your choice — `ask: {provider: none|openai|
anthropic|ollama, model, api_key, base_url}` in config.yaml (openai =
any OpenAI-compatible endpoint via `base_url`; editable + testable in
the UI too). The chat sees that document's metadata, summary, and
opening text. Tagging remains constrained by the pipeline backend
regardless.

## Configuration

`config.yaml` (found via `--config`, `$VELLUM_CONFIG`, or `./config.yaml`):
thinking mode, temperature, OCR languages/DPI/page-detection threshold,
chunk size, max tags, and tool paths (`tools.mutool`, `tools.tesseract`,
`tools.tessdata`, `tools.llm_url`, `tools.embed_url`) — the pack sets these
to its bundled binaries and servers. `vocab.yaml`: the controlled
vocabulary — tag name → description; descriptions are shown to the LLM when
it chooses, so write them clearly.

The SQLite schema is identical to the retired Python prototype's, so a
`library.db` created by it keeps working as-is.

## Notes & limits

- **CPU speed**: Qwen3-4B runs at a few tokens/s on a typical 8-core CPU.
  A 20-page paper takes a few minutes; a 300-page book considerably longer.
  Ingest/OCR/FTS never touch the LLM and are fast. Long documents use a
  hierarchical reduce; tagging reads the computed summaries plus the
  opening text, not a huge raw prefix. To speed up further: process
  documents individually (`vellum process ID`, per-doc button in the web
  UI), raise `summarize.chunk_chars` (coarser map calls), or swap in the
  bundled smaller model: `VELLUM_LLM_GGUF=qwen3-1.7b.gguf ./vellum.sh serve`
  (it ships in the pack's `models/` and is roughly 2× faster). Progress
  is reported live — per-section lines in the CLI, a live status line in
  the web UI. No rebuild needed.
- **Context window**: the client tokenizes inputs (llama-server `/tokenize`)
  and trims to fit `llm.num_ctx` (pack default 8192; raise it and the server -c if you have the memory), so arbitrarily
  large documents can't produce exceeds-context errors; the chat server
  must be started with a matching `-c`.
- **Vocabulary growth**: the tagging prompt invites the model to propose
  new tags (`tags_other`, surfaced by the Vocabulary dialog and
  `vellum vocab review`); promote the keepers and the vocabulary grows
  with your library instead of staying stagnant.
- **Server lifecycle**: llama.cpp serves one model per process, so the
  launcher runs two small llama-servers (chat :8081, embeddings :8082) and
  reuses them if they're already up.
- **Ingest is two-phase**: ingest reads text layers only (seconds for
  anything, including scanned books — OCR work is deferred to
  `vellum process`, which re-classifies the document on the better
  text unless you set the kind yourself). Watch the progress line at
  process time: "extracting text (OCR on raster pages)".
- **Fixing garbled text**: if the text layer of a file is garbage
  (some other tool's bad OCR), re-extract with the raster forced:
  `vellum reextract ID --force-ocr` (or the detail pane's button in
  the web UI; same repair is automatic at process time for pages
  that look broken). Run `vellum embed` after to refresh vectors.
- **Scan geometry** (v0.6.0): OCR handles common scan quirks — open-book
  two-page spreads are split at the detected gutter (each half processed
  as its own page), rotated scans are turned upright (tesseract OSD when
  `tessdata/osd.traineddata` exists, ink-profile heuristics otherwise),
  and pages whose embedded text layer is garbage (broken word spacing,
  glyph-code junk — another tool's bad OCR pass) are detected and
  re-OCR'd from the raster instead of poisoning summaries and tags.
- **Semantic search** is brute-force cosine over chunk vectors — instant at
  personal-library scale (tens of thousands of chunks).
- **Portability**: the Go binary is fully static; the bundled `mutool` is
  built from the pinned upstream tag; tesseract's non-glibc libs ship in
  `lib/`. A reasonably current glibc on the target system is assumed.
- File paths are stored as absolute paths; files are indexed in place.

## Project layout

```
Vellum/
├── cmd/vellum/          # CLI entry point
├── internal/
│   ├── config/          # config.yaml loading
│   ├── extract/         # mutool extraction + tesseract OCR fallback
│   ├── llm/             # llama-server client (structured output + embeddings)
│   ├── ingest/           # pipeline: ingest + process (summarize/tag)
│   ├── summarize/       # map-reduce summaries + constrained tagging
│   ├── vocab/           # vocabulary load/save
│   ├── search/          # FTS5 + cosine semantic search
│   ├── api/             # JSON API + embedded web UI (vellum serve)
│   └── db/              # SQLite schema + FTS5 triggers
├── tests/               # e2e + API tests, self-generated fixtures
├── testdata/            # scan stand-in image for the OCR path
├── skills/vellum/       # installable AI-agent skill (see `vellum agent`)
├── templates/           # qwen3 no-think chat template (shipped in the pack)
├── pack/                # portable pack + Docker image build scripts
├── tessdata/            # tesseract .traineddata (project-local)
├── config.yaml          # dev config
└── vocab.yaml           # controlled tag vocabulary
```

## License

AGPL-3.0 (see LICENSE and THIRD-PARTY-NOTICES.md — MuPDF is AGPL, which is
also why Vellum is).