// Package api is Vellum's JSON surface: a small REST API under /api/ used by
// both the embedded web UI and AI agents (documented by `vellum agent`).
// The web UI itself is embedded from web/ and served at /.
package api

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"vellum/internal/ask"
	"vellum/internal/classify"
	"vellum/internal/collection"
	"vellum/internal/config"
	"vellum/internal/db"
	"vellum/internal/extract"
	"vellum/internal/ingest"
	"vellum/internal/llm"
	"vellum/internal/search"
	"vellum/internal/summarize"
	"vellum/internal/vocab"
)

//go:embed all:web
var webFS embed.FS

// Server wires config + db to the HTTP handlers. The vocabulary is re-read
// from disk on every mutation so manual edits to vocab.yaml are picked up.
type Job struct {
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	Label      string    `json:"label"`
	Status     string    `json:"status"` // queued|running|done|error|cancelled
	Message    string    `json:"message,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	cancel context.CancelFunc
	ctx    context.Context
}

// Stopped reports cooperative-cancellation state (checked by long
// loops between units of work).
func (j *Job) Stopped() bool { return j.ctx.Err() != nil }

type jobWaiter struct {
	job  *Job
	turn chan struct{}
}

type Server struct {
	cfg  *config.Config
	conn *sql.DB

	mu       sync.Mutex
	progress map[string]any // live state for /api/progress (running job)

	// jobs: FIFO queue + history for /api/jobs (single-user tool).
	jobsMu     sync.Mutex
	jobs       []*Job
	jobsSeq    int64
	jobRunning *Job
	waiters    []*jobWaiter
}

// jobProgress records the job's live message (also surfaces on
// /api/progress for the notice bar).
func (s *Server) jobProgress(j *Job, msg string) {
	j.Message = msg
	s.mu.Lock()
	s.progress = map[string]any{
		"running": j.Status == "running" || j.Status == "queued",
		"message": msg, "updated": time.Now().Format(time.RFC3339),
	}
	s.mu.Unlock()
}

func (s *Server) finishJob(j *Job, status, errMsg string) {
	j.Status = status
	if errMsg != "" {
		j.Error = errMsg
	}
	j.FinishedAt = time.Now()
	s.mu.Lock()
	s.progress = map[string]any{"running": false, "message": "",
		"updated": time.Now().Format(time.RFC3339)}
	s.mu.Unlock()
}

// runJob runs one slow operation through the FIFO queue. work may call
// j.Report via s.jobProgress for live messages and check j.Stopped()
// for cooperative cancellation. Returns the final job state.
func (s *Server) runJob(kind, label string, work func(j *Job) error) *Job {
	j := &Job{Kind: kind, Label: label, Status: "queued",
		CreatedAt: time.Now()}
	j.ctx, j.cancel = context.WithCancel(context.Background())

	s.jobsMu.Lock()
	s.jobsSeq++
	j.ID = s.jobsSeq
	s.jobs = append(s.jobs, j)
	// keep history bounded: drop oldest FINISHED entries beyond 40
	for len(s.jobs) > 40 {
		dropped := false
		for i, x := range s.jobs {
			if x.Status != "queued" && x.Status != "running" {
				s.jobs = append(s.jobs[:i], s.jobs[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			break
		}
	}
	if s.jobRunning == nil {
		s.jobRunning = j
		s.jobsMu.Unlock()
	} else {
		w := &jobWaiter{job: j, turn: make(chan struct{})}
		s.waiters = append(s.waiters, w)
		s.jobsMu.Unlock()
		select {
		case <-w.turn:
			if j.Stopped() {
				// cancelled right as the turn arrived
				s.jobsMu.Lock()
				if s.jobRunning == j {
					s.jobRunning = nil
					// pass the turn on (below's unlock section)
					s.jobRunning = nil
				}
				s.jobsMu.Unlock()
				s.finishJob(j, "cancelled", "")
				return j
			}
		case <-j.ctx.Done():
			s.jobsMu.Lock()
			for i, x := range s.waiters {
				if x.job == j {
					s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
					break
				}
			}
			s.jobsMu.Unlock()
			s.finishJob(j, "cancelled", "")
			return j
		}
	}
	j.Status = "running"
	j.StartedAt = time.Now()
	err := work(j)
	switch {
	case j.Stopped():
		s.finishJob(j, "cancelled", "")
	case err != nil:
		s.finishJob(j, "error", err.Error())
	default:
		s.finishJob(j, "done", "")
	}
	// hand the turn over to the next still-interested waiter
	s.jobsMu.Lock()
	s.jobRunning = nil
	for len(s.waiters) > 0 {
		w := s.waiters[0]
		s.waiters = s.waiters[1:]
		if w.job.Stopped() {
			close(w.turn) // wake it so it cleans itself up
			continue
		}
		s.jobRunning = w.job
		close(w.turn)
		break
	}
	s.jobsMu.Unlock()
	return j
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
	mux.HandleFunc("GET /api/categories", s.categories)
	mux.HandleFunc("POST /api/categories/rename", s.renameCategory)
	mux.HandleFunc("GET /api/collections", s.listCollections)
	mux.HandleFunc("POST /api/collections", s.createCollection)
	mux.HandleFunc("GET /api/collections/{id}", s.getCollection)
	mux.HandleFunc("PATCH /api/collections/{id}", s.patchCollection)
	mux.HandleFunc("DELETE /api/collections/{id}", s.deleteCollection)
	mux.HandleFunc("POST /api/collections/{id}/documents", s.addCollectionDocs)
	mux.HandleFunc("DELETE /api/collections/{id}/documents/{docID}", s.removeCollectionDoc)
	mux.HandleFunc("GET /api/collections/{id}/export", s.exportCollection)
	mux.HandleFunc("POST /api/collections/import", s.importCollection)
	mux.HandleFunc("GET /api/documents/{id}", s.document)
	mux.HandleFunc("DELETE /api/documents/{id}", s.deleteDocument)
	mux.HandleFunc("GET /api/documents/{id}/file", s.file)
	mux.HandleFunc("GET /api/documents/{id}/cover", s.cover)
	mux.HandleFunc("PATCH /api/documents/{id}", s.patchDocument)
	mux.HandleFunc("PUT /api/documents/{id}/tags", s.putTags)
	mux.HandleFunc("POST /api/ingest", s.postIngest)
	mux.HandleFunc("GET /api/fs", s.fsList)
	mux.HandleFunc("POST /api/documents/{id}/reextract", s.reextract)
	mux.HandleFunc("POST /api/documents/{id}/regenerate", s.regenerate)
	mux.HandleFunc("POST /api/process", s.postProcess)
	mux.HandleFunc("GET /api/ask/config", s.getAskConfig)
	mux.HandleFunc("PUT /api/ask/config", s.putAskConfig)
	mux.HandleFunc("POST /api/ask/test", s.testAsk)
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("PUT /api/config", s.putConfig)
	mux.HandleFunc("GET /api/library/export", s.exportLibrary)
	mux.HandleFunc("POST /api/library/import", s.importLibrary)
	mux.HandleFunc("POST /api/documents/{id}/ask", s.postAsk)
	mux.HandleFunc("GET /api/jobs", s.jobList)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.jobCancel)
	mux.HandleFunc("GET /api/search", s.search)
	mux.HandleFunc("GET /api/vocab", s.getVocab)
	mux.HandleFunc("GET /api/vocab/suggestions", s.suggestions)
	mux.HandleFunc("POST /api/vocab", s.postVocab)
	mux.HandleFunc("DELETE /api/vocab/{name}", s.deleteVocab)
	mux.HandleFunc("GET /api/tags", s.listTags)
	mux.HandleFunc("GET /api/tags/{tag}", s.getTag)
	mux.HandleFunc("GET /api/notes", s.listNotes)
	mux.HandleFunc("POST /api/notes", s.createNote)
	mux.HandleFunc("GET /api/notes/{id}", s.getNote)
	mux.HandleFunc("PATCH /api/notes/{id}", s.patchNote)
	mux.HandleFunc("DELETE /api/notes/{id}", s.deleteNote)

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
	Category      string   `json:"category,omitempty"`
	OcrPending    bool     `json:"ocr_pending,omitempty"`
	SkipPages     string   `json:"skip_pages,omitempty"`
	Tags          []string `json:"tags"`
	OCRPages      int      `json:"ocr_pages"`
	NPages        int      `json:"n_pages"`
	Error         string   `json:"error,omitempty"`
	AddedAt       string   `json:"added_at"`
	ProcessedAt   string   `json:"processed_at,omitempty"`
}

const docColumns = "id, path, title, authors, year, summary, status, " +
	"kind, summary_source, category, error, ocr_pages, n_pages, ocr_pending, " +
	"skip_pages, added_at, processed_at"

type scanDoc struct {
	id                            int64
	path, status, addedAt         string
	title, authors, year, summary sql.NullString
	kind, summarySource, category sql.NullString
	err                           sql.NullString
	processedAt                   sql.NullString
	ocrPages, nPages              sql.NullInt64
	ocrPending                    sql.NullInt64
	skipPages                     sql.NullString
}

func scanDocRow(sc interface{ Scan(...any) error }) (scanDoc, error) {
	var d scanDoc
	err := sc.Scan(&d.id, &d.path, &d.title, &d.authors, &d.year, &d.summary,
		&d.status, &d.kind, &d.summarySource, &d.category, &d.err, &d.ocrPages,
		&d.nPages, &d.ocrPending, &d.skipPages, &d.addedAt, &d.processedAt)
	return d, err
}

func (d scanDoc) toJSON() documentJSON {
	return documentJSON{
		ID: d.id, Path: d.path,
		Title: d.title.String, Authors: d.authors.String,
		Year: d.year.String, Summary: d.summary.String, Status: d.status,
		Kind: d.kind.String, SummarySource: d.summarySource.String,
		Category:   d.category.String,
		OcrPending: d.ocrPending.Int64 > 0,
		SkipPages:  d.skipPages.String,
		OCRPages:   int(d.ocrPages.Int64), NPages: int(d.nPages.Int64),
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

// filters from query params: kind, category, repeated tag
type docFilter struct {
	kind, category string
	tags           []string
}

func parseFilter(r *http.Request) docFilter {
	q := r.URL.Query()
	return docFilter{
		kind:     q.Get("kind"),
		category: q.Get("category"),
		tags:     q["tag"],
	}
}

// empty reports whether any filter is set.
func (f docFilter) empty() bool {
	return f.kind == "" && f.category == "" && len(f.tags) == 0
}

// match checks a document against the filter (kind/category compare
// case-insensitively; a document matches the tag filter only if it has ALL
// the requested tags).
func (f docFilter) match(d documentJSON) bool {
	if f.kind != "" && !strings.EqualFold(d.Kind, f.kind) {
		return false
	}
	if f.category != "" && !strings.EqualFold(d.Category, f.category) {
		return false
	}
	for _, want := range f.tags {
		have := false
		for _, t := range d.Tags {
			if strings.EqualFold(t, want) {
				have = true
				break
			}
		}
		if !have {
			return false
		}
	}
	return true
}

func (s *Server) documents(w http.ResponseWriter, r *http.Request) {
	rows, err := s.conn.Query("SELECT " + docColumns + " FROM documents ORDER BY id")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()

	tags := s.tagsByDoc(-1) // all documents
	filter := parseFilter(r)
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
		if !filter.empty() && !filter.match(doc) {
			continue
		}
		docs = append(docs, doc)
	}
	writeJSON(w, 200, docs)
}

// allDocuments returns every document with tags attached (personal-library
// scale: a few thousand rows is nothing).
func (s *Server) allDocuments() ([]documentJSON, error) {
	rows, err := s.conn.Query("SELECT " + docColumns + " FROM documents ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tags := s.tagsByDoc(-1)
	var docs []documentJSON
	for rows.Next() {
		d, err := scanDocRow(rows)
		if err != nil {
			return nil, err
		}
		doc := d.toJSON()
		doc.Tags = tags[doc.ID]
		if doc.Tags == nil {
			doc.Tags = []string{}
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

// categories lists the distinct user categories with document counts.
func (s *Server) categories(w http.ResponseWriter, r *http.Request) {
	rows, err := s.conn.Query(`
SELECT category, COUNT(*) FROM documents
WHERE category != '' GROUP BY category ORDER BY category`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var cat string
		var n int
		rows.Scan(&cat, &n)
		out = append(out, map[string]any{"category": cat, "documents": n})
	}
	writeJSON(w, 200, out)
}

// renameCategory renames a category everywhere and moves its whole
// subtree with it: {"from":"machine-learning",
// "to":"computer-science/machine-learning"} turns category
// "machine-learning/transformers" into
// "computer-science/machine-learning/transformers". Renaming onto an
// existing shelf merges the two. User-pinned shelves move too — this is
// the user reorganizing their shelving, not the model re-filing.
func (s *Server) renameCategory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	from := strings.Trim(body.From, "/")
	to := strings.Trim(body.To, "/")
	if from == "" || to == "" {
		writeErr(w, 400, "from and to are required")
		return
	}
	if to == from || strings.HasPrefix(to, from+"/") {
		writeErr(w, 400, "cannot nest a category inside itself")
		return
	}
	tx, err := s.conn.Begin()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE documents SET category=? WHERE category=?", to, from)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	// subtree: "<from>/x..." -> "<to>/x..." (substr is 1-based; the
	// second half keeps the "/x..." part)
	res, err = tx.Exec(
		"UPDATE documents SET category=?||substr(category,?) WHERE substr(category,1,?)=?",
		to, len(from)+1, len(from)+1, from+"/")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	n2, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"from": from, "to": to, "updated": n + n2})
}

// ------------------------------- collections -------------------------------
//
// Collections are user-managed groups of documents (research projects): a
// paper can be in several at once. Membership is entirely manual — nothing
// in the processing pipeline adds or removes documents.

func (s *Server) listCollections(w http.ResponseWriter, r *http.Request) {
	cols, err := db.ListCollections(s.conn)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, cols)
}

func (s *Server) createCollection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name, Description string
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, 400, "collection name is required")
		return
	}
	c, err := db.CreateCollection(s.conn, name, strings.TrimSpace(body.Description))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, 409, "a collection named "+name+" already exists")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, c)
}

// getCollection returns one collection plus its documents (as full
// documentJSON rows, so the UI can render them with the shared tree rows).
func (s *Server) getCollection(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad collection id")
		return
	}
	var c db.Collection
	err = s.conn.QueryRow(
		"SELECT id, name, description FROM collections WHERE id=?", id).
		Scan(&c.ID, &c.Name, &c.Description)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "no such collection")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	docIDs, err := db.CollectionDocIDs(s.conn, id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	c.Documents = len(docIDs)
	byID := map[int64]documentJSON{}
	docs, err := s.allDocuments()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, d := range docs {
		byID[d.ID] = d
	}
	out := []documentJSON{}
	for _, docID := range docIDs {
		if d, ok := byID[docID]; ok {
			out = append(out, d)
		}
	}
	writeJSON(w, 200, map[string]any{"collection": c, "documents": out})
}

func (s *Server) patchCollection(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad collection id")
		return
	}
	var body struct {
		Name, Description *string
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	var name, desc string
	if err := s.conn.QueryRow(
		"SELECT name, description FROM collections WHERE id=?", id).
		Scan(&name, &desc); err == sql.ErrNoRows {
		writeErr(w, 404, "no such collection")
		return
	}
	if body.Name != nil {
		name = strings.TrimSpace(*body.Name)
	}
	if body.Description != nil {
		desc = strings.TrimSpace(*body.Description)
	}
	if name == "" {
		writeErr(w, 400, "collection name is required")
		return
	}
	if err := db.UpdateCollection(s.conn, id, name, desc); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, 409, "a collection named "+name+" already exists")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	s.getCollection(w, r)
}

func (s *Server) deleteCollection(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad collection id")
		return
	}
	res, err := s.conn.Exec("DELETE FROM collections WHERE id=?", id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 404, "no such collection")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (s *Server) addCollectionDocs(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad collection id")
		return
	}
	var body struct {
		DocIDs []int64 `json:"doc_ids"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	var exists int
	if err := s.conn.QueryRow(
		"SELECT COUNT(*) FROM collections WHERE id=?", id).Scan(&exists); err != nil || exists == 0 {
		writeErr(w, 404, "no such collection")
		return
	}
	added, err := db.AddDocsToCollection(s.conn, id, body.DocIDs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"added": added})
}

