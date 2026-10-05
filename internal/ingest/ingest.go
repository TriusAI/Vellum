// Package ingest indexes files (text extraction + OCR fallback) and runs the
// LLM processing pass (summarize + constrained tagging). Results are
// structured (per-file, per-document) so the CLI and the web API can both
// report them — to humans and to agents.
package ingest

import (
	"crypto/sha256"
	"database/sql"
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
func Ingest(cfg *config.Config, conn *sql.DB, paths []string, reprocess bool,
	progress func(string)) (*Stats, error) {
	st := &Stats{Files: []FileResult{}}
	files := collectFiles(paths)
	for i, path := range files {
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
	return st, nil
}

func collectFiles(paths []string) []string {
	var files []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			log.Printf("skipping (not found): %s", p)
			continue
		}
		if st.IsDir() {
			filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() && extract.Supported(path) {
					files = append(files, path)
				}
				return nil
			})
		} else {
			files = append(files, p)
		}
	}
	sort.Strings(files)
	return files
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
func ProcessPending(cfg *config.Config, conn *sql.DB, v *vocab.Vocabulary,
	ids []int64, limit int, progress func(string)) ([]ProcessResult, error) {
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
		if progress != nil {
			progress(fmt.Sprintf("document %d/%d: %s", i+1, len(docs), filepath.Base(d.path)))
		}
		log.Printf("processing %s", d.path)
		res, err := processOne(cfg, conn, v, d.id, d.title, d.authors, d.year,
			d.path, d.kind, progress)
		if err != nil {
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

func processOne(cfg *config.Config, conn *sql.DB, v *vocab.Vocabulary,
	docID int64, title, authors, year, path, kind string, progress func(string)) (*summarize.TagResult, error) {
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

	// ---- re-classify on the current text unless the user set the kind:
	// ingest classified from the text layer alone; OCR text (chapters,
	// TOC, preface) flips borderline calls, and classifier improvements
	// reach old libraries at processing time.
	if kindUser == 0 {
		var ocrPages, nPages int64
		if err := conn.QueryRow(
			"SELECT ocr_pages, n_pages FROM documents WHERE id=?", docID).
			Scan(&ocrPages, &nPages); err == nil {
			newKind, _ := classify.Detect(text, int(ocrPages), int(nPages))
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
			tags, err := summarize.TagDocumentWithCategories(cfg, v, existingCategories,
				[]string{abstract, opening}, nil, abstract, progress)
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
				tags, terr := summarize.TagDocumentWithCategories(cfg, v,
					existingCategories, tagInput, nil, front, progress)
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
	summaries, chunks, err := summarize.MapSummaries(cfg, text, progress)
	if err != nil {
		return nil, err
	}
	summary, err := summarize.SummarizeFrom(cfg, chunks, summaries, progress)
	if err != nil {
		return nil, err
	}
	tags, err := summarize.TagDocumentWithCategories(cfg, v, existingCategories,
		chunks, summaries, summary, progress)
	if err != nil {
		return nil, err
	}

	if err := storeProcessed(conn, docID, title, authors, year, tags,
		summary, "generated"); err != nil {
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
