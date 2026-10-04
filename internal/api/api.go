// Package api is Vellum's JSON surface: a small REST API under /api/ used by
// both the embedded web UI and AI agents (documented by `vellum agent`).
// The web UI itself is embedded from web/ and served at /.
package api

import (
	"database/sql"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"vellum/internal/config"
	"vellum/internal/db"
	"vellum/internal/ingest"
	"vellum/internal/llm"
	"vellum/internal/search"
	"vellum/internal/vocab"
)

//go:embed all:web
var webFS embed.FS

// Server wires config + db to the HTTP handlers. The vocabulary is re-read
// from disk on every mutation so manual edits to vocab.yaml are picked up.
type Server struct {
	cfg  *config.Config
	conn *sql.DB

	mu       sync.Mutex
	progress map[string]any // live processing state (single-user tool)
}

// New creates a Server.
func New(cfg *config.Config, conn *sql.DB) *Server {
	return &Server{cfg: cfg, conn: conn}
}

// Mux returns the root http.Handler: /api/* plus the embedded web UI.
func (s *Server) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/progress", s.getProgress)
	mux.HandleFunc("GET /api/documents", s.documents)
	mux.HandleFunc("GET /api/documents/{id}", s.document)
	mux.HandleFunc("GET /api/documents/{id}/file", s.file)
	mux.HandleFunc("PATCH /api/documents/{id}", s.patchDocument)
	mux.HandleFunc("PUT /api/documents/{id}/tags", s.putTags)
	mux.HandleFunc("POST /api/ingest", s.postIngest)
	mux.HandleFunc("POST /api/process", s.postProcess)
	mux.HandleFunc("GET /api/search", s.search)
	mux.HandleFunc("GET /api/vocab", s.getVocab)
	mux.HandleFunc("GET /api/vocab/suggestions", s.suggestions)
	mux.HandleFunc("POST /api/vocab", s.postVocab)
	mux.HandleFunc("DELETE /api/vocab/{name}", s.deleteVocab)

	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}

// ---------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

// documentJSON is one library entry, machine-friendly.
type documentJSON struct {
	ID            int64    `json:"id"`
	Path          string   `json:"path"`
	Title         string   `json:"title"`
	Authors       string   `json:"authors"`
	Year          string   `json:"year"`
	Summary       string   `json:"summary"`
	Status        string   `json:"status"`
	Kind          string   `json:"kind,omitempty"`
	SummarySource string   `json:"summary_source,omitempty"`
	Tags          []string `json:"tags"`
	OCRPages      int      `json:"ocr_pages"`
	NPages        int      `json:"n_pages"`
	Error         string   `json:"error,omitempty"`
	AddedAt       string   `json:"added_at"`
	ProcessedAt   string   `json:"processed_at,omitempty"`
}

const docColumns = "id, path, title, authors, year, summary, status, " +
	"kind, summary_source, error, ocr_pages, n_pages, added_at, processed_at"

type scanDoc struct {
	id                            int64
	path, status, addedAt         string
	title, authors, year, summary sql.NullString
	kind, summarySource, err      sql.NullString
	processedAt                   sql.NullString
	ocrPages, nPages              sql.NullInt64
}

func scanDocRow(sc interface{ Scan(...any) error }) (scanDoc, error) {
	var d scanDoc
	err := sc.Scan(&d.id, &d.path, &d.title, &d.authors, &d.year, &d.summary,
		&d.status, &d.kind, &d.summarySource, &d.err, &d.ocrPages, &d.nPages,
		&d.addedAt, &d.processedAt)
	return d, err
}

func (d scanDoc) toJSON() documentJSON {
	return documentJSON{
		ID: d.id, Path: d.path,
		Title: d.title.String, Authors: d.authors.String,
		Year: d.year.String, Summary: d.summary.String, Status: d.status,
		Kind: d.kind.String, SummarySource: d.summarySource.String,
		OCRPages: int(d.ocrPages.Int64), NPages: int(d.nPages.Int64),
		Error: d.err.String, AddedAt: d.addedAt,
		ProcessedAt: d.processedAt.String, Tags: []string{},
	}
}

// tagsByDoc returns doc_id -> tags for all (or one) documents.
func (s *Server) tagsByDoc(docID int64) map[int64][]string {
	out := map[int64][]string{}
	rows, err := s.conn.Query(
		"SELECT doc_id, tag FROM doc_tags WHERE ? ORDER BY doc_id, tag", docID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var t string
		if rows.Scan(&id, &t) == nil {
			out[id] = append(out[id], t)
		}
	}
	return out
}

