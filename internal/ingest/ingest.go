// Package ingest indexes files (text extraction + OCR fallback) and runs the
// LLM processing pass (summarize + constrained tagging). Results are
// structured (per-file, per-document) so the CLI and the web API can both
// report them — to humans and to agents.
package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"vellum/internal/classify"
	"vellum/internal/config"
	"vellum/internal/db"
	"vellum/internal/extract"
	"vellum/internal/summarize"
	"vellum/internal/vocab"
)

// yearRE: a credible publication year is a 4-digit number, optionally
// followed by a second one ("1998", "2010-2012").
var yearRE = regexp.MustCompile(`^\d{4}(-\d{4})?$`)

// FileResult is the outcome for one ingested file.
type FileResult struct {
	Path   string `json:"path"`
	Action string `json:"action"` // added | updated | skipped | failed
	Error  string `json:"error,omitempty"`
}

// Stats summarizes an ingest run.
type Stats struct {
	Added, Updated, Skipped, Failed int
	Cancelled                       bool         `json:"cancelled,omitempty"`
	Files                           []FileResult `json:"files"`
}

func (st *Stats) count(action string) {
	switch action {
	case "added":
		st.Added++
	case "updated":
		st.Updated++
	case "skipped":
		st.Skipped++
	case "failed":
		st.Failed++
	}
}

// ProcessResult is the outcome for one processed document.
type ProcessResult struct {
	DocID     int64    `json:"id"`
	Path      string   `json:"path"`
	Status    string   `json:"status"` // done | error
	Tags      []string `json:"tags,omitempty"`
	TagsOther []string `json:"tags_other,omitempty"`
	Category  string   `json:"category,omitempty"` // auto-filed or suggested
	Error     string   `json:"error,omitempty"`
}

// Ingest walks the paths and indexes every supported file, with
// sha256-based change detection. Ingest is QUICK: it reads text layers
// only — OCR work on raster-heavy pages is deferred to the processing
// phase (documents land with ocr_pending=1). progress (may be nil)
// receives per-file updates for UI feedback.
func Ingest(ctx context.Context, cfg *config.Config, conn *sql.DB, paths []string,
	reprocess bool, progress func(string)) (*Stats, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	st := &Stats{Files: []FileResult{}}
	files, inaccessible := collectFiles(paths)
	for _, f := range inaccessible {
		st.Files = append(st.Files, f)
		st.count("failed")
	}
	cancelled := false
	for i, path := range files {
		if err := orCtxIn(ctx).Err(); err != nil {
			cancelled = true
			break
		}
		if progress != nil {
			progress(fmt.Sprintf("ingesting %d/%d: %s",
				i+1, len(files), filepath.Base(path)))
		}
		action, err := ingestOne(cfg, conn, path, reprocess)
		if err != nil {
			log.Printf("failed to ingest %s: %s", path, err)
			action = "failed"
			st.Files = append(st.Files, FileResult{Path: path, Action: action, Error: err.Error()})
		} else {
			st.Files = append(st.Files, FileResult{Path: path, Action: action})
		}
		st.count(action)
	}
	st.Cancelled = cancelled
	return st, nil
}

// collectFiles walks the requested paths and returns the supported files
// beneath them. A path that cannot be accessed (missing, permission denied)
// is returned as a failed FileResult rather than silently skipped, so an
// ingest that would otherwise "succeed" over zero files surfaces the problem
// — both the CLI and the web UI render st.Files.
func collectFiles(paths []string) (files []string, failed []FileResult) {
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			log.Printf("ingest: cannot access %s: %s", p, err)
			failed = append(failed, FileResult{Path: p, Action: "failed", Error: err.Error()})
			continue
		}
		if !st.IsDir() {
			files = append(files, p)
			continue
		}
		// Recursive: a directory contributes every supported file beneath it,
		// at any depth (this is what makes ingesting an Obsidian vault — or
		// any notes folder — a one-liner). Hidden entries are skipped: a
		// vault's .obsidian/ (config + plugins), .trash/, and .git/ are not
		// library material.
		root := p
		filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				// An unreadable directory (or a file that vanished mid-walk):
				// record it and keep going so one bad subtree doesn't abort the
				// run, but the caller still learns about it.
				log.Printf("ingest: cannot read %s: %s", path, err)
				failed = append(failed, FileResult{Path: path, Action: "failed", Error: err.Error()})
				return nil
			}
			name := d.Name()
			if d.IsDir() {
				if path != root && strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(name, ".") {
				return nil
			}
			if extract.Supported(path) {
				files = append(files, path)
			}
			return nil
		})
	}
	sort.Strings(files)
	return files, failed
}

