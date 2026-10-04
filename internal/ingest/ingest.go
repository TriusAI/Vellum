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
	Error     string   `json:"error,omitempty"`
}

// Ingest walks the paths and indexes every supported file, with sha256-based
// change detection.
func Ingest(cfg *config.Config, conn *sql.DB, paths []string, reprocess bool) (*Stats, error) {
	st := &Stats{Files: []FileResult{}}

	for _, path := range collectFiles(paths) {
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
	res, err := extract.Extract(abs, cfg)
	if err != nil {
		return "failed", err
	}
	hasText := false
	for _, c := range res.Chunks {
		if c.Text != "" {
			hasText = true
			break
		}
	}
	if !hasText {
		return "failed", fmt.Errorf("no text extracted")
	}

	action := "updated"
	if known {
		if _, err := conn.Exec(
			"UPDATE documents SET sha256=?, status='ingested', error=NULL, processed_at=NULL WHERE id=?",
			digest, docID); err != nil {
			return "failed", err
		}
	} else {
		result, err := conn.Exec(
			"INSERT INTO documents(path, sha256, title, authors, year, ocr_pages, status) VALUES(?,?,?,?,?,?,'ingested')",
			abs, digest, res.Title, res.Authors, "", res.OCRPages)
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
	log.Printf("%s: %d chunks (%d OCR pages) in %.1fs",
		filepath.Base(abs), len(res.Chunks), res.OCRPages, time.Since(t0).Seconds())
	return action, nil
}

// ProcessPending summarizes and tags documents. With ids empty it takes all
// pending ('ingested') documents, limited by limit (0 = no limit); with ids
// it processes exactly those, whatever their status (a 'done' document gets
// a fresh summary and tags).
func ProcessPending(cfg *config.Config, conn *sql.DB, v *vocab.Vocabulary,
	ids []int64, limit int) ([]ProcessResult, error) {
	type docRow struct {
		id                         int64
		path, title, authors, year string
	}
	var docs []docRow

	if len(ids) > 0 {
		for _, id := range ids {
			var d docRow
			err := conn.QueryRow(
				"SELECT id, path, title, authors, year FROM documents WHERE id=?", id).
				Scan(&d.id, &d.path, &d.title, &d.authors, &d.year)
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
		q := "SELECT id, path, title, authors, year FROM documents WHERE status='ingested' ORDER BY id"
		if limit > 0 {
			q += fmt.Sprintf(" LIMIT %d", limit)
		}
		rows, err := conn.Query(q)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var d docRow
			if err := rows.Scan(&d.id, &d.path, &d.title, &d.authors, &d.year); err != nil {
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
	for _, d := range docs {
		log.Printf("processing %s", d.path)
		res, err := processOne(cfg, conn, v, d.id, d.title, d.authors, d.year)
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
			Tags: res.Tags, TagsOther: res.TagsOther})
	}
	return results, nil
}

func processOne(cfg *config.Config, conn *sql.DB, v *vocab.Vocabulary,
	docID int64, title, authors, year string) (*summarize.TagResult, error) {
	text, err := db.DocumentText(conn, docID)
	if err != nil {
		return nil, err
	}
	// one map phase feeds both the summary and the tagging call —
	// long documents are tagged from summaries + opening text, not by
	// re-sending a huge raw-text prefix
	summaries, chunks, err := summarize.MapSummaries(cfg, text)
	if err != nil {
		return nil, err
	}
	summary, err := summarize.SummarizeFrom(cfg, chunks, summaries)
	if err != nil {
		return nil, err
	}
	tags, err := summarize.TagDocument(cfg, v, chunks, summaries, summary)
	if err != nil {
		return nil, err
	}

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

	if _, err := conn.Exec(
		"UPDATE documents SET summary=?, title=?, authors=?, year=?, status='done', processed_at=datetime('now') WHERE id=?",
		summary, newTitle, newAuthors, newYear, docID); err != nil {
		return nil, err
	}
	pairs := make([][2]string, 0, len(tags.Tags)+len(tags.TagsOther))
	for _, t := range tags.Tags {
		pairs = append(pairs, [2]string{t, "vocab"})
	}
	for _, t := range tags.TagsOther {
		pairs = append(pairs, [2]string{t, "suggested"})
	}
	if err := db.SetTags(conn, docID, pairs); err != nil {
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
