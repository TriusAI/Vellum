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
    vellum export [PATH]         # consistent snapshot of the library (backup)
                                  # (safe while processing runs)
    vellum import PATH           # replace the library with a backup
                                  # (STOP a running serve first — it holds
                                  #  the old file; the UI import swaps live)
    vellum show all | ID          # inspect
    vellum remove ID [ID...]      # remove documents from the library index
                                  # (the files stay on disk — indexed in place)
    vellum rename-category OLD NEW
                                  # rename a shelf; its whole subtree moves
                                  # (machine-learning -> computer-science/
                                  #  machine-learning also moves .../transformers)
    vellum collection list|create|delete|add|remove|show
                                  # user-managed groups of documents
                                  # (research projects; a doc can be in many)
    vellum collection export NAME|ID [PATH.zip]
                                  # bundle a collection (files + metadata)
                                  # into a shareable .zip
    vellum collection import PATH.zip [--name NAME]
                                  # restore a bundle into this library
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

## Backends: bundled llama.cpp, your own llama.cpp, or ollama
llm.backend: llama-server (default; bundled) — the shell launcher starts
the bundled qwen3 + nomic servers (skip-able individually):
    llm: {external: true}      # your OWN llama-server at tools.llm_url
    embed: {external: true}    # your own embedding server
    llm: {backend: ollama, model: qwen3:4b}
    embed: {provider: ollama, model: nomic-embed-text}
llm.external / embed.external: true points the pipeline at servers you
run yourself (fast GPU hosts welcome); tools.llm_url / tools.embed_url
must be reachable then. IMPORTANT: constraints survive the switch —
tags stay grammar-bound on every backend (llama-server response_format
= ollama "format" = the same JSON schema), and llm.num_ctx must match
the actual server context on any backend (ollama: num_ctx option).

## Ingesting (folders, and notes vaults)
Ingest is RECURSIVE: a directory contributes every supported file beneath
it at ANY depth, so pointing ingest at a folder — including an Obsidian
vault — indexes it in one go:
    vellum ingest ~/MyVault          # .md/.markdown/.txt/.rst + PDF-likes
    POST /api/ingest {"paths":["/abs/dir"]}
Hidden entries are SKIPPED (a vault's .obsidian/, .trash/, .git/ and
dotfiles are not library material). Supported = .md/.markdown/.txt/.rst
and .pdf/.epub/.mobi/.azw/.azw3/.fb2; anything else is ignored.

## Deep links (the web UI is URL-addressable)
The active page is mirrored into the URL, so any view can be pasted,
bookmarked, or linked from another app (e.g. an Obsidian note):
    /?doc=42                      [Summary] for document 42
    /?doc=42&view=preview&page=7  [Preview] open at page 7
    /?doc=42&view=text   /?doc=42&view=ask
    /?tag=attention   /?collection=3   /?note=5
    /?view=tags   /?view=notes   /?view=collections
    /?q=terms&semantic=1          All Documents showing search results
The link button in each page's title bar copies the current page's URL.

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

## Skipping pages
pages hidden from the Text tab AND excluded from document text
(summarization/regeneration/ask-context). Stored on documents.skip_pages
as a csv ("3,7-12"); searchable until re-extracted (skip != delete).
    vellum skip ID 3,7-12
    vellum skip ID -          # clear
    PATCH /api/documents/{id} {"skip_pages": "3,7-12"}
The UI Text tab has a per-page skip button and un-hide chips.

## Regenerate individual metadata
Per-field rebuilds WITHOUT a full reprocess ("meta" = title/authors/year
via one cheap call; "summary" = the kind's fast path; "tags"/"category"
= one tag call applied to that field; "kind" = deterministic
reclassify on current text — user-kind pins LIFT only when the user
explicitly re-generates kind/category):
    vellum regenerate ID [meta|summary|tags|category|kind ...]
    POST /api/documents/{id}/regenerate {"fields":["summary","tags"]}

## Filesystem watcher (auto-ingest on arrival)
While vellum serve runs, a background watcher scans the configured
folders on a timer and auto-runs the pipeline on new/changed files, in
this order: ingest -> kind -> category -> metadata (title/author/year)
-> tags -> summary. The summary is generated LAST, so tags come from
the document's text rather than from a summary that does not exist yet.
Enrichment needs the vocabulary and the chat backend; without them the
files are still indexed and get enriched on a later scan. A file must
hold the same size+mtime across two scans before it is picked up, so a
half-written download is never indexed ("Scan now" acts at once).
Managed from the UI's Watch dialog or:
    vellum watch                 # show settings + pending count
    vellum watch add PATH...     # watch these folders (and enable)
    vellum watch remove PATH...  # stop watching these folders
    vellum watch on|off          # toggle the background watcher
    vellum watch interval N      # seconds between scans (>= 2)
    vellum watch run             # one-shot scan + enrich (cron-friendly)