func (s *Server) removeCollectionDoc(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad collection id")
		return
	}
	docID, err := strconv.ParseInt(r.PathValue("docID"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad document id")
		return
	}
	if err := db.RemoveDocFromCollection(s.conn, id, docID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"removed": docID})
}

// exportCollection streams a collection as a shareable .zip (manifest +
// the documents' original files). See internal/collection.
func (s *Server) exportCollection(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad collection id")
		return
	}
	var name string
	err = s.conn.QueryRow("SELECT name FROM collections WHERE id=?", id).Scan(&name)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "no such collection")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// headers must be set before the stream starts (they can't change after)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", "collection-"+collection.Slug(name)+".zip"))
	if _, err := collection.Export(s.conn, id, w); err != nil {
		return // headers are out; the abbreviated zip is the only signal
	}
}

// importCollection accepts a bundle from exportCollection (raw .zip body),
// extracts + ingests it, and recreates the collection. Loopback-only.
func (s *Server) importCollection(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		host = strings.Trim(host, "[]")
	}
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		writeErr(w, 403, "collection import is loopback-only")
		return
	}
	if r.ContentLength <= 0 {
		writeErr(w, 400, "empty upload — POST the collection .zip as the request body")
		return
	}
	res, err := collection.Import(r.Context(), s.cfg, s.conn, r.Body,
		strings.TrimSpace(r.URL.Query().Get("name")))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"collection": res.Collection, "documents": res.Documents, "dir": res.Dir})
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

	collections, err := db.CollectionsOfDoc(s.conn, id)
	if err != nil {
		collections = []db.Collection{}
	}

	writeJSON(w, 200, map[string]any{
		"document": doc, "chunks": chunks, "tag_sources": sources,
		"collections": collections})
}

