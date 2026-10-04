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
    vellum process               # summarize + tag pending docs (LLM, slow on CPU)
    vellum search QUERY           # FTS5 keyword search
    vellum search QUERY --semantic
    vellum show all | ID          # inspect
    vellum vocab list|review|promote|add|remove
    vellum serve [--listen ADDR] # local web UI + JSON API (see below)

Add --json to ingest/process/search/show/vocab for machine-readable output.
Configuration is read from ./config.yaml (or --config PATH / $VELLUM_CONFIG).

## Model servers
The chat server (llm_url, default 127.0.0.1:8081) is needed for
ingest-less commands only: process. The embed server (embed_url, :8082) is
needed for --semantic search (chunks are embedded lazily on first use).
If they are not running: with the portable pack, invoke ./vellum.sh (it
starts them); otherwise start them per README ("Build from source").

## Workflows

Add documents:      vellum ingest --json /path/to/dir
  -> {"added":N,"updated":N,"skipped":N,"failed":N,"files":[...]}
  (unchanged files are skipped via sha256; --reprocess forces re-extract)

Then process:       vellum process --json
  -> [{"id":N,"path":"...","status":"done","tags":["..."],"tags_other":[...]},...]
  SLOW on CPU (minutes per document) — batch it, don't loop one-by-one.

Find documents:     vellum search "query" --json [--semantic] [--limit N]
  keyword  -> [{"doc_id":N,"title":"...","page":N,"snippet":"..."},...]
  semantic -> [{"doc_id":N,"title":"...","snippets":[{"page":N,"text":"...","score":0.64},...]},...]

Inspect:            vellum show all --json
  -> [{"id":N,"path":"...","title":"...","authors":"...","year":"...",
       "summary":"...","status":"done","tags":["..."],"ocr_pages":N,...},...]

## Vocabulary curation (IMPORTANT)
Tags are constrained to vocab.yaml. When a document matches a topic
outside it, the LLM names it in tags_other (stored with source
"suggested"). Review and promote good ones:
    vellum vocab review --json      # -> [{"tag":"...","count":N,"example":"..."}]
    vellum vocab promote NAME "description shown to the LLM"
    vellum vocab add NAME "description"    # add directly
    vellum vocab remove NAME
Editing vocab.yaml by hand is also fine (name: description, YAML map).

## JSON API (vellum serve, default http://127.0.0.1:8090)
    GET  /api/status                 # counts, server availability
    GET  /api/documents             # all documents (with tags)
    GET  /api/documents/{id}        # + chunks, tag_sources
    PATCH /api/documents/{id}       # {"title":..,"authors":..,"year":..,"summary":..}
    PUT  /api/documents/{id}/tags   # {"tags":[...]} (replaces; source=manual)
    POST /api/ingest                # {"paths":[...],"reprocess":false} -> stats
    POST /api/process               # {"limit":0} -> per-document results (slow)
    GET  /api/search?q=&mode=keyword|semantic&limit=N
    GET  /api/vocab                 # [{"name":..,"description":..}]
    GET  /api/vocab/suggestions     # LLM-suggested tags for review
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