func sha256file(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func ingestOne(cfg *config.Config, conn *sql.DB, path string, reprocess bool) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "failed", err
	}
	digest, err := sha256file(abs)
	if err != nil {
		return "failed", err
	}

	var docID int64
	var oldDigest string
	err = conn.QueryRow("SELECT id, sha256 FROM documents WHERE path=?", abs).
		Scan(&docID, &oldDigest)
	known := err == nil
	if known && oldDigest == digest && !reprocess {
		return "skipped", nil
	}

	t0 := time.Now()
	// text layer only: ingest never OCRs (tesseract on a rastered book
	// takes minutes and made ingest look hung). Pages that look thin
	// or garbled set ocr_pending; the processing phase does the OCR
	// with live progress, then re-classifies on the better text.
	res, err := extract.ExtractText(abs, cfg)
	if err != nil {
		return "failed", err
	}
	hasText := res.NeedsOCR // raster-heavy documents ingest with thin text
	for _, c := range res.Chunks {
		if c.Text != "" {
			hasText = true
			break
		}
	}
	if !hasText {
		return "failed", fmt.Errorf("no text extracted and nothing to OCR")
	}

	pending := 0
	if res.NeedsOCR {
		pending = 1
	}

	action := "updated"
	if known {
		if _, err := conn.Exec(
			"UPDATE documents SET sha256=?, status='ingested', error=NULL, processed_at=NULL, ocr_pending=?, n_pages=? WHERE id=?",
			digest, pending, len(res.Chunks), docID); err != nil {
			return "failed", err
		}
	} else {
		result, err := conn.Exec(
			"INSERT INTO documents(path, sha256, title, authors, year, ocr_pages, n_pages, ocr_pending, status) VALUES(?,?,?,?,?,?,?,?,'ingested')",
			abs, digest, res.Title, res.Authors, "", 0, len(res.Chunks), pending)
		if err != nil {
			return "failed", err
		}
		docID, err = result.LastInsertId()
		if err != nil {
			return "failed", err
		}
		action = "added"
	}

	if err := db.ReplaceDocumentText(conn, docID, res.Chunks); err != nil {
		return "failed", err
	}

	// kind detection is instant and deterministic: structural heuristics
	// over the extracted text (paper/book/gallery/course/reference).
	// Processing re-classifies after OCR when this text was thin; users
	// can override any time via `vellum kind`, the API, or the UI.
	kind, _ := classify.Detect(strings.Join(chunkTexts(res.Chunks), "\n\n"),
		0, len(res.Chunks))
	kindSuffix := ""
	if kind != "" {
		kindSuffix = " [" + kind + "]"
	}
	if _, err := conn.Exec(
		"UPDATE documents SET kind=? WHERE id=?", kind, docID); err != nil {
		return "failed", err
	}

	ocrNote := ""
	if pending == 1 {
		ocrNote = " — OCR deferred to processing"
	}
	log.Printf("%s: %d chunks in %.1fs%s%s",
		filepath.Base(abs), len(res.Chunks), time.Since(t0).Seconds(),
		kindSuffix, ocrNote)
	return action, nil
}

func chunkTexts(chunks []db.Chunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.Text
	}
	return out
}