func (s *Server) patchDocument(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad document id")
		return
	}
	var body struct {
		Title, Authors, Year, Summary, Kind, Category *string
		SkipPages                                     *string
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	set := map[string]string{}
	for col, p := range map[string]*string{
		"title": body.Title, "authors": body.Authors, "year": body.Year,
		"summary": body.Summary, "kind": body.Kind, "category": body.Category,
		"skip_pages": body.SkipPages} {
		if p != nil {
			set[col] = *p
		}
	}
	if len(set) == 0 {
		writeErr(w, 400, "nothing to update (title/authors/year/summary)")
		return
	}
	if body.SkipPages != nil && *body.SkipPages != "" {
		if _, err := parsePageList(*body.SkipPages); err != nil {
			writeErr(w, 400, "bad skip_pages: "+err.Error())
			return
		}
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
	// a user-provided kind is sticky: processing only re-classifies
	// documents the user has not overrode
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
	if _, hasKind := set["kind"]; hasKind {
		s.conn.Exec("UPDATE documents SET kind_user=1 WHERE id=?", id)
	}
	if _, hasCategory := set["category"]; hasCategory {
		s.conn.Exec("UPDATE documents SET category_user=1 WHERE id=?", id)
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

// deleteDocument removes a document from the library index. The FILE on
// disk is never touched (files are indexed in place) — only the index
// rows go: chunks and tags cascade (ON DELETE CASCADE), the FTS rows
// follow via the chunks_ad trigger, and the cached cover PNG is dropped.
func (s *Server) deleteDocument(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad document id")
		return
	}
	var path string
	err = s.conn.QueryRow("SELECT path FROM documents WHERE id=?", id).Scan(&path)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "no such document")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := s.conn.Exec("DELETE FROM documents WHERE id=?", id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	os.Remove(filepath.Join(s.cfg.BaseDir, "covers", fmt.Sprintf("cover-%d.png", id)))
	writeJSON(w, 200, map[string]any{"deleted": id, "path": path})
}

// fsList backs the ingest file picker: lists ONE directory's entries
// (no file contents are served). Local-machine tool: the request must
// come from loopback even when serve was bound to a wider interface.
func (s *Server) fsList(w http.ResponseWriter, r *http.Request) {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host != "127.0.0.1" && host != "::1" {
		writeErr(w, 403, "filesystem browsing is loopback-only")
		return
	}
	dir := r.URL.Query().Get("path")
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = home
		}
	}
	if !filepath.IsAbs(dir) {
		writeErr(w, 400, "path must be absolute")
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	type entry struct {
		Name      string `json:"name"`
		Dir       bool   `json:"dir"`
		Supported bool   `json:"supported"`
		Size      int64  `json:"size,omitempty"`
	}
	out := struct {
		Path    string  `json:"path"`
		Parent  string  `json:"parent,omitempty"`
		Entries []entry `json:"entries"`
	}{Path: dir, Entries: []entry{}}
	if parent := filepath.Dir(dir); parent != dir {
		out.Parent = parent
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue // dotfiles: not interesting in a library picker
		}
		full := filepath.Join(dir, e.Name())
		en := entry{Name: e.Name(), Dir: e.IsDir()}
		if !e.IsDir() {
			en.Supported = extract.Supported(full)
			if fi, err := e.Info(); err == nil {
				en.Size = fi.Size()
			}
		}
		out.Entries = append(out.Entries, en)
	}
	writeJSON(w, 200, out)
}

