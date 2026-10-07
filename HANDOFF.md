# Vellum — HANDOFF

Handoff document for the next developer on Vellum. Read this top to bottom
before changing anything in `internal/`;

---

## 1. What Vellum is

Vellum is a **fully local, personal library manager** for books, papers, and
PDF/EPUB-style documents. It ingests files in place, extracts their text
(OCR where needed), classifies them into kinds and shelves them into
categories, produces summaries and assigns **tags from a controlled
vocabulary enforced at the LLM decode level** (vocabulary drift is
impossible, not just filtered), and offers keyword + semantic search plus a
web UI. One static Go binary + two small local LLM servers; everything runs
on CPU (a small GPU is used opportunistically), no cloud in the pipeline.

Target user: one person at one machine, a personal library of thousands of
documents. Multi-user, auth, and synchronization are explicitly out of
scope. License: AGPL-3.0 (MuPDF dependency requires it).

## 2. Runtime architecture

```
vellum serve  ────  HTTP :8090 (127.0.0.1)
   │                  ┌ JSON API under /api/*  (docs: `vellum agent` cmd)
   │                  └ embedded web UI (internal/api/web/, go:embed)
   │
   ├── chat llama-server :8081   (qwen3 GGUF, -c 8192, --jinja,
   │                              templates/qwen3-nothink.jinja = no-think prefill)
   └── embed llama-server :8082  (nomic-embed-text-v1.5, CPU ONLY,
        no -c override: 2048-token context, --ubatch-size 2048)
```

- The two llama-servers are started by the pack launcher `vellum.sh` (built
  from a heredoc inside `pack/build.sh` — **there is no separate
  vellum.sh source file**; edit the heredoc and rebuild/regenerate the
  stage copy).
- CLI subcommands all read `config.yaml`; global flags are pre-scanned
  anywhere in the args (`--json`, `--reprocess`, ...).
- The pipeline talks to llama.cpp via an OpenAI-compatible client
  (`internal/llm`), `response_format: json_schema` → **grammar-constrained
  decoding** is the load-bearing feature (see invariants §6).

### Data model (single SQLite file `library.db`)

- `documents` — one row per file: path (absolute, files are indexed in
  place, sha256 change detection), title/authors/year (LLM metadata, junk
  filtered), summary, `summary_source` (`abstract` | `front-matter` | `generated` |
  empty), `status` (`ingested` = pending | `done` | `error`), `kind`,
  `category` (user shelving; **slash-paths nest subcategories**:
  `ai/transformers`), plus the feature flags:
  - `ocr_pending` — ingest flagged it; processing will OCR (see §4)
  - `kind_user` / `category_user` — the user pinned it; automated
    classification/file-ization won't overwrite it
  - `ocr_done_pages` (csv or `all`) — pages repaired by force-OCR (they
    stay OCR-backed when the text is rebuilt)
  - `skip_pages` (csv) — pages hidden in the Text tab AND excluded from
    `DocumentText` (summarization/ask-context); still searchable
  - `error`, `ocr_pages`, `n_pages`, timestamps
- `chunks` — text per chunk (chunk ≈ page for PDFs, ~chunk_chars for text),
  with `embedding` (semantic) and a maintained FTS5 index (triggers).
- `doc_tags` (tag, source: vocab|manual|suggested), `fts`, `meta`.
- `collections` — user-managed named groups (research projects);
  `collection_docs` joins them to documents (many-to-many, both FKs
  ON DELETE CASCADE). Entirely manual; nothing in the pipeline touches
  them. New TABLES (not columns) land via the idempotent
  `CREATE TABLE IF NOT EXISTS` in the schema const — no migrate() row
  needed, old libraries pick them up at Open.
- `notes` — freeform scratchpad (id, body, created_at, updated_at);
  the UI's Notes pages. Also a new table (idempotent CREATE).
- `theme` (config.yaml) — {preset, colors} for the web UI only; carried
  by GET/PUT /api/config and applied as CSS variables.

## 3. Schema rules (non-negotiable)

- **Never destructive**: additive columns only; a migration = new `ALTER
  TABLE` row in `internal/db/db.go` migrate() + the same column inline in
  the CREATE TABLE block (fresh DBs). Old libraries keep working — check:
  existing `library.db` files open and function.