// ---------------------------------------------------------------- handlers

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	var counts struct {
		Documents, Done, Pending, Errors, Chunks int
	}
	s.conn.QueryRow("SELECT COUNT(*) FROM documents").Scan(&counts.Documents)
	s.conn.QueryRow("SELECT COUNT(*) FROM documents WHERE status='done'").Scan(&counts.Done)
	s.conn.QueryRow("SELECT COUNT(*) FROM documents WHERE status='ingested'").Scan(&counts.Pending)
	s.conn.QueryRow("SELECT COUNT(*) FROM documents WHERE status='error'").Scan(&counts.Errors)
	s.conn.QueryRow("SELECT COUNT(*) FROM chunks").Scan(&counts.Chunks)

	v, err := vocab.Load(s.cfg.VocabPath)
	vocabSize := 0
	if err == nil {
		vocabSize = len(v.Tags)
	}

	writeJSON(w, 200, map[string]any{
		"version":     version,
		"db":          s.cfg.DBPath,
		"library_dir": s.cfg.LibraryDir,
		"documents":   counts.Documents,
		"done":        counts.Done,
		"pending":     counts.Pending,
		"errors":      counts.Errors,
		"chunks":      counts.Chunks,
		"vocab_tags":  vocabSize,
		"llm_up":      llm.Available(s.cfg.Tools.LLMURL),
		"embed_up":    llm.Available(s.cfg.Tools.EmbedURL),
		"llm_url":     s.cfg.Tools.LLMURL,
		"embed_url":   s.cfg.Tools.EmbedURL,
	})
}

// version is set by the CLI package at init (see SetVersion).
var version = "dev"

// SetVersion lets cmd/vellum inject the build version.
func SetVersion(v string) { version = v }

func (s *Server) documents(w http.ResponseWriter, r *http.Request) {
	rows, err := s.conn.Query("SELECT " + docColumns + " FROM documents ORDER BY id")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()

	tags := s.tagsByDoc(-1) // all documents
	docs := []documentJSON{}
	for rows.Next() {
		d, err := scanDocRow(rows)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		doc := d.toJSON()
		doc.Tags = tags[doc.ID]
		if doc.Tags == nil {
			doc.Tags = []string{}
		}
		docs = append(docs, doc)
	}
	writeJSON(w, 200, docs)
}

func (s *Server) document(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad document id")
		return
	}
	d, err := scanDocRow(s.conn.QueryRow(
		"SELECT "+docColumns+" FROM documents WHERE id=?", id))
	if err == sql.ErrNoRows {
		writeErr(w, 404, "no such document")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	doc := d.toJSON()
	doc.Tags = s.tagsByDoc(id)[id]
	if doc.Tags == nil {
		doc.Tags = []string{}
	}

	chunks := []map[string]any{}
	crows, err := s.conn.Query(
		"SELECT seq, page_no, text FROM chunks WHERE doc_id=? ORDER BY seq", id)
	if err == nil {
		defer crows.Close()
		for crows.Next() {
			var seq int
			var page sql.NullInt64
			var text string
			crows.Scan(&seq, &page, &text)
			chunks = append(chunks, map[string]any{
				"seq": seq, "page": page.Int64, "text": text})
		}
	}

	// tag sources, for the curation UI
	sources := map[string]string{}
	srows, err := s.conn.Query("SELECT tag, source FROM doc_tags WHERE doc_id=?", id)
	if err == nil {
		for srows.Next() {
			var t, src string
			srows.Scan(&t, &src)
			sources[t] = src
		}
		srows.Close()
	}

	writeJSON(w, 200, map[string]any{
		"document": doc, "chunks": chunks, "tag_sources": sources})
}

func (s *Server) patchDocument(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad document id")
		return
	}
	var body struct {
		Title, Authors, Year, Summary, Kind *string
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	set := map[string]string{}
	for col, p := range map[string]*string{
		"title": body.Title, "authors": body.Authors,
		"year": body.Year, "summary": body.Summary, "kind": body.Kind} {
		if p != nil {
			set[col] = *p
		}
	}
	if len(set) == 0 {
		writeErr(w, 400, "nothing to update (title/authors/year/summary)")
		return
	}
	assignments := ""
	args := []any{}
	for col, val := range set {
		if assignments != "" {
			assignments += ", "
		}
		assignments += col + "=?"
		args = append(args, val)
	}
	args = append(args, id)
	res, err := s.conn.Exec("UPDATE documents SET "+assignments+" WHERE id=?", args...)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 404, "no such document")
		return
	}
	s.document(w, r)
}

func (s *Server) putTags(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad document id")
		return
	}
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	var pairs [][2]string
	for _, t := range body.Tags {
		pairs = append(pairs, [2]string{t, "manual"})
	}
	if err := db.SetTags(s.conn, id, pairs); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.document(w, r)
}

