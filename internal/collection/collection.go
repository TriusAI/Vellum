// Package collection imports and exports COLLECTIONS (user-made groups
// of documents) as self-contained .zip bundles, so a research project can
// be shared with someone else and restored on another Vellum.
//
// A bundle is a zip with a manifest.json (collection name/description +
// per-document metadata and tags) and the documents' original files under
// files/. Import extracts the files under <library_dir>/collections/,
// ingests them (text layers only — no OCR), recreates the collection, and
// restores the metadata; the originator's kind/category become user pins.
package collection

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"vellum/internal/config"
	"vellum/internal/db"
	"vellum/internal/ingest"
)

// Manifest is the manifest.json of a shared collection bundle.
type Manifest struct {
	Format     string     `json:"format"`
	Version    int        `json:"version"`
	Collection Collection `json:"collection"`
	Documents  []Document `json:"documents"`
}

// Collection is the collection metadata carried in the manifest.
type Collection struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Document is one bundled document's metadata (File is its path INSIDE
// the zip; Path is the original absolute path, informational only).
type Document struct {
	File          string `json:"file"`
	Path          string `json:"path"`
	Title         string `json:"title,omitempty"`
	Authors       string `json:"authors,omitempty"`
	Year          string `json:"year,omitempty"`
	Summary       string `json:"summary,omitempty"`
	SummarySource string `json:"summary_source,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Category      string `json:"category,omitempty"`
	Tags          []Tag  `json:"tags,omitempty"`
}

// Tag is one document tag with its provenance.
type Tag struct {
	Tag    string `json:"tag"`
	Source string `json:"source"`
}

const format = "vellum-collection"

// Export writes a bundle for collection id to w and returns its name.
// Documents whose file has vanished are listed in the manifest but
// shipped without a payload.
func Export(conn *sql.DB, id int64, w io.Writer) (string, error) {
	var name, desc string
	err := conn.QueryRow(
		"SELECT name, description FROM collections WHERE id=?", id).Scan(&name, &desc)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no such collection")
	}
	if err != nil {
		return "", err
	}

	type drow struct {
		id                            int64
		path                          string
		title, authors, year, summary string
		summarySource, kind, category string
	}
	rows, err := conn.Query(`
SELECT d.id, d.path, COALESCE(d.title,''), COALESCE(d.authors,''), COALESCE(d.year,''),
       COALESCE(d.summary,''), COALESCE(d.summary_source,''), COALESCE(d.kind,''), COALESCE(d.category,'')
FROM documents d JOIN collection_docs cd ON cd.doc_id = d.id
WHERE cd.collection_id = ? ORDER BY d.id`, id)
	if err != nil {
		return "", err
	}
	var docs []drow
	for rows.Next() {
		var d drow
		if err := rows.Scan(&d.id, &d.path, &d.title, &d.authors, &d.year,
			&d.summary, &d.summarySource, &d.kind, &d.category); err != nil {
			rows.Close()
			return "", err
		}
		docs = append(docs, d)
	}
	rows.Close()

	tagsByDoc := map[int64][]Tag{}
	trows, err := conn.Query(`
SELECT cd.doc_id, dt.tag, dt.source FROM collection_docs cd
JOIN doc_tags dt ON dt.doc_id = cd.doc_id WHERE cd.collection_id = ? ORDER BY dt.tag`, id)
	if err == nil {
		for trows.Next() {
			var docID int64
			var t Tag
			trows.Scan(&docID, &t.Tag, &t.Source)
			tagsByDoc[docID] = append(tagsByDoc[docID], t)
		}
		trows.Close()
	}

	zw := zip.NewWriter(w)
	manifest := Manifest{
		Format: format, Version: 1,
		Collection: Collection{Name: name, Description: desc},
	}
	used := map[string]bool{}
	for _, d := range docs {
		entryName := filepath.Base(d.path)
		if used[entryName] {
			entryName = strconv.FormatInt(d.id, 10) + "-" + entryName
		}
		used[entryName] = true
		zipPath := "files/" + entryName
		manifest.Documents = append(manifest.Documents, Document{
			File: zipPath, Path: d.path, Title: d.title, Authors: d.authors,
			Year: d.year, Summary: d.summary, SummarySource: d.summarySource,
			Kind: d.kind, Category: d.category, Tags: tagsByDoc[d.id],
		})
		f, err := os.Open(d.path)
		if err != nil {
			continue // file gone: manifest entry only
		}
		fw, err := zw.Create(zipPath)
		if err == nil {
			_, _ = io.Copy(fw, f)
		}
		f.Close()
	}
	mw, err := zw.Create("manifest.json")
	if err != nil {
		zw.Close()
		return name, err
	}
	enc := json.NewEncoder(mw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		zw.Close()
		return name, err
	}
	return name, zw.Close()
}

// Result reports what an Import did.
type Result struct {
	Collection db.Collection
	Documents  int
	Dir        string
}

// Import reads a bundle from r, extracts its files under
// <library_dir>/collections/<slug>/, ingests them, creates the collection
// (nameOverride wins; the manifest name is used otherwise, made unique if
// taken) and restores each document's metadata and tags.
func Import(ctx context.Context, cfg *config.Config, conn *sql.DB, r io.Reader, nameOverride string) (Result, error) {
	tmp, err := os.CreateTemp("", "vellum-collection-*.zip")
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return Result{}, fmt.Errorf("upload failed: %w", err)
	}
	tmp.Close()

	zr, err := zip.OpenReader(tmp.Name())
	if err != nil {
		return Result{}, fmt.Errorf("not a zip file: %w", err)
	}
	defer zr.Close()

	var manifest Manifest
	found := false
	for _, f := range zr.File {
		if f.Name != "manifest.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return Result{}, fmt.Errorf("manifest unreadable: %w", err)
		}
		err = json.NewDecoder(rc).Decode(&manifest)
		rc.Close()
		if err != nil {
			return Result{}, fmt.Errorf("bad manifest.json: %w", err)
		}
		found = true
		break
	}
	if !found || manifest.Format != format {
		return Result{}, fmt.Errorf("not a vellum collection export (missing/invalid manifest.json)")
	}

	name := strings.TrimSpace(nameOverride)
	if name == "" {
		name = strings.TrimSpace(manifest.Collection.Name)
	}
	if name == "" {
		name = "imported-collection"
	}
	base := expandHome(cfg.LibraryDir)
	dest, err := uniqueDir(filepath.Join(base, "collections", slugify(manifest.Collection.Name)))
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return Result{}, err
	}

	extracted := map[string]string{} // zip entry -> on-disk path
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "files/") || f.FileInfo().IsDir() {
			continue
		}
		clean := filepath.Base(f.Name)
		if clean == "" || clean == "." || clean == string(filepath.Separator) {
			continue
		}
		out := filepath.Join(dest, clean)
		rc, err := f.Open()
		if err != nil {
			return Result{}, fmt.Errorf("extract %s: %w", f.Name, err)
		}
		wf, err := os.Create(out)
		if err != nil {
			rc.Close()
			return Result{}, err
		}
		_, err = io.Copy(wf, rc)
		wf.Close()
		rc.Close()
		if err != nil {
			return Result{}, fmt.Errorf("extract %s: %w", f.Name, err)
		}
		extracted[f.Name] = out
	}
	if len(extracted) == 0 {
		return Result{}, fmt.Errorf("the bundle contains no files/ entries")
	}

	paths := make([]string, 0, len(extracted))
	for _, p := range extracted {
		paths = append(paths, p)
	}
	if _, err := ingest.Ingest(ctx, cfg, conn, paths, false, nil); err != nil {
		return Result{}, fmt.Errorf("ingest failed: %w", err)
	}
	pathIDs := map[string]int64{}
	for _, p := range paths {
		var docID int64
		if err := conn.QueryRow("SELECT id FROM documents WHERE path=?", p).
			Scan(&docID); err == nil {
			pathIDs[p] = docID
		}
	}

	colName, err := uniqueCollectionName(conn, name)
	if err != nil {
		return Result{}, err
	}
	col, err := db.CreateCollection(conn, colName, manifest.Collection.Description)
	if err != nil {
		return Result{}, err
	}

	added := []int64{}
	for _, md := range manifest.Documents {
		p, ok := extracted[md.File]
		if !ok {
			continue
		}
		docID, ok := pathIDs[p]
		if !ok {
			continue
		}
		status := "ingested"
		if strings.TrimSpace(md.Summary) != "" {
			status = "done"
		}
		if _, err := conn.Exec(`
UPDATE documents SET title=?, authors=?, year=?, summary=?, summary_source=?,
  kind=?, kind_user=CASE WHEN ?='' THEN kind_user ELSE 1 END,
  category=?, category_user=CASE WHEN ?='' THEN category_user ELSE 1 END,
  status=?, processed_at=CASE WHEN ?='done' THEN datetime('now') ELSE processed_at END
WHERE id=?`,
			md.Title, md.Authors, md.Year, md.Summary, md.SummarySource,
			md.Kind, md.Kind, md.Category, md.Category, status, status, docID); err != nil {
			return Result{}, err
		}
		pairs := make([][2]string, 0, len(md.Tags))
		for _, t := range md.Tags {
			src := t.Source
			if src == "" {
				src = "manual"
			}
			pairs = append(pairs, [2]string{t.Tag, src})
		}
		if err := db.SetTags(conn, docID, pairs); err != nil {
			return Result{}, err
		}
		added = append(added, docID)
	}
	if len(added) > 0 {
		if _, err := db.AddDocsToCollection(conn, col.ID, added); err != nil {
			return Result{}, err
		}
	}
	col.Description = manifest.Collection.Description
	return Result{Collection: col, Documents: len(added), Dir: dest}, nil
}

// Slug returns a filesystem-safe token for a collection name (used in the
// download filename).
func Slug(name string) string { return slugify(name) }

func uniqueCollectionName(conn *sql.DB, name string) (string, error) {
	candidate := name
	for i := 2; ; i++ {
		var n int
		if err := conn.QueryRow(
			"SELECT COUNT(*) FROM collections WHERE name=?", candidate).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s (%d)", name, i)
	}
}

func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "collection"
	}
	return out
}

func uniqueDir(path string) (string, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path, nil
	}
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s-%d", path, i)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("no free directory name near %s", path)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}
