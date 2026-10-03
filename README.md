# Vellum

A small, **fully local** library manager for books, papers, and literature.
It indexes your files, OCRs whatever needs OCR, writes a summary paragraph,
and tags every document against **your own controlled vocabulary** — so you
never end up with `ml`, `ML`, `machine learning`, and `machinelearning` as
four different tags.

Everything runs locally on CPU: [Ollama](https://ollama.com) serves a small
LLM (default Qwen3-4B) and an embedding model; MuPDF (`mutool`) extracts
text; Tesseract handles OCR. Storage is a single SQLite file with FTS5
full-text search. `vellum` is one static Go binary (AGPL-3.0).

## How it works

```
ingest ──▶ text extraction (mutool)      born-digital PDFs, EPUB (via conversion), TXT, MD
        └▶ OCR fallback (Tesseract)     pages with little text but images
process ──▶ summarize (map-reduce)       long docs: per-chunk summaries, then one paragraph
         └▶ tag (constrained decoding)  the LLM *cannot* spell a tag outside your vocabulary
search ───▶ FTS5 keyword + semantic      embeddings via nomic-embed-text, cosine search
```

### Why tags don't drift

Tagging uses Ollama's **structured output**: the JSON schema sent with the
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
extraction/OCR, Ollama for the models.

```bash
go build -o vellum ./cmd/vellum
go test ./tests/          # e2e; set MUTOOL=<path> if mutool is not on PATH
```

```bash
ollama pull qwen3:4b          # summarization + tagging (~2.5 GB)
ollama pull nomic-embed-text  # semantic search embeddings (~270 MB)
```

Tesseract language data lives in `tessdata/` (project-local, so adding
languages needs no root): currently `eng`, `chi_sim`, `fin` (fast variants).
To add a language, drop its `.traineddata` there and extend `ocr.langs`.

## Usage

```bash
# 1. index things
vellum ingest ~/papers/that-scan.pdf ~/books/
vellum process            # summarize + tag everything pending (slow on CPU — batch it)

# 2. find things
vellum search "quantum error correction"     # FTS5 keyword
vellum search "papers arguing against tabula rasa" --semantic

# 3. inspect
vellum show all
vellum show 42

# 4. curate the vocabulary
vellum vocab list
vellum vocab review                          # what the LLM suggested beyond the vocabulary
vellum vocab promote marine-biology "Study of ocean life"   # adopt a suggestion
vellum vocab add my-new-tag "what it covers"
```

## Download pack (one folder, everything included)

`pack/build.sh` assembles a portable, self-contained folder —
Go binary + mutool + tesseract (+ libs) + traineddata + ollama +
the models (~3 GB, dominated by qwen3:4b):

```bash
pack/build-mutool.sh      # builds mutool from the pinned MuPDF tag
pack/build.sh             # -> pack/vellum-<ver>-linux-amd64.tar.gz
```

Unpack and run — `vellum.sh` starts the bundled Ollama server on first use
and points everything at the bundled tools/models:

```bash
tar -xzf vellum-*-linux-amd64.tar.gz
cd vellum-*-linux-amd64
./vellum.sh ingest ~/books/
```

## Docker image

Same content as a container (models baked into the image):

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o pack/docker/vellum ./cmd/vellum
docker build -f pack/Dockerfile -t vellum .

mkdir vellum-data && cd vellum-data
docker run --rm -v "$PWD:/data" vellum ingest /data/library
docker run --rm -v "$PWD:/data" vellum process
docker run --rm -v "$PWD:/data" vellum search "tabula rasa" --semantic
```

`config.yaml`, `vocab.yaml`, `library.db`, and the `library/` folder all
live in the mounted `/data`.

## Configuration

`config.yaml` (found via `--config`, `$VELLUM_CONFIG`, or `./config.yaml`):
model names, thinking mode, context size, OCR languages/DPI/page-detection
threshold, chunk size, max tags, and tool paths (`tools.mutool`,
`tools.tesseract`, `tools.tessdata`) — the pack sets these to its bundled
binaries. `vocab.yaml`: the controlled vocabulary — tag name → description;
descriptions are shown to the LLM when it chooses, so write them clearly.

The SQLite schema is identical to the retired Python prototype's, so a
`library.db` created by it keeps working as-is.

## Notes & limits

- **CPU speed**: Qwen3-4B runs at a few tokens/s on a typical 8-core CPU.
  A 20-page paper takes a few minutes; a 300-page book considerably longer.
  Ingest/OCR/FTS never touch the LLM and are fast. On a GPU later: change
  `models.llm`, set `llm.think: true`.
- **Scanned PDFs with a bad text layer**: pages with ≥ `min_chars_per_page`
  extractable characters are trusted as born-digital. If a scan has a junk
  OCR layer, lower the threshold or delete the text layer first.
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
│   ├── llm/             # Ollama client (structured output + embeddings)
│   ├── ingest/           # pipeline: ingest + process (summarize/tag)
│   ├── summarize/       # map-reduce summaries + constrained tagging
│   ├── vocab/           # vocabulary load/save
│   ├── search/          # FTS5 + cosine semantic search
│   └── db/              # SQLite schema + FTS5 triggers
├── tests/               # e2e test + a minimal test-only PDF writer
├── testdata/            # scan stand-in image for the OCR path
├── pack/                # portable pack + Docker image build scripts
├── tessdata/            # tesseract .traineddata (project-local)
├── config.yaml          # dev config
└── vocab.yaml           # controlled tag vocabulary
```

## License

AGPL-3.0 (see LICENSE and THIRD-PARTY-NOTICES.md — MuPDF is AGPL, which is
also why Vellum is).