func (s *Server) postIngest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths     []string `json:"paths"`
		Reprocess bool     `json:"reprocess"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	if len(body.Paths) == 0 {
		writeErr(w, 400, "paths[] required")
		return
	}
	st, err := ingest.Ingest(s.cfg, s.conn, body.Paths, body.Reprocess)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) postProcess(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs   []int64 `json:"ids"`
		Limit int     `json:"limit"`
	}
	if r.ContentLength > 0 {
		if err := decodeBody(r, &body); err != nil {
			writeErr(w, 400, "bad JSON body: "+err.Error())
			return
		}
	}
	v, err := vocab.Load(s.cfg.VocabPath)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if len(v.Tags) == 0 {
		writeErr(w, 400, "vocab.yaml is empty — add tags first (see `vellum agent` docs)")
		return
	}
	if !llm.Available(s.cfg.Tools.LLMURL) {
		writeErr(w, 503, "no llama-server at "+s.cfg.Tools.LLMURL+
			" — start it with the vellum launcher")
		return
	}
	s.mu.Lock()
	s.progress = map[string]any{"running": true, "message": "starting", "updated": time.Now().Format(time.RFC3339)}
	s.mu.Unlock()
	results, err := ingest.ProcessPending(s.cfg, s.conn, v, body.IDs, body.Limit,
		func(msg string) {
			s.mu.Lock()
			s.progress = map[string]any{"running": true, "message": msg, "updated": time.Now().Format(time.RFC3339)}
			s.mu.Unlock()
		})
	s.mu.Lock()
	s.progress = map[string]any{"running": false, "message": "", "updated": time.Now().Format(time.RFC3339)}
	s.mu.Unlock()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, results)
}

// file serves the original document file. PDFs render inline in the
// browser (the UI embeds them as a preview); ?dl=1 forces download.
func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad document id")
		return
	}
	var path string
	if err := s.conn.QueryRow("SELECT path FROM documents WHERE id=?", id).
		Scan(&path); err != nil {
		writeErr(w, 404, "no such document")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, 410, "file no longer readable: "+err.Error())
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	disposition := "inline"
	if r.URL.Query().Get("dl") != "" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition",
		disposition+`; filename="`+filepath.Base(path)+`"`)
	if strings.EqualFold(filepath.Ext(path), ".pdf") {
		w.Header().Set("Content-Type", "application/pdf")
	}
	http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), f)
}

// getProgress reports the live processing state (polled by the UI while
// a process request is in flight).
func (s *Server) getProgress(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.progress
	if st == nil {
		st = map[string]any{"running": false, "message": ""}
	}
	writeJSON(w, 200, st)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	mode := r.URL.Query().Get("mode")
	limit := 20
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}
	if q == "" {
		writeErr(w, 400, "q required")
		return
	}

	if mode == "semantic" {
		hits, err := search.Semantic(s.cfg, s.conn, q, limit)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out := []map[string]any{}
		for _, h := range hits {
			snips := []map[string]any{}
			for _, sn := range h.Snippets {
				snips = append(snips, map[string]any{
					"page": sn.Page, "text": sn.Text, "score": sn.Score})
			}
			out = append(out, map[string]any{
				"doc_id": h.DocID, "title": h.Title, "path": h.Path,
				"snippets": snips})
		}
		writeJSON(w, 200, out)
		return
	}

	hits, err := search.Keyword(s.conn, q, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := []map[string]any{}
	for _, h := range hits {
		out = append(out, map[string]any{
			"doc_id": h.DocID, "title": h.Title, "path": h.Path,
			"page": h.Page, "snippet": h.Snippet})
	}
	writeJSON(w, 200, out)
}

func (s *Server) getVocab(w http.ResponseWriter, r *http.Request) {
	v, err := vocab.Load(s.cfg.VocabPath)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := []map[string]string{}
	for _, k := range v.SortedKeys() {
		out = append(out, map[string]string{"name": k, "description": v.Tags[k]})
	}
	writeJSON(w, 200, out)
}

func (s *Server) suggestions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.conn.Query(`
SELECT t.tag, COUNT(*) AS n, MIN(d.title) AS example
FROM doc_tags t JOIN documents d ON d.id = t.doc_id
WHERE t.source='suggested'
GROUP BY t.tag ORDER BY n DESC, t.tag`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var tag, example sql.NullString
		var n int
		rows.Scan(&tag, &n, &example)
		out = append(out, map[string]any{
			"tag": tag.String, "count": n, "example": example.String})
	}
	writeJSON(w, 200, out)
}

// postVocab adds (or promotes) a vocabulary tag.
func (s *Server) postVocab(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	if body.Name == "" || body.Description == "" {
		writeErr(w, 400, "name and description required")
		return
	}
	v, err := vocab.Load(s.cfg.VocabPath)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	v.Add(body.Name, body.Description)
	if err := v.Save(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// promoting: any suggested tags of this name become controlled
	res, _ := s.conn.Exec(
		"UPDATE doc_tags SET source='vocab' WHERE tag=?", body.Name)
	moved, _ := res.RowsAffected()
	writeJSON(w, 200, map[string]any{
		"name": body.Name, "promoted_doc_tags": moved})
}

func (s *Server) deleteVocab(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeErr(w, 400, "name required")
		return
	}
	v, err := vocab.Load(s.cfg.VocabPath)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !v.Remove(name) {
		writeErr(w, 404, "not in vocabulary")
		return
	}
	if err := v.Save(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.conn.Exec("DELETE FROM doc_tags WHERE tag=?", name)
	writeJSON(w, 200, map[string]string{"removed": name})
}