Scans run through the same FIFO job queue, so they appear in the Jobs
API/UI and can be cancelled like any other job.

## Jobs: queue + cancellation
process / regenerate / reextract / ingest run through ONE FIFO queue;
results arrive strictly in request order. Every job appears in the
Jobs API/UI and can be cancelled individually: queued jobs leave the
queue instantly; running ones stop cooperatively before their next
unit of work (file / document / section / page / LLM call).
    GET  /api/jobs              # [{id, kind, label, status, message, ...}]
    POST /api/jobs/{id}/cancel  # queued=leave, running=stop at next unit

## Settings API (live config)
    GET /api/config                 # editable subset (api keys masked)
    PUT /api/config {...}           # applied IMMEDIATELY + saved to yaml
Sections: llm {backend, model, external, think, temperature, num_ctx,
url}, embed {...}, ocr {langs, dpi, workers, min_chars_per_page},
summarize {chunk_chars, max_tags}, ask {...}. Absent fields keep their
current settings. NOTE: server lifecycle (starting/stopping the
bundled llama-servers) still needs a launcher rerun.

## Library backup (export/import)
The library is ONE SQLite file — a backup is a consistent snapshot of it
(VACUUM INTO; safe to take while processing runs). Import validates the
upload (query-only: must have a documents table), refuses while jobs are
queued/running, keeps the replaced library as <db>.pre-import-<timestamp>
and reopens it. UI: the "Import/Export…" dialog.
    GET  /api/library/export     # download the snapshot (attachment)
    POST /api/library/import     # request body = the backup file
                                 # (loopback-only, like /api/fs)

## Semantic search + embedding prefixes
vellum search Q --semantic (and the UI "semantic" toggle) embeds the
query and ranks chunk vectors by cosine similarity. Retrieval models are
trained with ASYMMETRIC task instructions, so vellum prepends the model's
prefixes automatically (detected from the running embed server):
nomic -> "search_query: "/"search_document: "; EmbeddingGemma ->
"task: search result | query: "/"title: none | text: ". Unknown models
get none. Prefixes are part of the vector space: changing the embed
model/provider/URL (or the prefix scheme) invalidates stored vectors and
they re-embed lazily. vellum embed re-embeds the whole library up front.

## Ask an LLM (about one document)
A freeform streaming chat per document, powered by an EXTERNAL model
(config: ask: {provider: none|openai|anthropic|ollama, model, api_key,
base_url, tools}; openai = any OpenAI-COMPATIBLE endpoint via base_url).
The chat context = metadata + summary + opening text of that document.
    GET  /api/ask/config            # (key masked: key_set; includes tools)
    PUT  /api/ask/config {provider, model, api_key?, base_url, tools?}
    POST /api/ask/test              # tiny exchange; {} = test current
    POST /api/documents/{id}/ask    # stream: "data: {...}\n\n" events
                                    #   {"d": "..."} deltas, {"e": "..."}
                                    #   error, {"tool":"url"} a fetch,
                                    #   {"done":"1"} end
Provider can be overridden per request ({"provider":"ollama",...}).
This chat does NOT affect tagging (which stays grammar-constrained).

WEB FETCH: with ask.tools true (default), the model may call a fetch_url
tool to read external http(s) links the user or the document references
(a small local WebFetch: HTML reduced to text, size-capped; non-http(s)
and link-local metadata hosts refused; honors HTTPS_PROXY). The server
runs the tool loop (up to 4 rounds) and reports each fetch as a
{"tool":"url"} event. IMPLEMENTED for OpenAI-compatible providers
(including Ollama's /v1 endpoint and local llama.cpp) AND Anthropic
(tool_use/tool_result); Ollama's NATIVE /api/chat ignores tools. Answers
are rendered with light markdown (code, bold/italic, lists, links) in the UI.

## Fixing garbled text
When the text (Text tab in the UI) is garbled — a bad OCR pass baked
into the file by some other tool: re-extract with raster forced:
    vellum reextract ID --force-ocr          # OCR every page, replace text
    vellum reextract ID --pages 3,7-12       # OCR only those pages
    vellum reextract ID                      # repair only broken-looking pages
    POST /api/documents/{id}/reextract {"force":true,"pages":[3,7]}
Per-page repairs accumulate (documents.ocr_done_pages), so fixing one
page does not undo the previous fix when the text pass rebuilds.
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
and the model never re-files a pinned document.
AUTO-FILING LEARNS FROM YOUR CORRECTIONS: every document you shelved by
hand is recorded, and when the next similar document is processed, the
most textually similar user-shelved documents are fed to the tagging
call as few-shot examples ("file an alike document the same way").
Correcting a mis-shelved paper teaches the next one (no retraining —
retrieval from your own pins; keyword-overlap matching, deterministic). Process results carry
the field ("category") and the UI shows the shelf as a chip.
    vellum category 42 ai-papers     # set; "none" clears
    vellum show all --kind paper --category ai-papers --tag transformer
    vellum search "attention" --kind paper --tag ai --category ai-papers
(--tag repeatable; a doc must have ALL listed tags.)
The API accepts kind/category in GET/PATCH /api/documents, filter params
on GET /api/documents and GET /api/search (kind, category, repeated tag),
and GET /api/categories lists distinct categories with counts.

## Tags page + per-tag pages
The UI's "Tags…" opens a tag CLOUD (tag size grows with how many
documents carry it); each tag opens a [Tag] page listing its documents
(like a collection page). Sources: the controlled vocabulary, tags
actually applied, and LLM-suggested tags (shown dashed).
    GET /api/tags          # [{tag, documents, description, suggested}]
    GET /api/tags/{tag}    # {tag, description, documents:[documentJSON]}