var reSkipPagePart = regexp.MustCompile(`^[0-9]{1,4}(-[0-9]{1,4})?$`)

// parsePageList validates a page csv like \x223,7-12\x22 (shared by the
// skip_pages PATCH).
func parsePageList(s string) ([]int, error) {
	out := []int{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty page in %q", s)
		}
		if !reSkipPagePart.MatchString(part) {
			return nil, fmt.Errorf("bad page expression %q", part)
		}
		lo, hi := part, part
		if a, b, ok := strings.Cut(part, "-"); ok {
			lo, hi = a, b
		}
		low, _ := strconv.Atoi(lo)
		high, _ := strconv.Atoi(hi)
		if high < low {
			return nil, fmt.Errorf("bad page range %q", part)
		}
		for pp := low; pp <= high && pp < 10000; pp++ {
			out = append(out, pp)
		}
	}
	return out, nil
}

// vellumParseStoredPages decodes the ocr_done_pages csv ("all"/"" -> nil).
func vellumParseStoredPages(s string) []int {
	if s == "all" || s == "" {
		return nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && n >= 1 {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// vellumStoreDonePages merges page numbers into a sorted csv string.
func vellumStoreDonePages(pages []int) string {
	set := map[int]bool{}
	for _, p := range pages {
		set[p] = true
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	parts := []string{}
	for _, p := range out {
		parts = append(parts, strconv.Itoa(p))
	}
	return strings.Join(parts, ",")
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
	label := fmt.Sprintf("ingest %d path(s)", len(body.Paths))
	var ingestStats *ingest.Stats
	job := s.runJob("ingest", label, func(j *Job) error {
		var err error
		ingestStats, err = ingest.Ingest(j.ctx, s.cfg, s.conn, body.Paths,
			body.Reprocess,
			func(msg string) { s.jobProgress(j, msg) })
		return err
	})
	st := ingestStats
	if st == nil {
		st = &ingest.Stats{Cancelled: job.Status == "cancelled"}
	} else if job.Status == "cancelled" {
		st.Cancelled = true
	}
	writeJSON(w, 200, st)
}

// cover serves page 1 as a PNG — the item's "cover image" for the
// metadata view. Rendered lazily on first request and cached under
// <basedir>/covers/; ingest of a changed file drops the cache entry.
func (s *Server) cover(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var path string
	err = s.conn.QueryRow("SELECT path FROM documents WHERE id=?", id).Scan(&path)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "no such document")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if extract.KindOf(filepath.Ext(path)) != "pdf" {
		writeErr(w, 404, "no cover for "+filepath.Ext(path)+" files")
		return
	}
	cacheDir := filepath.Join(s.cfg.BaseDir, "covers")
	cache := filepath.Join(cacheDir, fmt.Sprintf("cover-%d.png", id))
	if fi, err := os.Stat(cache); err != nil || fi.Size() == 0 {
		if err := extract.RenderPagePNG(s.cfg, path, 1, 100, cache); err != nil {
			writeErr(w, 404, "cover render failed: "+err.Error())
			return
		}
	}
	f, err := os.Open(cache)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, "cover.png", fi.ModTime(), f)
}

// cover render lives in extract (mutool ownership).
// reextract re-runs text extraction on a document's source file: with
// {"force":true} the raster is OCR'd on EVERY page (the repair path
// for garbled embedded text layers), otherwise only broken-looking
// pages are re-OCR'd. Text is replaced in place; the document lands
// pending (status=ingested) so it is re-summarized and re-tagged.
func (s *Server) reextract(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var body struct {
		Force bool    `json:"force"`
		Pages []int64 `json:"pages"`
	}
	if r.ContentLength > 0 {
		if err := decodeBody(r, &body); err != nil {
			writeErr(w, 400, "bad JSON body: "+err.Error())
			return
		}
	}
	var path string
	var donePages string
	err = s.conn.QueryRow("SELECT path, ocr_done_pages FROM documents WHERE id=?", id).
		Scan(&path, &donePages)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "no such document")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := os.Stat(path); err != nil {
		writeErr(w, 409, "source file missing: "+path)
		return
	}
	label := fmt.Sprintf("re-extract #%d", id)
	var res *extract.Result
	var extractErr error
	s.runJob("reextract", label, func(j *Job) error {
		progress := func(msg string) { s.jobProgress(j, msg) }
		switch {
		case body.Force && len(body.Pages) > 0:
			// per-page repair: the named pages + any previously repaired
			// pages stay OCR-backed after the fresh text pass
			progress(fmt.Sprintf(
				"re-extracting text (OCR forced on %d page(s))", len(body.Pages)))
			var want []int
			for _, p := range body.Pages {
				if p >= 1 {
					want = append(want, int(p))
				}
			}
			for _, p := range vellumParseStoredPages(donePages) {
				want = append(want, p)
			}
			if res, err = extract.ExtractOCRPages(path, s.cfg, true, want); err != nil {
				return err
			}
			s.conn.Exec("UPDATE documents SET ocr_done_pages=? WHERE id=?",
				vellumStoreDonePages(want), id)
		case body.Force:
			progress("re-extracting text (OCR forced on every page)")
			res, err = extract.ExtractOCR(path, s.cfg)
			if err == nil {
				s.conn.Exec("UPDATE documents SET ocr_done_pages='all' WHERE id=?", id)
			}
		default:
			progress("re-extracting text")
			res, err = extract.Extract(path, s.cfg)
		}
		if err != nil {
			return err
		}
		if err := db.ReplaceDocumentText(s.conn, id, res.Chunks); err != nil {
			return err
		}
		var kindUser int64
		s.conn.QueryRow("SELECT kind_user FROM documents WHERE id=?", id).Scan(&kindUser)
		if kindUser == 0 && len(res.Chunks) > 0 {
			text := ""
			var sb strings.Builder
			for _, c := range res.Chunks {
				sb.WriteString(c.Text)
				sb.WriteString("\n\n")
				if sb.Len() > 200000 {
					break
				}
			}
			text = sb.String()
			newKind, _ := classify.Detect(text, res.OCRPages, len(res.Chunks))
			s.conn.Exec("UPDATE documents SET kind=?, kind_user=(SELECT kind_user FROM documents WHERE id=?), ocr_pages=?, n_pages=?, ocr_pending=0, status='ingested', error=NULL, processed_at=NULL WHERE id=?",
				newKind, id, res.OCRPages, len(res.Chunks), id)
		} else {
			s.conn.Exec("UPDATE documents SET ocr_pages=?, n_pages=?, ocr_pending=0, status='ingested', error=NULL, processed_at=NULL WHERE id=?",
				res.OCRPages, len(res.Chunks), id)
		}
		return nil
	})
	if extractErr != nil {
		writeErr(w, 500, extractErr.Error())
		return
	}
	s.document(w, r)
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
	if !llm.AvailableFor(s.cfg.LLM.Backend, s.cfg.Tools.LLMURL) {
		writeErr(w, 503, "no chat backend at "+s.cfg.Tools.LLMURL+
			" (llm.backend="+s.cfg.LLM.Backend+") — start the vellum launcher or your own llama.cpp/ollama")
		return
	}
	label := "process pending"
	if len(body.IDs) > 0 {
		label = fmt.Sprintf("process %d document(s)", len(body.IDs))
	}
	var results []ingest.ProcessResult
	var processErr error
	cancelled := false
	s.runJob("process", label, func(j *Job) error {
		results, processErr = ingest.ProcessPending(j.ctx, s.cfg, s.conn, v,
			body.IDs, body.Limit,
			func(msg string) { s.jobProgress(j, msg) })
		if processErr != nil || j.Stopped() {
			cancelled = cancelled || j.Stopped() ||
				errors.Is(processErr, summarize.ErrCancelled)
		}
		if processErr == nil && j.Stopped() {
			return errJobCancelled
		}
		return processErr
	})
	_ = cancelled
	if processErr != nil && !cancelled &&
		!errors.Is(processErr, summarize.ErrCancelled) {
		writeErr(w, 500, processErr.Error())
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

	filter := parseFilter(r)
	var allowed map[int64]bool
	if !filter.empty() {
		docs, err := s.allDocuments()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		allowed = map[int64]bool{}
		for _, d := range docs {
			if filter.match(d) {
				allowed[d.ID] = true
			}
		}
	}

	if mode == "semantic" {
		hits, err := search.Semantic(s.cfg, s.conn, q, limit)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if allowed != nil {
			kept := hits[:0]
			for _, h := range hits {
				if allowed[h.DocID] {
					kept = append(kept, h)
				}
			}
			hits = kept
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
	if allowed != nil {
		kept := hits[:0]
		for _, h := range hits {
			if allowed[h.DocID] {
				kept = append(kept, h)
			}
		}
		hits = kept
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

// ------------------------------- tags (cloud + per-tag pages) -------------

// tagInfo is one entry of the Tags page / cloud.
type tagInfo struct {
	Tag         string `json:"tag"`
	Documents   int    `json:"documents"`
	Description string `json:"description"`
	Suggested   bool   `json:"suggested"`
}

// listTags returns every tag known to the library — the controlled
// vocabulary, tags actually applied, and LLM-proposed ones — with usage
// counts and descriptions.
func (s *Server) listTags(w http.ResponseWriter, r *http.Request) {
	counts := map[string]int{}
	if rows, err := s.conn.Query(
		"SELECT tag, COUNT(DISTINCT doc_id) FROM doc_tags GROUP BY tag"); err == nil {
		for rows.Next() {
			var t string
			var n int
			rows.Scan(&t, &n)
			counts[t] = n
		}
		rows.Close()
	}
	suggested := map[string]bool{}
	if rows, err := s.conn.Query(
		"SELECT DISTINCT tag FROM doc_tags WHERE source='suggested'"); err == nil {
		for rows.Next() {
			var t string
			rows.Scan(&t)
			suggested[t] = true
		}
		rows.Close()
	}
	info := map[string]*tagInfo{}
	add := func(tag string) *tagInfo {
		if ti, ok := info[tag]; ok {
			return ti
		}
		ti := &tagInfo{Tag: tag, Suggested: suggested[tag]}
		info[tag] = ti
		return ti
	}
	if v, err := vocab.Load(s.cfg.VocabPath); err == nil {
		for name, desc := range v.Tags {
			add(name).Description = desc
		}
	}
	for tag, n := range counts {
		add(tag).Documents = n
	}
	out := make([]tagInfo, 0, len(info))
	for _, ti := range info {
		out = append(out, *ti)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Documents != out[j].Documents {
			return out[i].Documents > out[j].Documents
		}
		return out[i].Tag < out[j].Tag
	})
	writeJSON(w, 200, out)
}

// getTag returns one tag with its documents (for a [Tag] page).
func (s *Server) getTag(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	if unescaped, err := url.PathUnescape(tag); err == nil {
		tag = unescaped
	}
	if tag == "" {
		writeErr(w, 400, "tag required")
		return
	}
	desc := ""
	if v, err := vocab.Load(s.cfg.VocabPath); err == nil {
		desc = v.Tags[tag]
	}
	docs, err := s.allDocuments()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	matched := []documentJSON{}
	for _, d := range docs {
		for _, t := range d.Tags {
			if strings.EqualFold(t, tag) {
				matched = append(matched, d)
				break
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"tag": tag, "description": desc, "documents": matched})
}

// ------------------------------- notes (scratchpad) -----------------------

func (s *Server) listNotes(w http.ResponseWriter, r *http.Request) {
	notes, err := db.ListNotes(s.conn)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, notes)
}

func (s *Server) createNote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Body string `json:"body"`
	}
	if r.ContentLength > 0 {
		if err := decodeBody(r, &body); err != nil {
			writeErr(w, 400, "bad JSON body: "+err.Error())
			return
		}
	}
	n, err := db.CreateNote(s.conn, body.Body)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, n)
}

func (s *Server) getNote(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad note id")
		return
	}
	n, err := db.GetNote(s.conn, id)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "no such note")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, n)
}

