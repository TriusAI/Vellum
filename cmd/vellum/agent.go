package main

import (
	"fmt"

	"vellum/internal/api"
	"vellum/internal/config"
)

// agentDocs is the single source of truth for AI-agent usage, printed by
// `vellum agent` (and consumed by skills/vellum — see the repo). Keep it
// accurate: it documents commands, JSON output, the JSON API, workflows,
// and the operational gotchas agents most often trip on.
const agentDocs = `# Vellum — agent instructions

Vellum is a personal-library manager: it indexes books/papers (text
extraction + OCR), summarizes them with a local LLM (qwen3-4b via
llama.cpp llama-server), and tags each document against a CONTROLLED
vocabulary (vocab.yaml) using grammar-constrained decoding — tags outside
the vocabulary cannot be emitted. Storage: one SQLite file (FTS5).
Everything is local; no network calls except the local model servers.

## Quick start
    vellum ingest PATH...        # index files/dirs (fast; no LLM needed)
    vellum process               # summarize + tag ALL pending docs (LLM, slow on CPU)
    vellum process ID [ID...]    # ...or specific documents, by id
    vellum search QUERY           # FTS5 keyword search
    vellum search QUERY --semantic
    vellum show all | ID          # inspect
    vellum vocab list|review|promote|add|remove
    vellum serve [--listen ADDR] # local web UI + JSON API (see below)

Add --json to ingest/process/search/show/vocab for machine-readable output.
Configuration is read from ./config.yaml (or --config PATH / $VELLUM_CONFIG).

## Model servers
The chat server (llm_url, default 127.0.0.1:8081) is needed for process;
the embed server (embed_url, :8082) for --semantic search (chunks are
embedded lazily on first use). If they are not running: with the portable
pack, invoke ./vellum.sh (it starts them); otherwise start them per README
("Build from source"). The chat server must be started with -c matching
llm.num_ctx in the config (pack default: 8192; raise it — and the server -c — if you have the memory) — the client budgets its
requests against that value and tokenizes via /tokenize to prevent
exceeds-context errors on large documents.

## Performance
process cost scales with document length (map over ~6000-char chunks).
Very long documents use a hierarchical reduce and are tagged from their
summaries + opening text, not a huge raw prefix. A 30-page paper is
~10-15 sections = several minutes even on GPU — progress is reported
live (CLI lines, /api/progress in the web UI). To go faster:
- swap the chat model for qwen3-1.7b (ships in the pack's models/):
  VELLUM_LLM_GGUF=qwen3-1.7b.gguf ./vellum.sh serve   — roughly 2x
  faster (smaller model, constrained tagging verified identical).
- raise summarize.chunk_chars for fewer, coarser map calls (lower fidelity).
- process documents individually (vellum process ID) — results appear per
  document, don't batch-wait.

## Workflows

Add documents:      vellum ingest --json /path/to/dir
  -> {"added":N,"updated":N,"skipped":N,"failed":N,"files":[...]}
  (unchanged files are skipped via sha256; --reprocess forces re-extract)

Then process:       vellum process --json [ID...]
  -> [{"id":N,"path":"...","status":"done","tags":["..."],"tags_other":[...]},...]
  SLOW on CPU (minutes per document). Prefer per-id processing for
  incremental work; a failed document keeps status "error" (its "error"
  field says why) and can be re-processed with vellum process ID.

Find documents:     vellum search "query" --json [--semantic] [--limit N]
  keyword  -> [{"doc_id":N,"title":"...","page":N,"snippet":"..."},...]
  semantic -> [{"doc_id":N,"title":"...","snippets":[{"page":N,"text":"...","score":0.64},...]},...]

Inspect:            vellum show all --json
  -> [{"id":N,"path":"...","title":"...","authors":"...","year":"...",
       "summary":"...","status":"done","tags":["..."],"ocr_pages":N,...},...]

## Ingest is two-phase (fast ingest, OCR at process time)
ingest reads TEXT LAYERS only and never OCRs: a scanned 300-page book
indexes in seconds, marked ocr_pending=1 with a thin (or empty) chunk
text. The heavy work happens in process:
    vellum process ID
    -> progress: "extracting text (OCR on raster pages)" — geometry
       (spread split + rotation) runs here too
    -> after OCR the document is RE-CLASSIFIED from the better text
       (unless you set the kind yourself; your override is kept)
So after ingesting scans, just run process and watch the progress
line; ingest "hang" on big scans is fixed.

## Fixing garbled text
When the text (Text tab in the UI) is garbled — a bad OCR pass baked
into the file by some other tool: re-extract with raster forced:
    vellum reextract ID --force-ocr    # OCR every page, replace text
    vellum reextract ID                # repair only broken-looking pages
    POST /api/documents/{id}/reextract {"force":true}
Text is replaced in place; status lands back on pending so the next
process rebuilds summary/tags. Run "vellum embed" afterwards to
refresh semantic vectors (chunk text changed).

## Document kinds, categories, and fast paths
Every document gets a kind at ingest (instant, deterministic heuristics):
"paper", "book", "gallery", "course", "reference", or "" (generic).
- papers with an Abstract section: the summary IS the extracted abstract
  (summary_source="abstract" — the authors' own words, no generation), and
  tagging runs once over it — seconds instead of minutes.
- books: the summary is the extracted FRONT MATTER (preface, else
  foreword, else introduction — the author's own overview of the book;
  summary_source="front-matter"), with the contents listing fed to
  tagging as a topic hint. Full map-reduce of a whole book only happens
  when no front matter is found.
- near-empty texts (galleries): processed without any LLM call.
- everything else: the generic map-reduce.
Override a kind (then re-process):
    vellum kind 42 paper
    vellum process 42

Categories are YOUR shelving (any string; e.g. "ai-papers", "theology").
Processing AUTO-FILES unshelved documents: the tagging call proposes one
category per document (reusing your existing shelves when one fits;
junk values are filtered); PATCH/vellum category pins it (category_user)
and the model never re-files a pinned document. Process results carry
the field ("category") and the UI shows the shelf as a chip.
    vellum category 42 ai-papers     # set; "none" clears
    vellum show all --kind paper --category ai-papers --tag transformer
    vellum search "attention" --kind paper --tag ai --category ai-papers
(--tag repeatable; a doc must have ALL listed tags.)
The API accepts kind/category in GET/PATCH /api/documents, filter params
on GET /api/documents and GET /api/search (kind, category, repeated tag),
and GET /api/categories lists distinct categories with counts.

## Vocabulary curation (IMPORTANT)
Tags are constrained to vocab.yaml. The tagging prompt also invites the
model to PROPOSE new tags when a document's central topic isn't covered:
these land in tags_other (stored with source "suggested"). Review and
promote good ones:
    vellum vocab review --json      # -> [{"tag":"...","count":N,"example":"..."}]
    vellum vocab promote NAME "description shown to the LLM"
    vellum vocab add NAME "description"    # add directly
    vellum vocab remove NAME
Editing vocab.yaml by hand is also fine (name: description, YAML map).

## JSON API (vellum serve, default http://127.0.0.1:8090)
    GET  /api/status                 # counts, server availability
    GET  /api/documents             # all documents (with tags); filters:
                                    #   ?kind=&category=&tag=&tag= (AND)'
    GET  /api/categories            # distinct categories + counts
    GET  /api/documents/{id}        # + chunks, tag_sources
    GET  /api/documents/{id}/file    # the original file (?dl=1 to download)
    PATCH /api/documents/{id}       # {"title":..,"authors":..,"year":..,
                                    #  "summary":..,"kind":..,"category":..}
    PUT  /api/documents/{id}/tags   # {"tags":[...]} (replaces; source=manual)
    POST /api/ingest                # {"paths":[...],"reprocess":false} -> stats
                                    # (fast: text layers only; progress via
                                    #  /api/progress; scans land ocr_pending)
    GET  /api/fs?path=/abs/dir      # dir listing for the ingest picker
                                    # (loopback-only; no file contents served)
    POST /api/process               # {"ids":[...]} or {} for all pending -> results
    GET  /api/search?q=&mode=keyword|semantic&limit=N
    GET  /api/vocab                 # [{"name":..,"description":..}]
    GET  /api/vocab/suggestions     # LLM-proposed tags for review
    POST /api/vocab                 # {"name":..,"description":..} add/promote
    DELETE /api/vocab/{name}
Errors: {"error":"..."} with 4xx/5xx status codes.

## Gotchas
- process requires a non-empty vocabulary; it refuses with a clear error
  otherwise (nothing is destroyed, just do vocab add first).
- CPU-only: process takes minutes per document; ingest/search are fast.
- Scan geometry is handled by the OCR pipeline: two-page spreads
  (open-book scans, one image per spread) are split at the detected
  gutter into two pages; rotated scans are turned upright (tesseract
  OSD when osd.traineddata is available, else ink-profile heuristics).
- A page whose EMBEDDED text layer is garbage (some other tool's bad
  OCR pass) is detected ("the quick brown fox" without spaces,
  glyph-code junk) and re-OCR'd from the raster — bad embedded layers
  no longer poison summaries/tags/search. No config knob needed.
- The ingest picker in the web UI browses the filesystem and picks
  files/directories (GET /api/fs?path=/abs/dir — loopback-only,
  read-only listings; entries carry "supported" flags).
- Metadata: PDF producers emit junk ("unknown"); Vellum filters known
  junk values and validates years as 4-digit patterns.
- Files are indexed in place (absolute paths, sha256 change detection).
- The database is a single SQLite file; vellum show is safe at any time.
`

func cmdAgent(cfg *config.Config, args []string) {
	if len(args) > 0 && (args[0] == "--json" || args[0] == "-json") {
		fmt.Printf("%q\n", agentDocs)
		return
	}
	fmt.Print(agentDocs)
}

// injectBuildVersion lets api report the same version the CLI prints.
func init() { api.SetVersion(versionString) }
