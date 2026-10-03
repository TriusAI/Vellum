// Package ingest indexes files (text extraction + OCR fallback) and runs the
// LLM processing pass (summarize + constrained tagging).
package ingest

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vellum/internal/config"
	"vellum/internal/db"
	"vellum/internal/extract"
	"vellum/internal/summarize"
	"vellum/internal/vocab"
)

// Stats summarizes an ingest run.
type Stats struct {
	Added, Updated, Skipped, Failed int
}

// Ingest walks the paths and indexes every supported file, with sha256-based
// change detection.
func Ingest(cfg *config.Config, conn *sql.DB, paths []string, reprocess bool) (*Stats, error) {
	st := &Stats{}

	files := collectFiles(paths)
	for _, path := range files {
		if err := ingestOne(cfg, conn, path, reprocess, st); err != nil {
			log.Printf("failed to ingest %s: %s", path, err)
			st.Failed++
		}
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

func ingestOne(cfg *config.Config, conn *sql.DB, path string, reprocess bool, st *Stats) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	digest, err := sha256file(abs)
	if err != nil {
		return err
	}

	var docID int64
	var oldDigest string
	err = conn.QueryRow("SELECT id, sha256 FROM documents WHERE path=?", abs).
		Scan(&docID, &oldDigest)
	known := err == nil
	if known && oldDigest == digest && !reprocess {
		st.Skipped++
		return nil
	}

	t0 := time.Now()
	res, err := extract.Extract(abs, cfg)
	if err != nil {
		return err
	}
	hasText := false
	for _, c := range res.Chunks {
		if c.Text != "" {
			hasText = true
			break
		}
	}
	if !hasText {
		return fmt.Errorf("no text extracted")
	}

	if known {
		if _, err := conn.Exec(
			"UPDATE documents SET sha256=?, status='ingested', error=NULL, processed_at=NULL WHERE id=?",
			digest, docID); err != nil {
			return err
		}
		st.Updated++
	} else {
		result, err := conn.Exec(
			"INSERT INTO documents(path, sha256, title, authors, year, ocr_pages, status) VALUES(?,?,?,?,?,?,'ingested')",
			abs, digest, res.Title, res.Authors, "", res.OCRPages)
		if err != nil {
			return err
		}
		docID, err = result.LastInsertId()
		if err != nil {
			return err
		}
		st.Added++
	}

	if err := db.ReplaceDocumentText(conn, docID, res.Chunks); err != nil {
		return err
	}
	log.Printf("%s: %d chunks (%d OCR pages) in %.1fs",
		filepath.Base(abs), len(res.Chunks), res.OCRPages, time.Since(t0).Seconds())
	return nil
}

// ProcessPending summarizes and tags all documents with status 'ingested'.
func ProcessPending(cfg *config.Config, conn *sql.DB, v *vocab.Vocabulary, limit int) (int, error) {
	q := "SELECT id, path, title, authors, year FROM documents WHERE status='ingested' ORDER BY id"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := conn.Query(q)
	if err != nil {
		return 0, err
	}
	type docRow struct {
		id                         int64
		path, title, authors, year string
	}
	var docs []docRow
	for rows.Next() {
		var d docRow
		if err := rows.Scan(&d.id, &d.path, &d.title, &d.authors, &d.year); err != nil {
			rows.Close()
			return 0, err
		}
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	done := 0
	for _, d := range docs {
		log.Printf("processing %s", d.path)
		err := processOne(cfg, conn, v, d.id, d.title, d.authors, d.year)
		if err != nil {
			log.Printf("processing failed for %s: %s", d.path, err)
			conn.Exec("UPDATE documents SET status='error', error=? WHERE id=?",
				err.Error(), d.id)
			continue
		}
		done++
	}
	return done, nil
}

func processOne(cfg *config.Config, conn *sql.DB, v *vocab.Vocabulary,
	docID int64, title, authors, year string) error {
	text, err := db.DocumentText(conn, docID)
	if err != nil {
		return err
	}
	summary, err := summarize.Summarize(cfg, text)
	if err != nil {
		return err
	}
	tags, err := summarize.TagDocument(cfg, v, text)
	if err != nil {
		return err
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

	if _, err := conn.Exec(
		"UPDATE documents SET summary=?, title=?, authors=?, year=?, status='done', processed_at=datetime('now') WHERE id=?",
		summary, newTitle, newAuthors, newYear, docID); err != nil {
		return err
	}
	pairs := make([][2]string, 0, len(tags.Tags)+len(tags.TagsOther))
	for _, t := range tags.Tags {
		pairs = append(pairs, [2]string{t, "vocab"})
	}
	for _, t := range tags.TagsOther {
		pairs = append(pairs, [2]string{t, "suggested"})
	}
	if err := db.SetTags(conn, docID, pairs); err != nil {
		return err
	}
	log.Printf("done: tags: %s%s", strings.Join(tags.Tags, ", "),
		func() string {
			if len(tags.TagsOther) > 0 {
				return " | suggested: " + strings.Join(tags.TagsOther, ", ")
			}
			return ""
		}())
	return nil
}