func (s *Server) patchNote(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad note id")
		return
	}
	var body struct {
		Body *string `json:"body"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	if body.Body == nil {
		writeErr(w, 400, "body required")
		return
	}
	n, err := db.UpdateNote(s.conn, id, *body.Body)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "no such note")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, n)
}

func (s *Server) deleteNote(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad note id")
		return
	}
	res, err := s.conn.Exec("DELETE FROM notes WHERE id=?", id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 404, "no such note")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

// ------------------------------- ask (LLM chat about an item) ------------

// getAskConfig returns the ask configuration with the API key masked
// (the UI needs to know a key exists, not the key).
func (s *Server) getAskConfig(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Ask
	writeJSON(w, 200, map[string]any{
		"provider": c.Provider, "model": c.Model,
		"base_url": c.BaseURL, "key_set": c.APIKey != "",
		"enabled": c.Enabled(),
	})
}

// putAskConfig updates + saves the ask configuration. An empty api_key
// in the request keeps the stored one (users do not retype it).
func (s *Server) putAskConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider *string `json:"provider"`
		Model    *string `json:"model"`
		APIKey   *string `json:"api_key"`
		BaseURL  *string `json:"base_url"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	c := &s.cfg.Ask
	// present fields are applied (so an empty model/base_url CLEARS them);
	// an omitted api_key keeps the stored one (users don't retype it).
	if body.Provider != nil && *body.Provider != "" {
		c.Provider = *body.Provider
	}
	if body.Model != nil {
		c.Model = *body.Model
	}
	if body.BaseURL != nil {
		c.BaseURL = *body.BaseURL
	}
	if body.APIKey != nil && *body.APIKey != "" {
		c.APIKey = *body.APIKey
	}
	if err := s.cfg.Save(); err != nil {
		writeErr(w, 500, "config save failed: "+err.Error())
		return
	}
	// return the SAME shape as GET (provider/model/base_url/key_set/enabled):
	// the UI caches this response, so a partial object corrupted its state.
	s.getAskConfig(w, r)
}