// ProcessPending summarizes and tags documents. With ids empty it takes all
// pending ('ingested') documents, limited by limit (0 = no limit); with ids
// it processes exactly those, whatever their status (a 'done' document gets
// a fresh summary and tags).
// ProcessPending summarizes and tags documents. progress (may be nil)
// receives live updates. With ids empty it takes all pending
// ('ingested') documents, limited by limit (0 = no limit); with ids it
// processes exactly those, whatever their status.
func ProcessPending(ctx context.Context, cfg *config.Config, conn *sql.DB, v *vocab.Vocabulary,
	ids []int64, limit int, progress func(string),
) ([]ProcessResult, error) {
	ctx = orCtxIn(ctx)
	if err := ctxErrIn(ctx); err != nil {
		return nil, err
	}
	type docRow struct {
		id                               int64
		path, title, authors, year, kind string
		ocrPending, kindUser             int64
	}
	var docs []docRow

	if len(ids) > 0 {
		for _, id := range ids {
			var d docRow
			err := conn.QueryRow(
				"SELECT id, path, title, authors, year, kind, ocr_pending, kind_user FROM documents WHERE id=?", id).
				Scan(&d.id, &d.path, &d.title, &d.authors, &d.year, &d.kind,
					&d.ocrPending, &d.kindUser)
			if err == sql.ErrNoRows {
				log.Printf("process: no document #%d", id)
				continue
			}
			if err != nil {
				return nil, err
			}
			docs = append(docs, d)
		}
	} else {
		q := "SELECT id, path, title, authors, year, kind, ocr_pending, kind_user FROM documents WHERE status='ingested' ORDER BY id"
		if limit > 0 {
			q += fmt.Sprintf(" LIMIT %d", limit)
		}
		rows, err := conn.Query(q)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var d docRow
			if err := rows.Scan(&d.id, &d.path, &d.title, &d.authors, &d.year, &d.kind,
				&d.ocrPending, &d.kindUser); err != nil {
				rows.Close()
				return nil, err
			}
			docs = append(docs, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	results := []ProcessResult{}
	for i, d := range docs {
		if err := ctxErrIn(ctx); err != nil {
			progress("cancelled")
			break
		}
		if progress != nil {
			progress(fmt.Sprintf("document %d/%d: %s", i+1, len(docs), filepath.Base(d.path)))
		}
		log.Printf("processing %s", d.path)
		res, err := processOne(ctx, cfg, conn, v, d.id, d.title, d.authors, d.year,
			d.path, d.kind, progress)
		if err != nil {
			if errors.Is(err, summarize.ErrCancelled) || orCtxIn(ctx).Err() != nil {
				// stop: the current document keeps status='ingested'
				// (pending) and is simply tried again next time
				progress("cancelled")
				break
			}
			log.Printf("processing failed for %s: %s", d.path, err)
			conn.Exec("UPDATE documents SET status='error', error=? WHERE id=?",
				err.Error(), d.id)
			results = append(results, ProcessResult{
				DocID: d.id, Path: d.path, Status: "error", Error: err.Error()})
			continue
		}
		results = append(results, ProcessResult{
			DocID: d.id, Path: d.path, Status: "done",
			Tags: res.Tags, TagsOther: res.TagsOther, Category: res.Category})
	}
	return results, nil
}

func processOne(ctx context.Context, cfg *config.Config, conn *sql.DB, v *vocab.Vocabulary,
	docID int64, title, authors, year, path, kind string,
	progress func(string),
) (*summarize.TagResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if progress == nil {
		progress = func(string) {}
	}

	// ---- deferred OCR: ingest only read the text layer; documents
	// marked ocr_pending (thin pages, or a garbled embedded layer)
	// get the full raster treatment here, with live progress. This
	// can take minutes for a big scanned book — that is expected.
	var ocrPending, kindUser int64
	if err := conn.QueryRow(
		"SELECT ocr_pending, kind_user FROM documents WHERE id=?", docID).
		Scan(&ocrPending, &kindUser); err != nil {
		return nil, err
	}
	if ocrPending == 1 {
		progress("extracting text (OCR on raster pages — can take minutes)")
		t0 := time.Now()
		res, err := extract.Extract(path, cfg)
		if err != nil {
			return nil, fmt.Errorf("OCR extraction failed: %w", err)
		}
		if err := db.ReplaceDocumentText(conn, docID, res.Chunks); err != nil {
			return nil, err
		}
		if res.Title != "" && title == "" {
			title = res.Title
			conn.Exec("UPDATE documents SET title=? WHERE id=? AND (title IS NULL OR title='')",
				res.Title, docID)
		}
		if res.Authors != "" && authors == "" {
			authors = res.Authors
			conn.Exec("UPDATE documents SET authors=? WHERE id=? AND (authors IS NULL OR authors='')",
				res.Authors, docID)
		}
		if _, err := conn.Exec(
			"UPDATE documents SET ocr_pending=0, ocr_pages=?, n_pages=? WHERE id=?",
			res.OCRPages, len(res.Chunks), docID); err != nil {
			return nil, err
		}
		log.Printf("%s: OCR replaced text on %d pages in %.1fs",
			filepath.Base(path), res.OCRPages, time.Since(t0).Seconds())
	}

	text, err := db.DocumentText(conn, docID)
	if err != nil {
		return nil, err
	}

	existingCategories, err := db.ExistingCategories(conn)
	if err != nil {
		return nil, err
	}
	shelvingExamples, err := userShelvingExamples(conn, docID)
	if err != nil {
		return nil, err
	}
	kindExamples, err := userKindExamples(conn, docID)
	if err != nil {
		return nil, err
	}

	// ---- re-classify on the current text unless the user set the kind:
	// ingest classified from the text layer alone; OCR text (chapters,
	// TOC, preface) flips borderline calls, and classifier improvements
	// reach old libraries at processing time.
	if kindUser == 0 {
		var ocrPages, nPages int64
		if err := conn.QueryRow(
			"SELECT ocr_pages, n_pages FROM documents WHERE id=?", docID).
			Scan(&ocrPages, &nPages); err == nil {
			newKind, _ := classify.DetectWithExamples(text, int(ocrPages), int(nPages), kindExamples)
			if newKind != kind {
				kind = newKind
				if _, err := conn.Exec(
					"UPDATE documents SET kind=? WHERE id=?", kind, docID); err != nil {
					return nil, err
				}
				if kind != "" {
					progress("kind classified as " + kind)
				}
			}
		}
	}

	// ---- fast paths keyed on the detected kind -------------------------

	// near-empty text (galleries, image-only scans): nothing to summarize
	if len(strings.TrimSpace(text)) < 400 {
		if _, err := conn.Exec(
			"UPDATE documents SET summary='', summary_source='', status='done', processed_at=datetime('now') WHERE id=?",
			docID); err != nil {
			return nil, err
		}
		log.Printf("done: no text to summarize (kind=%q)", kind)
		return &summarize.TagResult{}, nil
	}

	// papers: extract the abstract instead of generating a summary —
	// the authors' own words, at zero LLM cost for the summary itself
	if kind == "paper" {
		report := progress
		if report == nil {
			report = func(string) {}
		}
		report("extracting abstract")
		abstract := classify.ExtractAbstract(text)
		if abstract != "" {
			report("choosing tags (from abstract)")
			// tag from the abstract + the opening text (title page)
			opening := text
			if len(opening) > 3000 {
				opening = opening[:3000]
			}
			tags, err := summarize.TagDocumentWithCategories(ctx, cfg, v, existingCategories,
				shelvingExamples, []string{abstract, opening}, nil, abstract, progress)
			if err != nil {
				return nil, err
			}
			if err := storeProcessed(conn, docID, title, authors, year, tags,
				abstract, "abstract"); err != nil {
				return nil, err
			}
			log.Printf("done: extracted abstract (%d chars) — tags: %s",
				len(abstract), strings.Join(tags.Tags, ", "))
			return tags, nil
		}
		// no abstract found: fall through to the generic path
	}

	// books: the author's own front matter (preface / foreword /
	// introduction) IS the summary — summarizing an entire book with
	// map-reduce would be hours of LLM time for a worse result. The
	// contents listing feeds the tagging call as a topic hint.
	if kind == "book" {
		if report := progress; true {
			if report == nil {
				report = func(string) {}
			}
			report("extracting front matter")
			if front := classify.ExtractFrontMatter(text); front != "" {
				report("choosing tags (from front matter + contents)")
				tagInput := []string{front}
				if toc := classify.ExtractTOC(text); toc != "" {
					tagInput = append(tagInput, "Contents:\n"+toc)
				}
				tags, terr := summarize.TagDocumentWithCategories(ctx, cfg, v,
					existingCategories, shelvingExamples, tagInput, nil, front, progress)
				if terr != nil {
					return nil, terr
				}
				if serr := storeProcessed(conn, docID, title, authors, year, tags,
					front, "front-matter"); serr != nil {
					return nil, serr
				}
				log.Printf("done: extracted front matter (%d chars) — tags: %s",
					len(front), strings.Join(tags.Tags, ", "))
				return tags, nil
			}
		}
		// no front matter found: fall through to the generic path
	}

	// ---- generic path: map-reduce over the whole document --------------
	// one map phase feeds both the summary and the tagging call —
	// long documents are tagged from summaries + opening text, not by
	// re-sending a huge raw-text prefix
	produced, err := produceSummary(ctx, cfg, kind, text, progress)
	if err != nil {
		if errors.Is(err, summarize.ErrCancelled) {
			return nil, err
		}
		return nil, err
	}
	summary := produced.Summary
	tags, err := summarize.TagDocumentWithCategories(ctx, cfg, v, existingCategories,
		shelvingExamples, produced.TagChunks, produced.TagSummaries, summary, progress)
	if err != nil {
		return nil, err
	}

	if err := storeProcessed(conn, docID, title, authors, year, tags,
		summary, produced.Source); err != nil {
		return nil, err
	}
	log.Printf("done: tags: %s%s", strings.Join(tags.Tags, ", "),
		func() string {
			if len(tags.TagsOther) > 0 {
				return " | suggested: " + strings.Join(tags.TagsOther, ", ")
			}
			return ""
		}())
	return tags, nil
}

// cleanMetaValue drops junk placeholder values ("unknown", LLM hedging like
// "not specified") — they are worse than nothing.
// userShelvingExamples returns the documents the user shelved personally
// (category_user — every manual correction or pin): the tagger feeds the
// most similar ones back as few-shot guidance, so corrections teach
// future auto-filing. The document being processed is excluded — it must
// not become its own example (a regeneration would just echo its shelf).
func userShelvingExamples(conn *sql.DB, excludeDocID int64) ([]summarize.ShelvingExample, error) {
	rows, err := conn.Query(
		`SELECT title, authors, category FROM documents
		 WHERE category_user=1 AND category != '' AND title != '' AND id != ?`,
		excludeDocID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []summarize.ShelvingExample{}
	for rows.Next() {
		var ex summarize.ShelvingExample
		var authors sql.NullString
		if err := rows.Scan(&ex.Title, &authors, &ex.Category); err != nil {
			return nil, err
		}
		ex.Authors = authors.String
		out = append(out, ex)
	}
	return out, rows.Err()
}

// userKindExamples returns the documents the user classified personally
// (kind_user): their kind votes on alike documents at classification
// time, so kind corrections are learned. A short sample (opening chunk)
// is matched; the list is capped so classification stays cheap.
func userKindExamples(conn *sql.DB, excludeDocID int64) ([]classify.Example, error) {
	rows, err := conn.Query(`
		SELECT d.kind, COALESCE(d.title, ''),
		       COALESCE((SELECT c.text FROM chunks c
		                 WHERE c.doc_id = d.id ORDER BY c.seq LIMIT 1), '')
		FROM documents d
		WHERE d.kind_user = 1 AND d.kind != '' AND d.id != ?
		ORDER BY d.id DESC LIMIT 300`, excludeDocID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []classify.Example{}
	for rows.Next() {
		var ex classify.Example
		if err := rows.Scan(&ex.Kind, &ex.Title, &ex.Text); err != nil {
			return nil, err
		}
		if len(ex.Text) > 4000 {
			ex.Text = ex.Text[:4000]
		}
		out = append(out, ex)
	}
	return out, rows.Err()
}

// cleanMetaValue drops junk placeholder values ("unknown", LLM hedging like
// "not specified") — they are worse than nothing.
func cleanMetaValue(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "unknown", "untitled", "unspecified", "anonymous", "none",
		"n/a", "na", "null", "not specified", "not available":
		return ""
	}
	return strings.TrimSpace(s)
}

// storeProcessed writes the outcome of a successful processing pass.
func storeProcessed(conn *sql.DB, docID int64, title, authors, year string,
	tags *summarize.TagResult, summary, summarySource string) error {
	newTitle := title
	if newTitle == "" {
		newTitle = tags.Title
	}
	newAuthors := authors
	if newAuthors == "" {
		newAuthors = strings.Join(tags.Authors, ", ")
	}
	newYear := year
	if newYear == "" {
		newYear = tags.Year
	}
	// never trust an LLM (or a PDF producer) fully: junk stays out of the index
	newTitle = cleanMetaValue(newTitle)
	newAuthors = cleanMetaValue(newAuthors)
	if !yearRE.MatchString(newYear) {
		newYear = ""
	}
	// auto-categorization: the model's category suggestion files the
	// document into a shelf — but ONLY when the user has not set one
	// themselves (category_user) or already picked a category
	if tags.Category != "" {
		conn.Exec(
			"UPDATE documents SET category=? WHERE id=? AND category_user=0 AND (category IS NULL OR category='')",
			tags.Category, docID)
	}
	if _, err := conn.Exec(
		"UPDATE documents SET summary=?, summary_source=?, title=?, authors=?, year=?, status='done', processed_at=datetime('now') WHERE id=?",
		summary, summarySource, newTitle, newAuthors, newYear, docID); err != nil {
		return err
	}
	pairs := make([][2]string, 0, len(tags.Tags)+len(tags.TagsOther))
	for _, t := range tags.Tags {
		pairs = append(pairs, [2]string{t, "vocab"})
	}
	for _, t := range tags.TagsOther {
		pairs = append(pairs, [2]string{t, "suggested"})
	}
	return db.SetTags(conn, docID, pairs)
}

// orCtxIn tolerates a nil context (internal callers).
func orCtxIn(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// ctxErrIn reports ErrCancelled when the context is done.
func ctxErrIn(ctx context.Context) error {
	if err := orCtxIn(ctx).Err(); err != nil {
		return ErrCancelled
	}
	return nil
}

// ErrCancelled is ingest's sentinel when a job context was cancelled
// mid-run (alias of summarize.ErrCancelled).
var ErrCancelled = summarize.ErrCancelled

// produced carries the summary and the inputs the tagging call wants.
type produced struct {
	Summary      string
	Source       string   // abstract | front-matter | generated
	TagChunks    []string // opening pieces for the tagging call
	TagSummaries []string // section summaries for the tagging call
}

// produceSummary generates a document's summary using the kind's fast
// path where possible (extracted abstract / front matter), else the
// map-reduce. Shared by full processing and per-field regeneration.
func produceSummary(ctx context.Context, cfg *config.Config, kind, text string,
	progress func(string)) (*produced, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if progress == nil {
		progress = func(string) {}
	}
	opening := text
	if len(opening) > 6000 {
		opening = opening[:6000]
	}
	if kind == "paper" {
		if abstract := classify.ExtractAbstract(text); abstract != "" {
			return &produced{Summary: abstract, Source: "abstract",
				TagChunks: []string{abstract, opening}}, nil
		}
	}
	if kind == "book" {
		if front := classify.ExtractFrontMatter(text); front != "" {
			tagInput := []string{front}
			if toc := classify.ExtractTOC(text); toc != "" {
				tagInput = append(tagInput, "Contents:\n"+toc)
			}
			return &produced{Summary: front, Source: "front-matter",
				TagChunks: tagInput}, nil
		}
	}
	summaries, chunks, err := summarize.MapSummaries(ctx, cfg, text, progress)
	if err != nil {
		return nil, err
	}
	summary, err := summarize.SummarizeFrom(ctx, cfg, chunks, summaries, progress)
	if err != nil {
		return nil, err
	}
	return &produced{Summary: summary, Source: "generated",
		TagChunks: chunks, TagSummaries: summaries}, nil
}

// Regenerate rebuilds INDIVIDUAL metadata fields in place:
//
//	"meta"     title/authors/year via one cheap constrained call
//	"summary"  the kind's fast path or map-reduce; source updated
//	"tags"     re-tagging from the stored summary + opening text
//	"category" model-filed like tags, then pinned-off (user asked for it)
//	"kind"     deterministic reclassify on the current text (no LLM)
//
// Unknown fields are ignored; returns the applied field names.
func Regenerate(ctx context.Context, cfg *config.Config, conn *sql.DB, v *vocab.Vocabulary,
	docID int64, fields []string, progress func(string),
) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if progress == nil {
		progress = func(string) {}
	}
	var path, title, authors, year, summary, kind string
	var kindUser, categoryUser int64
	var ocrPages, nPages int64
	var nTitle, nAuthors, nYear, nSummary, nKind sql.NullString
	err := conn.QueryRow(
		"SELECT path, title, authors, year, summary, kind, kind_user, category_user, ocr_pages, n_pages FROM documents WHERE id=?",
		docID).Scan(&path, &nTitle, &nAuthors, &nYear, &nSummary, &nKind,
		&kindUser, &categoryUser, &ocrPages, &nPages)
	if err != nil {
		return nil, err
	}
	title, authors, year, summary, kind =
		nTitle.String, nAuthors.String, nYear.String, nSummary.String, nKind.String
	text, err := db.DocumentText(conn, docID)
	if err != nil {
		return nil, err
	}
	opening := text
	if len(opening) > 8000 {
		opening = opening[:8000]
	}
	want := map[string]bool{}
	for _, f := range fields {
		want[f] = true
	}
	applied := []string{}

	// full tag call reuse: tags/category/meta can share one call
	fullTagCall := want["tags"] || (want["category"] && len(text) > 0)
	if want["meta"] && !want["tags"] {
		progress("re-generating title/authors/year")
		mr, err := summarize.RegenMeta(ctx, cfg, opening)
		if err != nil {
			return applied, err
		}
		newTitle := cleanMetaValue(mr.Title)
		newAuthors := cleanMetaValue(strings.Join(mr.Authors, ", "))
		newYear := ""
		if yearRE.MatchString(mr.Year) {
			newYear = mr.Year
		}
		// store whichever came back sanely (empty result = keep old)
		if newTitle != "" {
			title = newTitle
		}
		if newAuthors != "" {
			authors = newAuthors
		}
		if newYear != "" {
			year = newYear
		}
		if _, err := conn.Exec(
			"UPDATE documents SET title=?, authors=?, year=? WHERE id=?",
			title, authors, year, docID); err != nil {
			return applied, err
		}
		progress("metadata updated")
		applied = append(applied, "meta")
	}

	if want["summary"] {
		progress("re-generating summary")
		produced, err := produceSummary(ctx, cfg, kind, text, progress)
		if err != nil {
			return applied, err
		}
		if _, err := conn.Exec(
			"UPDATE documents SET summary=?, summary_source=? WHERE id=?",
			produced.Summary, produced.Source, docID); err != nil {
			return applied, err
		}
		summary = produced.Summary
		progress("summary updated")
		applied = append(applied, "summary")
	}

	if want["kind"] {
		kindEx, _ := userKindExamples(conn, docID)
		newKind, _ := classify.DetectWithExamples(text, int(ocrPages), int(nPages), kindEx)
		if _, err := conn.Exec(
			"UPDATE documents SET kind=?, kind_user=0 WHERE id=?",
			newKind, docID); err != nil {
			return applied, err
		}
		progress("kind re-detected: " + or_(newKind))
		applied = append(applied, "kind")
	}

	if fullTagCall {
		if want["tags"] {
			progress("re-generating tags")
		}
		if want["category"] {
			progress("re-generating category")
		}
		existingCategories, err := db.ExistingCategories(conn)
		if err != nil {
			return applied, err
		}
		shelvingExamples, err := userShelvingExamples(conn, docID)
		if err != nil {
			return applied, err
		}
		tagSummaries := []string{}
		tagChunks := []string{opening}
		if summary != "" {
			tagSummaries = []string{summary}
		}
		if err := ctxErrIn(ctx); err != nil {
			return applied, err
		}
		tr, err := summarize.TagDocumentWithCategories(ctx, cfg, v, existingCategories,
			shelvingExamples, tagChunks, tagSummaries, summary, progress)
		if err != nil {
			return applied, err
		}
		if want["tags"] {
			pairs := make([][2]string, 0, len(tr.Tags)+len(tr.TagsOther))
			for _, t := range tr.Tags {
				pairs = append(pairs, [2]string{t, "vocab"})
			}
			for _, t := range tr.TagsOther {
				pairs = append(pairs, [2]string{t, "suggested"})
			}
			if err := db.SetTags(conn, docID, pairs); err != nil {
				return applied, err
			}
			progress("tags updated")
			applied = append(applied, "tags")
		}
		if want["category"] && tr.Category != "" {
			// the user explicitly asked to re-gen: their pin lifts
			if _, err := conn.Exec(
				"UPDATE documents SET category=?, category_user=0 WHERE id=?",
				tr.Category, docID); err != nil {
				return applied, err
			}
			progress("category updated: " + tr.Category)
			applied = append(applied, "category")
		}
		// the same call returns metadata: fill EMPTY fields for free
		// (never clobber good ones)
		gapTitle, gapAuthors := "", ""
		gapYear := ""
		if cleanMetaValue(tr.Title) != "" && cleanMetaValue(title) == "" {
			gapTitle = cleanMetaValue(tr.Title)
		}
		if cleanMetaValue(strings.Join(tr.Authors, ", ")) != "" && cleanMetaValue(authors) == "" {
			gapAuthors = cleanMetaValue(strings.Join(tr.Authors, ", "))
		}
		if yearRE.MatchString(tr.Year) && cleanMetaValue(year) == "" {
			gapYear = tr.Year
		}
		if gapTitle != "" || gapAuthors != "" || gapYear != "" {
			conn.Exec(
				"UPDATE documents SET title=CASE WHEN ?!='' THEN ? ELSE title END, authors=CASE WHEN ?!='' THEN ? ELSE authors END, year=CASE WHEN ?!='' THEN ? ELSE year END WHERE id=?",
				gapTitle, gapTitle, gapAuthors, gapAuthors, gapYear, gapYear, docID)
		}
	}
	return applied, nil
}

// EnrichStages is the order the filesystem watcher applies to a freshly
// indexed document: the cheap/deterministic work first, the costly
// summary LAST:
//
//	ingest   finish the deferred OCR (only when ingest flagged ocr_pending)
//	kind     deterministic re-detect on the current text
//	category the tagging call's category, applied first
//	meta     …then title/authors/year (same call)
//	tags     …then tags (same call)
//	summary  the kind's fast path or the map-reduce pass
var EnrichStages = []string{"ingest", "kind", "category", "meta", "tags", "summary"}

// Enrich auto-enriches one already-indexed document in the watcher's stage
// order (see EnrichStages) and marks it done. It is the one-shot companion
// to Ingest: Ingest indexes text-layer-only and cheaply, Enrich finishes
// the deferred OCR and then runs the metadata passes. Unlike
// ProcessPending, the summary is generated LAST, so the single
// grammar-constrained tagging call (which yields category, metadata and
// tags together) is applied as three ordered stages and derives tags from
// the surviving text rather than from a summary that does not exist yet.
//
// A document whose enrichment fails stays status='ingested', so the next
// watcher scan retries it.
func Enrich(ctx context.Context, cfg *config.Config, conn *sql.DB, v *vocab.Vocabulary,
	docID int64, progress func(string)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if progress == nil {
		progress = func(string) {}
	}
	var path, kind string
	var title, authors, year, nKind sql.NullString
	var ocrPending, kindUser, ocrPages, nPages int64
	if err := conn.QueryRow(
		"SELECT path, title, authors, year, kind, kind_user, ocr_pending, ocr_pages, n_pages FROM documents WHERE id=?",
		docID).Scan(&path, &title, &authors, &year, &nKind,
		&kindUser, &ocrPending, &ocrPages, &nPages); err != nil {
		return err
	}
	kind = nKind.String
	t, a, y := title.String, authors.String, year.String

	// ---- ingest: finish what ingest deferred. ingest is quick by design
	// and never OCRs; raster-heavy pages land flagged ocr_pending.
	if ocrPending == 1 {
		progress("extracting text (OCR on raster pages — can take minutes)")
		res, err := extract.Extract(path, cfg)
		if err != nil {
			return fmt.Errorf("OCR extraction failed: %w", err)
		}
		if err := db.ReplaceDocumentText(conn, docID, res.Chunks); err != nil {
			return err
		}
		if res.Title != "" && t == "" {
			t = res.Title
		}
		if res.Authors != "" && a == "" {
			a = res.Authors
		}
		if _, err := conn.Exec(
			"UPDATE documents SET title=?, authors=?, ocr_pending=0, ocr_pages=?, n_pages=? WHERE id=?",
			t, a, res.OCRPages, len(res.Chunks), docID); err != nil {
			return err
		}
		ocrPages, nPages = int64(res.OCRPages), int64(len(res.Chunks))
	}

	text, err := db.DocumentText(conn, docID)
	if err != nil {
		return err
	}
	if err := ctxErrIn(ctx); err != nil {
		return err
	}

	// ---- kind: deterministic re-detect on the (possibly OCR-fresh) text.
	// A user's explicit pick (kind_user) is never overridden.
	if kindUser == 0 {
		kindEx, _ := userKindExamples(conn, docID)
		newKind, _ := classify.DetectWithExamples(text, int(ocrPages), int(nPages), kindEx)
		if newKind != kind {
			kind = newKind
			if _, err := conn.Exec(
				"UPDATE documents SET kind=? WHERE id=?", kind, docID); err != nil {
				return err
			}
		}
		progress("kind: " + or_(kind))
	}

	// near-empty text (galleries, image-only scans): nothing to enrich
	if len(strings.TrimSpace(text)) < 400 {
		_, err := conn.Exec(
			"UPDATE documents SET summary='', summary_source='', status='done', error=NULL, processed_at=datetime('now') WHERE id=?",
			docID)
		return err
	}

	// ---- one grammar-constrained tagging call feeds three ordered stages:
	// category, then title/author/year, then tags. No summary yet by design.
	opening := text
	if len(opening) > 8000 {
		opening = opening[:8000]
	}
	existingCategories, err := db.ExistingCategories(conn)
	if err != nil {
		return err
	}
	shelvingExamples, err := userShelvingExamples(conn, docID)
	if err != nil {
		return err
	}
	if err := ctxErrIn(ctx); err != nil {
		return err
	}
	progress("filing category")
	tr, err := summarize.TagDocumentWithCategories(ctx, cfg, v, existingCategories,
		shelvingExamples, []string{opening}, nil, "", progress)
	if err != nil {
		return err
	}
	if tr.Category != "" {
		if _, err := conn.Exec(
			"UPDATE documents SET category=?, category_user=0 WHERE id=?",
			tr.Category, docID); err != nil {
			return err
		}
		progress("category: " + tr.Category)
	}
	if newTitle := cleanMetaValue(tr.Title); newTitle != "" {
		t = newTitle
	}
	if newAuthors := cleanMetaValue(strings.Join(tr.Authors, ", ")); newAuthors != "" {
		a = newAuthors
	}
	if yearRE.MatchString(tr.Year) {
		y = tr.Year
	}
	if _, err := conn.Exec(
		"UPDATE documents SET title=?, authors=?, year=? WHERE id=?",
		t, a, y, docID); err != nil {
		return err
	}
	progress("metadata updated")
	pairs := make([][2]string, 0, len(tr.Tags)+len(tr.TagsOther))
	for _, tg := range tr.Tags {
		pairs = append(pairs, [2]string{tg, "vocab"})
	}
	for _, tg := range tr.TagsOther {
		pairs = append(pairs, [2]string{tg, "suggested"})
	}
	if err := db.SetTags(conn, docID, pairs); err != nil {
		return err
	}
	progress("tags updated")

	// ---- summary LAST: the kind's fast path (abstract / front matter)
	// where possible, else map-reduce.
	produced, err := produceSummary(ctx, cfg, kind, text, progress)
	if err != nil {
		return err
	}
	if _, err := conn.Exec(
		"UPDATE documents SET summary=?, summary_source=?, status='done', error=NULL, processed_at=datetime('now') WHERE id=?",
		produced.Summary, produced.Source, docID); err != nil {
		return err
	}
	progress("summary updated")
	return nil
}

// PendingUnderDirs lists the ids of pending documents (status='ingested')
// whose path lies under one of dirs. Used by the filesystem watcher and
// `vellum watch run`.
func PendingUnderDirs(conn *sql.DB, dirs []string) ([]int64, error) {
	cleaned := make([]string, 0, len(dirs))
	for _, d := range dirs {
		cleaned = append(cleaned, filepath.Clean(d))
	}
	rows, err := conn.Query(
		"SELECT id, path FROM documents WHERE status='ingested' ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		cp := filepath.Clean(p)
		for _, d := range cleaned {
			if cp == d || strings.HasPrefix(cp, d+string(os.PathSeparator)) {
				ids = append(ids, id)
				break
			}
		}
	}
	return ids, rows.Err()
}

func or_(s string) string {
	if s == "" {
		return "(generic)"
	}
	return s
}
