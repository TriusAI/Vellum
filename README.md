# Vellum

A small, local-model library manager for books, papers, and literature.
It indexes your files, OCRs whatever needs OCR, writes a summary paragraph,
and tags every document against **your own controlled vocabulary** — so you
never end up with `ml`, `ML`, `machine learning`, and `machinelearning` as
four different tags.

Everything runs locally on CPU: [Ollama](https://ollama.com) serves a small
LLM (default Qwen3-4B) and an embedding model; Tesseract handles OCR.
Storage is a single SQLite file with FTS5 full-text search.

## How it works

```
ingest ──▶ text extraction (PyMuPDF)   born-digital PDFs, EPUB, TXT, MD
        └▶ OCR fallback (Tesseract)    pages with little text but images
process ─▶ summarize (map-reduce)      long docs: per-chunk summaries, then one paragraph
        └▶ tag (constrained decoding)  the LLM *cannot* spell a tag outside your vocabulary
search ──▶ FTS5 keyword + semantic     embeddings via nomic-embed-text, cosine search
```

### Why tags don't drift

Tagging uses Ollama's **structured output**: the JSON schema sent with the
request contains

```json
"tags": { "type": "array", "items": { "type": "string", "enum": ["epistemology", "cryptography", ...] } }
```

Constrained decoding makes it physically impossible for the model to emit
anything outside the `enum`. The schema also contains a freeform `tags_other`
field (capped at 3 entries): when a document clearly belongs to a topic you
haven't defined yet, the LLM names it there. Review those with
`vellum vocab review` and promote the keepers into `vocab.yaml` — your
vocabulary grows deliberately instead of drifting.

## Setup

Prerequisites: Python ≥ 3.10, [Tesseract](https://github.com/tesseract-ocr/tesseract),
and [Ollama](https://ollama.com).

```bash
cd Vellum
python -m venv .venv && . .venv/bin/activate
pip install -e .            # installs the `vellum` command

ollama pull qwen3:4b        # summarization + tagging (~2.5 GB)
ollama pull nomic-embed-text  # semantic search embeddings (~270 MB)
```

Tesseract language data lives in `tessdata/` (project-local, so adding
languages needs no root). It currently ships `eng`, `chi_sim`, `fin`
(fast variants — see [tessdata_fast](https://github.com/tesseract-ocr/tessdata_fast));
to add a language, drop its `.traineddata` there and add the code to
`ocr.langs` in `config.yaml`.

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

## Configuration

`config.yaml` (next to the library DB): model names, thinking mode, context
size, OCR languages/DPI/page-detection threshold, chunk size, max tags.
`vocab.yaml`: the controlled vocabulary — tag name → description; the
descriptions are shown to the LLM when it chooses, so write them clearly.

## Notes & limits

- **CPU speed**: Qwen3-4B runs at a few tokens/s on a typical 8-core CPU.
  A 20-page paper takes a few minutes; a 300-page book considerably longer.
  Ingest/OCR/FTS never touch the LLM and are fast. If you later get a GPU,
  change `models.llm` and set `llm.think: true` for better summaries.
- **Scanned PDFs with a bad text layer**: pages with ≥ `min_chars_per_page`
  extractable characters are trusted as born-digital. If a scan has a junk
  OCR layer, lower the threshold or delete the text layer first.
- **Semantic search** is brute-force cosine over chunk vectors — instant at
  personal-library scale (tens of thousands of chunks).
- File paths are stored as absolute paths; files are indexed in place.
  Moving files means re-ingesting (the DB rows update by path).

## Project layout

```
Vellum/
├── config.yaml        # settings
├── vocab.yaml         # controlled tag vocabulary
├── library.db         # created on first run (documents, chunks, tags, FTS5)
├── tessdata/          # Tesseract .traineddata files
├── vellum/
│   ├── cli.py         # commands
│   ├── ingest.py      # extraction pipeline
│   ├── ocr.py         # PyMuPDF extraction + Tesseract fallback
│   ├── summarize.py   # map-reduce summaries + constrained tagging
│   ├── llm.py         # Ollama chat (structured output) + embeddings
│   ├── vocab.py       # vocabulary load/save
│   ├── db.py          # SQLite schema + FTS5
│   └── embed.py       # chunk embeddings + cosine search
└── tests/             # end-to-end tests with generated sample docs
```