// regenerate rebuilds individual metadata fields in place (no full
// reprocess): fields ⊆ {"meta","summary","tags","category","kind"}.
func (s *Server) regenerate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var body struct {
		Fields []string `json:"fields"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	allowed := map[string]bool{"meta": true, "summary": true,
		"tags": true, "category": true, "kind": true}
	fields := []string{}
	for _, f := range body.Fields {
		if allowed[f] {
			fields = append(fields, f)
		}
	}
	if len(fields) == 0 {
		writeErr(w, 400,
			`fields[] ⊆ ["meta","summary","tags","category","kind"] required`)
		return
	}
	v, err := vocab.Load(s.cfg.VocabPath)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	needLLM := false
	for _, f := range fields {
		if f != "kind" {
			needLLM = true
		}
	}
	if needLLM && len(v.Tags) == 0 && (slicesContains(fields, "tags") || slicesContains(fields, "category")) {
		writeErr(w, 400, "vocab.yaml is empty — add tags first")
		return
	}
	if needLLM && !llm.AvailableFor(s.cfg.LLM.Backend, s.cfg.Tools.LLMURL) {
		writeErr(w, 503, "no chat backend at "+s.cfg.Tools.LLMURL+
			" (llm.backend="+s.cfg.LLM.Backend+")")
		return
	}
	label := "regenerate #" + strconv.FormatInt(id, 10) +
		" (" + strings.Join(fields, ", ") + ")"
	var regenErr error
	var cancelled bool
	s.runJob("regenerate", label, func(j *Job) error {
		applied, err := ingest.Regenerate(j.ctx, s.cfg, s.conn, v, id, fields,
			func(msg string) { s.jobProgress(j, msg) })
		_ = applied
		if errors.Is(err, summarize.ErrCancelled) || j.Stopped() {
			cancelled = true
			return nil
		}
		regenErr = err
		return err
	})
	_ = cancelled
	if regenErr != nil && !cancelled {
		writeErr(w, 500, regenErr.Error())
		return
	}
	s.document(w, r)
}

func slicesContains(xs []string, x string) bool {
	for _, e := range xs {
		if e == x {
			return true
		}
	}
	return false
}

// testAsk verifies the ask provider configuration with a tiny exchange.
// With a JSON body the UNSTORED values are tested (the UI's pre-save
// check); with no body the current configuration is.
func (s *Server) testAsk(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Ask
	if r.ContentLength > 0 {
		var body struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			APIKey   string `json:"api_key"`
			BaseURL  string `json:"base_url"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeErr(w, 400, "bad JSON body: "+err.Error())
			return
		}
		if body.Provider != "" || body.Model != "" {
			c = ask.Config{Provider: body.Provider, Model: body.Model,
				APIKey: body.APIKey, BaseURL: body.BaseURL}
		}
	}
	if err := c.Test(); err != nil {
		writeErr(w, 502, "ask test failed: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// postAsk streams a chat answer about one document (SSE). The system
// context carries the document's metadata + summary + opening text;
// the client's turns are passed through as-is.
func (s *Server) postAsk(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var body struct {
		Messages []ask.Message `json:"messages"`
		// quick-try overrides (usable before saving the config)
		Provider string `json:"provider"`
		Model    string `json:"model"`
		APIKey   string `json:"api_key"`
		BaseURL  string `json:"base_url"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	var title, authors, year, summary, kind string
	var nTitle, nAuthors, nYear, nSummary, nKind sql.NullString
	err = s.conn.QueryRow(
		"SELECT title, authors, year, summary, kind FROM documents WHERE id=?", id).
		Scan(&nTitle, &nAuthors, &nYear, &nSummary, &nKind)
	title, authors, year, summary, kind =
		nTitle.String, nAuthors.String, nYear.String, nSummary.String, nKind.String
	if err == sql.ErrNoRows {
		writeErr(w, 404, "no such document")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// opening text: the first chunks (title page/introduction carry the
	// document's voice); ~6k chars keeps the request cheap for any
	// provider
	opening, err := db.DocumentText(s.conn, id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if len(opening) > 6000 {
		opening = opening[:6000]
	}
	sys := "You are answering questions about one specific document in the " +
		"user's personal library. Use the provided material; say plainly " +
		"when something is outside it. Answer concisely.\n\n" +
		"Document metadata: title=" + title + "; authors=" + authors +
		"; year=" + year + "; kind=" + kind + "\n"
	if summary != "" {
		sys += "Summary:\n" + summary + "\n"
	}
	if opening != "" {
		sys += "\nOpening text (truncated):\n" + opening
	}
	out := s.cfg.Ask
	if body.Provider != "" {
		out = ask.Config{Provider: body.Provider, Model: body.Model,
			APIKey: body.APIKey, BaseURL: body.BaseURL}
	}
	deltas, err := out.Stream(sys, body.Messages)
	if err != nil {
		// surface the provider's own error (e.g. "connection refused",
		// "401 Unauthorized", a 400 JSON body) — the UI shows this text
		writeErr(w, 502, "ask provider: "+err.Error())
		return
	}
	flush, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	for d := range deltas {
		if d.Error != "" {
			sseSend(w, map[string]string{"e": d.Error})
			break
		}
		sseSend(w, map[string]string{"d": d.Text})
	}
	sseSend(w, map[string]string{"done": "1"})
	flush.Flush()
}

// sseSend writes one SSE event line-pair.
func sseSend(w http.ResponseWriter, payload any) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// ------------------------------- configuration ---------------------------

// getConfig returns the editable subset for the Settings dialog (the
// api keys are masked).
func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	c := s.cfg
	type askOut struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		BaseURL  string `json:"base_url"`
		KeySet   bool   `json:"key_set"`
	}
	writeJSON(w, 200, map[string]any{
		"llm": map[string]any{
			"backend":     c.LLM.Backend,
			"model":       c.LLM.Model,
			"external":    c.LLM.External,
			"think":       c.LLM.Think,
			"temperature": c.LLM.Temperature,
			"num_ctx":     c.LLM.NumCtx,
			"url":         c.Tools.LLMURL,
		},
		"embed": map[string]any{
			"provider": c.Embed.Provider,
			"model":    c.Embed.Model,
			"external": c.Embed.External,
			"batch":    c.Embed.Batch,
			"url":      c.Tools.EmbedURL,
		},
		"ocr": map[string]any{
			"langs":              c.OCR.Langs,
			"dpi":                c.OCR.DPI,
			"workers":            c.OCR.Workers,
			"min_chars_per_page": c.OCR.MinCharsPerPage,
		},
		"summarize": map[string]any{
			"chunk_chars": c.Summarize.ChunkChars,
			"max_tags":    c.Summarize.MaxTags,
		},
		"ask": askOut{
			Provider: c.Ask.Provider, Model: c.Ask.Model,
			BaseURL: c.Ask.BaseURL, KeySet: c.Ask.APIKey != "",
		},
		"theme": map[string]any{
			"preset": c.Theme.Preset,
			"colors": c.Theme.Colors,
		},
	})
}

// putConfig applies settings IMMEDIATELY to the running server (in
// memory) and persists to config.yaml. Values absent from the request
// keep their current settings. Server LIFECYCLE is the one thing that
// cannot take effect this way: switching to/from a bundled backend
// needs the launcher rerun (`./vellum.sh serve` is idempotent and
// won't duplicate servers).
func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LLM struct {
			Backend     *string  `json:"backend"`
			Model       *string  `json:"model"`
			External    *bool    `json:"external"`
			Think       *bool    `json:"think"`
			Temperature *float64 `json:"temperature"`
			NumCtx      *int     `json:"num_ctx"`
			URL         *string  `json:"url"`
		} `json:"llm"`
		Embed struct {
			Provider *string `json:"provider"`
			External *bool   `json:"external"`
			Model    *string `json:"model"`
			Batch    *int    `json:"batch"`
			URL      *string `json:"url"`
		} `json:"embed"`
		OCR struct {
			Langs           *string `json:"langs"`
			DPI             *int    `json:"dpi"`
			Workers         *int    `json:"workers"`
			MinCharsPerPage *int    `json:"min_chars_per_page"`
		} `json:"ocr"`
		Summarize struct {
			ChunkChars *int `json:"chunk_chars"`
			MaxTags    *int `json:"max_tags"`
		} `json:"summarize"`
		Ask struct {
			Provider *string `json:"provider"`
			Model    *string `json:"model"`
			APIKey   *string `json:"api_key"`
			BaseURL  *string `json:"base_url"`
		} `json:"ask"`
		Theme struct {
			Preset *string           `json:"preset"`
			Colors map[string]string `json:"colors"`
		} `json:"theme"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	c := s.cfg
	b := body.LLM
	if b.Backend != nil && (*b.Backend == "llama-server" || *b.Backend == "ollama") {
		c.LLM.Backend = *b.Backend
	}
	if b.Model != nil {
		c.LLM.Model = *b.Model
	}
	if b.External != nil {
		c.LLM.External = *b.External
	}
	if b.Think != nil {
		c.LLM.Think = *b.Think
	}
	if b.Temperature != nil && *b.Temperature >= 0 && *b.Temperature <= 2 {
		c.LLM.Temperature = *b.Temperature
	}
	if b.NumCtx != nil && *b.NumCtx >= 1024 && *b.NumCtx <= 1024*1024 {
		c.LLM.NumCtx = *b.NumCtx
	}
	if b.URL != nil {
		c.Tools.LLMURL = *b.URL
	}
	b2 := body.Embed
	if b2.Provider != nil && (*b2.Provider == "llama-server" || *b2.Provider == "ollama") {
		c.Embed.Provider = *b2.Provider
	}
	if b2.External != nil {
		c.Embed.External = *b2.External
	}
	if b2.Model != nil {
		c.Embed.Model = *b2.Model
	}
	if b2.Batch != nil && *b2.Batch >= 1 && *b2.Batch <= 512 {
		c.Embed.Batch = *b2.Batch
	}
	if b2.URL != nil {
		c.Tools.EmbedURL = *b2.URL
	}
	o := body.OCR
	if o.Langs != nil && *o.Langs != "" {
		c.OCR.Langs = *o.Langs
	}
	if o.DPI != nil && *o.DPI >= 72 && *o.DPI <= 1200 {
		c.OCR.DPI = *o.DPI
	}
	if o.Workers != nil && *o.Workers >= 1 && *o.Workers <= 32 {
		c.OCR.Workers = *o.Workers
	}
	if o.MinCharsPerPage != nil && *o.MinCharsPerPage >= 0 && *o.MinCharsPerPage <= 10000 {
		c.OCR.MinCharsPerPage = *o.MinCharsPerPage
	}
	sm := body.Summarize
	if sm.ChunkChars != nil && *sm.ChunkChars >= 500 && *sm.ChunkChars <= 50000 {
		c.Summarize.ChunkChars = *sm.ChunkChars
	}
	if sm.MaxTags != nil && *sm.MaxTags >= 1 && *sm.MaxTags <= 32 {
		c.Summarize.MaxTags = *sm.MaxTags
	}
	a := body.Ask
	if a.Provider != nil && *a.Provider != "" {
		c.Ask.Provider = *a.Provider
	}
	if a.Model != nil && *a.Model != "" {
		c.Ask.Model = *a.Model
	}
	if a.APIKey != nil && *a.APIKey != "" {
		c.Ask.APIKey = *a.APIKey
	}
	if a.BaseURL != nil {
		c.Ask.BaseURL = *a.BaseURL
	}
	if body.Theme.Preset != nil && *body.Theme.Preset != "" {
		c.Theme.Preset = *body.Theme.Preset
	}
	if body.Theme.Colors != nil {
		c.Theme.Colors = body.Theme.Colors
	}
	// persist
	if err := s.cfg.Save(); err != nil {
		writeErr(w, 500, "config save failed: "+err.Error())
		return
	}
	s.getConfig(w, r)
}

// ------------------------------- library backup ---------------------------

// exportLibrary streams a consistent snapshot of the whole library
// (VACUUM INTO a temp copy, served as a download). Safe any time — even
// while processing runs, VACUUM INTO reads a consistent snapshot.
func (s *Server) exportLibrary(w http.ResponseWriter, r *http.Request) {
	tmp, err := os.CreateTemp("", "vellum-export-*.db")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	name := tmp.Name()
	tmp.Close()
	os.Remove(name) // VACUUM INTO requires a NOT-yet-existing target
	defer os.Remove(name)
	if _, err := s.conn.Exec(
		"VACUUM INTO '" + strings.ReplaceAll(name, "'", "''") + "'"); err != nil {
		writeErr(w, 500, "snapshot failed: "+err.Error())
		return
	}
	f, err := os.Open(name)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition",
		"attachment; filename=\"vellum-library-"+time.Now().Format("20060102-150405")+".db\"")
	http.ServeContent(w, r, "vellum-library.db", time.Now(), f)
}

// importLibrary replaces the live library with an uploaded vellum
// database (the request body IS the backup file). The current library is
// moved aside to "<db>.pre-import-<timestamp>" first, so an import is
// always recoverable. Loopback-only (like /api/fs); refuses while jobs
// are queued/running. Requests in flight during the swap may error — a
// single-user tool, the UI reloads right after.
func (s *Server) importLibrary(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		host = strings.Trim(host, "[]")
	}
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		writeErr(w, 403, "library import is loopback-only")
		return
	}
	s.jobsMu.Lock()
	busy := s.jobRunning != nil || len(s.waiters) > 0
	s.jobsMu.Unlock()
	if busy {
		writeErr(w, 503, "jobs are queued/running — cancel them or let them finish first")
		return
	}
	if r.ContentLength <= 0 {
		writeErr(w, 400, "empty upload — POST the backup database as the request body")
		return
	}
	tmp, err := os.CreateTemp("", "vellum-import-*.db")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := io.Copy(tmp, r.Body); err != nil {
		tmp.Close()
		writeErr(w, 400, "upload failed: "+err.Error())
		return
	}
	tmp.Close()
	n, err := db.ValidateLibrary(name)
	if err != nil {
		writeErr(w, 400, "not a vellum library database: "+err.Error())
		return
	}
	// swap: close the live connection (checkpoints the WAL), move the
	// current library aside, put the uploaded one in its place, reopen
	// (the reopen also migrates older libraries).
	s.conn.Close()
	os.Remove(s.cfg.DBPath + "-wal")
	os.Remove(s.cfg.DBPath + "-shm")
	backup := s.cfg.DBPath + ".pre-import-" + time.Now().Format("20060102-150405")
	if err := os.Rename(s.cfg.DBPath, backup); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := os.Rename(name, s.cfg.DBPath); err != nil {
		os.Rename(backup, s.cfg.DBPath) // put it back
		writeErr(w, 500, err.Error())
		return
	}
	newConn, err := db.Open(s.cfg.DBPath)
	if err != nil {
		// the uploaded file was rejected on open — restore the old library
		os.Remove(s.cfg.DBPath)
		os.Rename(backup, s.cfg.DBPath)
		s.conn, _ = db.Open(s.cfg.DBPath)
		writeErr(w, 500, "import failed: "+err.Error())
		return
	}
	s.conn = newConn
	writeJSON(w, 200, map[string]any{"documents": n, "backup": backup})
}

