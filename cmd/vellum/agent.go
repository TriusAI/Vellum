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
summaries + opening text, not a huge raw prefix. To go faster:
- swap the chat model for a smaller one (e.g. qwen3-1.7b): drop the GGUF in
  the pack's models/ and set models.llm (+ VELLUM_LLM_GGUF for vellum.sh,
  or start llama-server with -m qwen3-1.7b.gguf yourself). No rebuild needed.
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
    GET  /api/documents             # all documents (with tags)
    GET  /api/documents/{id}        # + chunks, tag_sources
    GET  /api/documents/{id}/file    # the original file (?dl=1 to download)
    PATCH /api/documents/{id}       # {"title":..,"authors":..,"year":..,"summary":..}
    PUT  /api/documents/{id}/tags   # {"tags":[...]} (replaces; source=manual)
    POST /api/ingest                # {"paths":[...],"reprocess":false} -> stats
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
- Scanned PDFs: pages with < ocr.min_chars_per_page extractable chars but
  images get OCR'd (tesseract). A scan with a junk text layer needs
  min_chars_per_page lowered.
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