## Notes (scratchpad)
Freeform notes for jotting ideas while reading — a body plus created_at /
updated_at, nothing else. The UI's "Notes…" management page lists them
(first line = title); each opens a [Note] page that is a single textarea
with debounced autosave.
    vellum note list|add "text"|show ID|delete ID   (add - reads stdin)
    GET/POST /api/notes          GET/PATCH/DELETE /api/notes/{id}
    PATCH {"body":"..."} bumps updated_at.

## Themes (UI colors)
The web UI's colors are data: a preset (light|dark|sepia|contrast) plus
optional per-variable overrides, saved in config.yaml under theme: and
applied as CSS variables. Settings -> Theme has a preset picker and a
color input per variable (--bg, --fg, --muted, --accent, --card, --line,
--chip). Presentation only; the pipeline is unaffected. GET/PUT /api/config
carry it (theme: {preset, colors}).

## Collections (research projects)
A collection is a user-managed group of documents — e.g. an applied-ML
project holding ML papers plus the medicine papers you plan to apply
them to. A document can be in several collections; membership is
ENTIRELY manual (nothing in processing adds or removes). Deleting a
document or a collection cleans up membership rows (cascade).
    vellum collection create applied-ml "ML x medicine"
    vellum collection add applied-ml 3 7 12
    vellum collection show applied-ml
    vellum collection list
    vellum collection remove applied-ml 3
    vellum collection delete applied-ml     # documents are untouched
    GET    /api/collections                 # [{id,name,description,documents}]
    POST   /api/collections                 # {name, description}; 409 on dup name
    GET    /api/collections/{id}            # {collection, documents:[...]}
    PATCH  /api/collections/{id}            # {name?, description?}
    DELETE /api/collections/{id}
    POST   /api/collections/{id}/documents  # {doc_ids:[...]} (idempotent add)
    DELETE /api/collections/{id}/documents/{docID}
GET /api/documents/{id} also returns "collections" (the doc's memberships).
The UI has a "Collections…" page (create/browse/manage) and per-document
[Collection] pages; the Summary page has an add/remove control.

## Sharing a collection (.zip bundle)
A collection can be exported as a self-contained .zip and handed to
someone else: it holds the member documents' original files under files/
and a manifest.json (collection name/description + each document's
metadata and tags). Importing extracts the files under
<library_dir>/collections/<name>/, ingests them (text layers only, no
OCR), recreates the collection (the name is made unique if taken; --name
or ?name= overrides) and restores the metadata — the originator's
kind/category become user pins (they were human-set). Loopback-only.
    vellum collection export applied-ml applied-ml.zip   # share this
    vellum collection import applied-ml.zip [--name other]
    GET  /api/collections/{id}/export    # the zip (Content-Disposition)
    POST /api/collections/import         # body = the zip
UI: "Export .zip" on a collection page; "Import .zip…" on Collections….

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
    POST /api/categories/rename     # {"from":"...","to":"..."} — subtree move
                                     # (renaming onto an existing shelf merges)
    GET  /api/documents/{id}        # + chunks, tag_sources
    DELETE /api/documents/{id}      # remove from the library (file stays on disk)
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
    GET  /api/watch                 # {enabled, dirs, interval, running,
                                    #  last_scan, added, pending, last_error}
    PUT  /api/watch                 # {"enabled":..,"dirs":[..],"interval":..}
    POST /api/watch/scan            # scan + enrich now -> {ok, job, added}
    GET  /api/search?q=&mode=keyword|semantic&limit=N
    GET  /api/vocab                 # [{"name":..,"description":..}]
    GET  /api/vocab/suggestions     # LLM-proposed tags for review
    POST /api/vocab                 # {"name":..,"description":..} add/promote
    DELETE /api/vocab/{name}
    GET  /api/tags                  # tag cloud: [{tag,documents,description,suggested}]
    GET  /api/tags/{tag}            # {tag,description,documents:[...]}
    GET  /api/notes                 # [{id,body,created_at,updated_at}]
    POST /api/notes                 # {"body":..} -> the new note
    GET  /api/notes/{id}            # one note
    PATCH /api/notes/{id}           # {"body":..}, bumps updated_at
    DELETE /api/notes/{id}
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