// errJobCancelled marks a cooperative cancel inside runJob work.
var errJobCancelled = errors.New("cancelled")

// ------------------------------- jobs -------------------------------------

// jobList: the queue + history for the Jobs dialog (newest last; the
// UI sorts as it likes).
func (s *Server) jobList(w http.ResponseWriter, r *http.Request) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	out := make([]*Job, 0, len(s.jobs))
	out = append(out, s.jobs...)
	writeJSON(w, 200, map[string]any{
		"jobs":    out,
		"running": s.jobRunning != nil,
	})
}

// jobCancel cancels a job: queued jobs leave the queue immediately;
// running jobs stop at their next unit of work (file/document/section/
// page). Idempotent: cancelling a finished job just reports its state.
func (s *Server) jobCancel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, "bad job id")
		return
	}
	s.jobsMu.Lock()
	var target *Job
	for _, j := range s.jobs {
		if j.ID == id {
			target = j
			break
		}
	}
	if target == nil {
		s.jobsMu.Unlock()
		writeErr(w, 404, "no such job")
		return
	}
	switch target.Status {
	case "queued":
		target.cancel() // the waiting submitter cleans up
	case "running":
		target.cancel() // cooperative: stops before the next unit
	default:
		s.jobsMu.Unlock()
		writeJSON(w, 200, map[string]any{
			"status": target.Status, "note": "already finished"})
		return
	}
	status := target.Status
	s.jobsMu.Unlock()
	writeJSON(w, 200, map[string]any{"status": status, "id": id})
}