- If a projection changes, update **both** documents-columns constants and
  their scan structs: `docColumns` in `internal/api/api.go`
  (documentJSON + scanDoc + toJSON) and `documentColumns` in
  `cmd/vellum/main.go` (CLI `show` document.scan()).

## 4. Pipeline: two-phase ingest with deferred OCR

**Ingest is fast and never OCRs.** `ingest.Ingest` →
`extract.ExtractText` (text layer only, seconds even for a scanned
book). Pages that are thin or whose embedded text layer fails the sanity
check set `ocr_pending=1`.

**Process does the heavy work with live progress:** `ingest.ProcessPending`
→ per document `processOne` → (if `ocr_pending`) `extract.Extract` (raster
+ OCR with **scan geometry**, via the job's progress channel) → text
replaced → **re-classify** (`classify.Detect` on the better text — unless
`kind_user`) → summary by the kind's fast path → one grammar-constrained
tagging call (which also proposes a category).

Fast paths by kind (`internal/classify` + `produceSummary` in ingest):
- **paper** with an Abstract section → the summary IS the extracted
  abstract (`summary_source=abstract`); one tagging call.
- **book** → the summary is the extracted FRONT MATTER (preface → foreword
  → introduction preference order; `summary_source=front-matter`); the TOC
  listing feeds the tagging call as a topic hint. Full map-reduce of a
  whole book only runs when no front matter is found.
- near-empty docs → marked done with no summary; no LLM call.
- anything else → map-reduce (hierarchical reduce for long documents).

### Scan geometry (`internal/ocrimg` — hand-written, dependency-free)

- `DetectSpine`: open-book two-page spreads are split at the gutter
  (near-white central column flanked by ink; thresholds are page-relative).
- Orientation: tesseract OSD when `osd.traineddata` exists (bundled in
  `tessdata/`); else ink-profile heuristics. **Correctness comes from
  arbitration**: candidate orientations are actually OCR'd and scored
  against a common-word dictionary (`internal/extract/quality.go`) —
  flipped text yields pseudo-words with zero dictionary hits. The cheap
  signals only ORDER candidates (their absolute sign proved unreliable)
  — a wrong hint costs one pass, never accuracy.
- OCR renders gray PGM (mutool `-F pgm`), passed to tesseract directly
  with `--dpi` (PGM headers do not carry resolution).
- Pages whose embedded text layer fails `textLayerSane` (another tool's
  bad OCR: broken spacing, glyph junk) are re-OCR'd from the raster in
  `auto` mode. Force mode (`ExtractOCR`) OCRs every page — the repair
  path for garbled documents; per-page subset = `ExtractOCRPages`
  (accumulates in `ocr_done_pages`).

### LLM call mechanics (internal/summarize + internal/llm)

- Every prompt is token-budgeted against `llm.num_ctx` via the server's
  `/tokenize` (`TrimToTokenBudget`); when no tokenizer is reachable
  (Ollama), it falls back to a character heuristic. Long-doc tagging is
  fed the summaries + opening text, not a raw prefix.
- Context threading: job `context.Context` reaches the HTTP calls
  (`postCtx`), so **cancellation kills the in-flight request**, not just
  the next loop iteration. Cancellation normalizes to
  `summarize.ErrCancelled`; cancelled documents stay `ingested`.

## 5. Operations surface (all of it works today)

- kinds + fast paths + `vellum kind ID [VALUE]` (pins, `none` clears)
- categories: `vellum category ID [VALUE|-]`, auto-filed at processing
  (the tag call proposes one; existing shelves are fed to the prompt to
  consolidate; user-pinned shelves are never re-filed), category chips on
  rows (outside the tree — the group header already says it there),
  shelf tree in the UI (collapsible, nested, ✎ rename).
  Auto-filing LEARNS: user-shelved documents (category_user) are matched
  by keyword overlap to the document being processed and the top-3 fed
  as few-shot examples to the tagging call — corrections propagate
  (see `ShelvingExample`/`bestExamples` in internal/summarize, fetched
  by `userShelvingExamples` in ingest).
- per-field regeneration: `vellum regenerate ID [meta|summary|tags|
  category|kind ...]` — cheap individual rebuilds, no full reprocess
  (meta = one constrained call on the opening text; kind = deterministic
  classify and lifts the pin only when explicitly regenerated).
- remove: `vellum remove ID [ID...]` / `DELETE /api/documents/{id}` —
  index-only removal (chunks + tags + FTS rows + cover cache go; the FILE
  stays on disk). Also a "Remove from library…" button in the UI.
- tags/notes/theme: `GET /api/tags` (cloud) + `GET /api/tags/{tag}`;
  `vellum note list|add|show|delete` + `/api/notes` CRUD; `theme` in
  config (preset + color overrides) drives the UI's CSS variables.
  UI: a Tags page (cloud → [Tag] pages), a Notes management page
  (→ [Note] pages, one textarea, debounced autosave), and a Theme group
  in Settings. Tags/notes are new tables (idempotent CREATE).
- rename category: `vellum rename-category OLD NEW` /
  `POST /api/categories/rename` — renames a shelf everywhere and moves
  its whole subtree (machine-learning → computer-science/machine-learning
  also moves machine-learning/transformers; renaming onto an existing
  shelf merges). ✎ button on shelf headers in the UI tree.
- backup: `vellum export [PATH]` / `GET /api/library/export` downloads a
  consistent snapshot (VACUUM INTO — safe during processing);
  `vellum import PATH` (CLI: stop serve first!) /
  `POST /api/library/import` (loopback-only, jobs-idle-checked, the
  replaced library is kept as `<db>.pre-import-<timestamp>`, server
  closes+reopens its connection) swaps a backup in. "Import/Export…"
  dialog in the UI.
- collections: `vellum collection list|create|delete|add|remove|show` /
  CRUD under `/api/collections[/{id}[/documents[/{docID}]]]` — user-made
  research groups; `GET /api/documents/{id}` also returns the doc's
  `collections`. UI: a Collections root page (create/browse) + a
  `[Collection] <name>` page per collection (add/remove docs), and an
  add/remove control on the Summary page. See internal/db (ListCollections
  et al.) and the collections block in internal/api.
- semantic search uses the embed model's ASYMMETRIC retrieval prefixes:
  `internal/search.embedPrefixes` maps the model name (detected from the
  running server via `/v1/models`, basename-normalized so a versioned pack
  path never changes identity) to nomic ("search_query: "/
  "search_document: ") or EmbeddingGemma ("task: search result | query: "/
  "title: none | text: ") prefixes. Prefixes are part of the
  `meta.embed_model` identity, so adopting them (or swapping models)
  invalidates stored vectors and re-embeds lazily — `vellum embed` does it
  up front.
- collection sharing: `vellum collection export NAME|ID [PATH.zip]` /
  `GET /api/collections/{id}/export` bundle a collection (member files +
  manifest.json with metadata/tags) into a zip; `vellum collection import
  PATH.zip` / `POST /api/collections/import` (loopback-only) extract under
  `<library_dir>/collections/<name>/`, ingest, recreate the collection
  (unique name; --name/?name= override) and restore metadata (originator
  kind/category become user pins). Core: internal/collection.
- kind detection is LEARNABLE: `classify.DetectWithExamples` folds the
  user's `kind_user` pins in as votes (keyword overlap, saturating) —
  one strong example makes an unconfident call confident, overriding a
  confident heuristic takes two+ agreeing pins (ingest.userKindExamples).
- re-extract/repair: `vellum reextract ID [--force-ocr] [--pages 3,7-12]`
- skip: `vellum skip ID [PAGES|-]`
- jobs: every slow op is a Job; FIFO queue; per-job cancel (queued =
  instant, running = cooperative + HTTP abort); `GET /api/jobs`,
  `POST /api/jobs/{id}/cancel`, a Jobs dialog in the UI top bar.
- ask: per-document streaming chat via an EXTERNAL provider (config
  `ask: provider|model|api_key|base_url|tools`; openai-compatible/
  anthropic/ollama adapters in `internal/ask`); UI tab with Test/Save;
  tags stay grammar-bound regardless of what the chat uses. With
  `ask.tools` (default on) the model may call a `fetch_url` (WebFetch)
  tool — `internal/ask/webfetch.go` + the tool loops in providers.go
  (OpenAI-compatible AND Anthropic; Ollama-native ignores tools; ≤4
  rounds; honors HTTPS_PROXY; http(s) only, metadata hosts blocked).
  `{"tool":url}` SSE events; answers render light markdown in the UI.
- settings: `GET/PUT /api/config` — live-applied + persisted to
  config.yaml; "Settings…" dialog in the top bar. (Starting/stopping the
  bundled llama-servers is launcher territory: rerun `./vellum.sh serve`.)
- covers: `GET /api/documents/{id}/cover` (page 1 PNG, lazily cached in
  `<basedir>/covers/`; dropped on re-ingest of a changed file).
- ingest picker: `GET /api/fs?path=` (loopback-only, read-only listing) +
  a filesystem-browser dialog in the UI. Directory ingest is RECURSIVE
  (filepath.WalkDir) and skips hidden entries — pointing it at an Obsidian
  vault indexes the notes and ignores .obsidian/ etc.
- deep links: the UI mirrors the active page into the URL
  (`?doc=`, `&view=preview&page=`, `?tag=`, `?collection=`, `?note=`,
  `?view=tags|notes|collections`, `?q=&semantic=1`); `pageQuery`/`syncURL`/
  `openFromURL` in app.js, a 🔗 copy-link button per page.
- backends: bundled llama.cpp | your own llama.cpp
  (`llm.external: true`) | Ollama (`llm.backend: ollama`; tags remain
  grammar-bound through Ollama's `"format"`). `vellum backends` prints
  the per-channel decision (chat/embed) — the launcher asks the binary
  instead of parsing YAML in shell.

## 6. Invariants (break these and the users' libraries suffer)

1. **The shipped stage's `library.db` is a LIVE user database.** Never
   delete it, never "reset" it, be careful with any script that touches
   `pack/stage/*/`. Tarballs must exclude `library.db*` and `*.log` (they
   do; keep it that way — check with `tar -tzf ... | grep library.db`,
   expect 0). Historical note: this DB was once lost and recovered from
   `/proc/<pid>/fd` of the running server process.
2. **The tag enum is enforced by grammar-constrained decoding** — anything
   touching `internal/summarize`'s schemas, `internal/llm`'s chat path, or
   the Ollama adapter must keep `response_format`/`format` intact.
   Tagging results are VALIDATED against the vocabulary a second time in
   Go, and junk-filtered ("unknown"/hedging dropped) at the boundary.
3. **Additive migrations only** (§3).
4. **No rounded corners in the UI** (explicit user preference). Check with
   `grep -c border-radius internal/api/web/style.css` (0 today).
5. The UI is vanilla JS (`internal/api/web/app.js`), no build step; syntax
   check with `node --check`. The main frame is a horizontal strip of
   PAGES (library + per-document summary/preview/text/ask); page frames
   are built once and only their content is re-rendered, so streaming
   state survives reordering. Page content may be async — the data load
   goes through the per-page token guard in `refreshPage` (a newer
   refresh always wins; there was an `[object Promise]` bug once — page
   content renderers themselves must stay synchronous).
6. LLM-returned metadata is NEVER trusted raw (`cleanMetaValue` +
   validators at every boundary).

## 7. Build, run, test

```sh
go build ./...                  # the binary: cmd/vellum
go test ./internal/...          # unit tests (fast; no external deps)

# e2e — env-driven; starts its OWN llama-servers on EPHEMERAL ports
# (they conflict neither with a running user serve nor each other).
# IMPORTANT: when LLAMA_SERVER_BIN/QWEN_GGUF/NOMIC_GGUF are set, the
# suite ALWAYS starts dedicated ephemeral servers — it never shares the
# user's live 8081/8082 servers (whose load once starved a run past the
# 10m default timeout; explicit env vars mean "isolated run"). Without
# the env vars it still reuses up-servers on the fixed ports (pack /
# entrypoint case), and without either the LLM part skips cleanly.
MUTOOL=/path/to/mutool \
LLAMA_SERVER_BIN=/path/to/llama-server \
QWEN_GGUF=/path/to/qwen3.gguf \
NOMIC_GGUF=/path/to/nomic-embed.gguf \
    go test -timeout 40m ./tests/ -v
```

- Without the env vars the LLM part of the e2e skips cleanly (ingest +
  API tests still run). `tests/testutil` generates PDF fixtures — a
  minimal PDF writer born in tests, including layout pages with
  positioned blocks (for spreads) and image-only scan wrappers whose
  page size follows the image dpi (a landscape image on a portrait page
  = 2:1 squash = garbage OCR — fixed once already; keep it).
- The e2e suite needs ~5-15 min on a free machine. GPU contention with a
  live user session (their own serve running) makes the chat CPU-bound
  and slow — in one 40-minute-timeout run the long-document test
  starved; use the bundled 1.7b model (2× faster) for test runs.

### Pack (portable folder + tarball)

```sh
pack/build.sh              # builds a stage/ dir: binary + llama.cpp + models +
                           # tessdata + vellum.sh + templates + config.yaml
pack/build-mutool.sh       # pins MuPDF from source (no sudo needed)
pack/build-llamacpp.sh     # pins official llama.cpp prebuilts (CPU + Vulkan)
GGUF_DIR=... pack/build.sh # model files
```

Updating the live stage to a new commit (the user's library lives there):

```sh
mv pack/stage/vellum-<old>-linux-amd64 pack/stage/vellum-<new>-linux-amd64
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
    -o pack/stage/vellum-<new>-linux-amd64/vellum ./cmd/vellum
```

If `pack/build.sh`'s `vellum.sh` heredoc changed, regenerate the stage's
copy the same way every build does: the launcher has NO source file of
its own — `pack/build.sh` contains the literal block

    cat > "$STAGE/vellum.sh" <<'EOF'
    ...launcher script...
    EOF
    chmod +x "$STAGE/vellum.sh"

Copy everything between `<<'EOF'` and the terminating `EOF` line into
`pack/stage/vellum-<new>-linux-amd64/vellum.sh` (plus a trailing newline),
then `sh -n` it and `chmod +x`. When only Go files changed, the existing
stage `vellum.sh` is left as-is.

Then the distributable tarball (never contains the user's DB or logs):

```sh
tar --exclude='library.db*' --exclude='*.log' \
    -C pack/stage -czf pack/vellum-<new>-linux-x86_64.tar.gz vellum-<new>-linux-amd64
tar -tzf pack/vellum-<new>-linux-x86_64.tar.gz | grep -c library.db   # expect 0
```

Version string: `versionString` in `cmd/vellum/main.go`. Commit style:
`vX.Y.Z: <one-liner>` with a body bulleting the changes; commits are made
with `-c user.name=vellum -c user.email=vellum@local` (no remote).

Docker: `pack/Dockerfile` + `docker-entrypoint.sh`; Hub is proxy-blocked
(see §8); rebuild with `--build-arg REGISTRY_PREFIX=mirror.gcr.io/`.

## 8. Environment quirks observed on this machine (2026-10)

- **No sudo.** mutool is built from source under `/tmp/opencode/mupdf-src`
  — **/tmp is ephemeral; expect to rebuild it** with
  `pack/build-mutool.sh`. GGUF model files live in
  `/tmp/opencode/vellum-gguf/` — also ephemeral; the SHIPPED copies are in
  the pack's `models/` (permanent) — copy from there if /tmp got wiped.
- Tesseract languages live in the repo `tessdata/` via `TESSDATA_PREFIX`
  (eng, chi_sim, fin + osd); system tessdata lacks eng.
- **Proxy**: all direct egress needs `-x http://127.0.0.1:10081`; Docker
  Hub is blocked by it (use the mirror registry above). The `ask` feature
  hits `api.openai.com`/`api.anthropic.com` from the SERVER process
  (respect the proxy when testing those; env `HTTPS_PROXY`).
- **GPU contention**: the user's own two llama-servers (ports 8081, 8082,
  chat on a 4GB T500 via Vulkan) occupy the GPU. Two chat servers cannot
  share it — a second one silently falls back to CPU (and the embed server
  CRASHES under GPU contention by design — it runs CPU-only, see §2). Run
  the e2e with the 1gb/1.7b model when the user's session is live.
- **Stale llama-servers bind silently**: a leftover llama-server on the
  same port makes clients talk to the WRONG server (old flags/model) with
  no error anywhere. Before runs: `ss -tln | grep -E '808[12]|1808[12]|8090'`
  and kill leftovers (`pgrep -x llama-server`, then by PID; pkill -f
  patterns match your own shell). Test servers use ephemeral ports for
  exactly this reason.
- zsh: no word-splitting of unquoted vars; `rm -f dir/glob*` errors when
  nothing matches (use explicit names); GNU tar applies `-C` to the
  archive argument too when it follows it (confusion observed once —
  write the output path plainly, without `-C` tricks).
- Shell backgrounding trick needed for detached test servers:
  `( cmd > log 2>&1 & )` — a plain `cmd &` dies with the shell.

## 9. Known limitations / open threads (honest list)

- Cancel granularity for running jobs is one unit of work (document,
  section, page, or one in-flight LLM call) — aborts are instant for the
  HTTP call but a step in progress may still complete first (e.g. a page
  OCR finishes; text progress is kept — cancellation is safe, not
  instant).
- CJK-only pages flipped 180° may slip through when OSD is unavailable
  (CJK glyphs are near-symmetric; the English dictionary arbitration
  can't judge them). Latin text is covered.
- The ask chat history is session-local (not persisted). Deliberate
  scope; revisit if the user asks.
- `vellum skip`/`regenerate`/category are UI-first — CLI exists for all;
  batch UI affordances (multi-select regenerate) not built.
- The Docker image is NOT rebuilt for the newest versions (batteries: it
  runs the same pack; rebuild with the mirror registry prefix).
- The kind classifier is heuristic (v0.7-level: full-text + page count +
  front-matter/ISBN/TOC signals + re-classify after OCR). If
  misclassifications in real use become a theme, the agreed long-term
  plan was a small fine-tuned classifier (~0.6B, llama.cpp finetune or
  QLoRA) behind the same `classify` interface, bootstrapped from the
  user's real library (v0.4 discussion; deferred until needed).
- `ask` provider config is persisted as PLAINTEXT in config.yaml
  (`0600`) — fine for a personal machine; do not "fix" silently.

## 10. Where things state-wise

- HEAD: v0.23.0 (`78a1e17`), all tests green. Since 0.23.0: the ask
  WebFetch tool now also works on ANTHROPIC (tool_use/tool_result loop;
  mock-tested), not just OpenAI-compatible providers.
- v0.22.0: fixed the el() boolean-attr bug (disabled:false disabled the
  button); ask answers render light markdown; ask WebFetch tool added
  (ask.tools default on).
- v0.21.2: ask errors surfaced (the real provider message instead of
  "Bad Request"), tolerant base-URL joins (…/v1), disabled-button styling.
- v0.21.1: fixed the broken Ask page (config json tags, PUT echo, config
  reload on render).
- v0.21: the UI is URL-ADDRESSABLE and directory ingest is recursive +
  skips hidden entries.
- v0.20: TAGS page (cloud → [Tag] pages), NOTES (scratchpad table, CRUD,
  CLI, management + [Note] pages), THEMES (config theme → CSS variables).
- v0.19: embed models' asymmetric retrieval prefixes (embedPrefixes;
  basename-normalized identity → lazy re-embed).
- v0.18: notice durations; learnable kind detection; category edits
  refresh collection pages; collection .zip export/import; closable root
  pages; .item-list styling with hover affordance.
- v0.17: COLLECTIONS; CSS-order page moves; bottom status bar.
- v0.16: auto-filing learns from user corrections; flash-free updates.
- Pack stage `pack/stage/vellum-78a1e17-linux-amd64/` (user's live
  library inside) updated to the 0.23.0 binary; distributable tarball
  `pack/vellum-78a1e17-linux-x86_64.tar.gz` (clean of the DB — verified
  with tar -tzf | grep -c library.db = 0). NOTE: the user runs their OWN
  serve(s) and restarts them freely — an old one (PID 701, port 8097) is
  long-running; a newer one from the stage may appear (its exe shows
  "(deleted)" after the version-dir rename). Do NOT kill them; each
  restart picks up the current stage binary.
- The user runs `./vellum.sh serve` (their own llama-servers on 8081/8082
  are LONG-RUNNING — do not kill them; kill only ephemeral test ones).
- `AGENTS.md` (repo root) carries the agent-focused subset of this
  document; keep the two in sync when you change invariants.
- `README.md` is user-facing (usage + limitations) — update the feature
  sections there with anything user-visible.