package api

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"vellum/internal/ingest"
	"vellum/internal/llm"
	"vellum/internal/search"
	"vellum/internal/vocab"
)

// ---------------------------------------------------------------- stats

// stats is the library overview for the Stats page: counts, breakdowns by
// kind/category/tag, and embedding coverage.
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	one := func(q string) int {
		var n int
		s.conn.QueryRow(q).Scan(&n)
		return n
	}
	type kv struct {
		Key   string `json:"key"`
		Count int    `json:"count"`
	}
	group := func(q string) []kv {
		out := []kv{}
		rows, err := s.conn.Query(q)
		if err != nil {
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int
			if rows.Scan(&k, &n) == nil {
				out = append(out, kv{Key: k, Count: n})
			}
		}
		return out
	}
	writeJSON(w, 200, map[string]any{
		"documents":   one("SELECT COUNT(*) FROM documents"),
		"done":        one("SELECT COUNT(*) FROM documents WHERE status='done'"),
		"pending":     one("SELECT COUNT(*) FROM documents WHERE status='ingested'"),
		"errors":      one("SELECT COUNT(*) FROM documents WHERE status='error'"),
		"chunks":      one("SELECT COUNT(*) FROM chunks"),
		"embedded":    one("SELECT COUNT(*) FROM chunks WHERE embedding IS NOT NULL"),
		"collections": one("SELECT COUNT(*) FROM collections"),
		"notes":       one("SELECT COUNT(*) FROM notes"),
		"chats":       one("SELECT COUNT(*) FROM chat_sessions"),
		"vocab_tags": func() int {
			v, err := vocab.Load(s.cfg.VocabPath)
			if err != nil {
				return 0
			}
			return len(v.Tags)
		}(),
		"kinds": group(`SELECT COALESCE(NULLIF(kind,''),'(none)'), COUNT(*)
			FROM documents GROUP BY kind ORDER BY COUNT(*) DESC`),
		"categories": group(`SELECT COALESCE(NULLIF(category,''),'(uncategorized)'), COUNT(*)
			FROM documents GROUP BY category ORDER BY COUNT(*) DESC`),
		"tags": group(`SELECT tag, COUNT(*) FROM doc_tags GROUP BY tag
			ORDER BY COUNT(*) DESC LIMIT 60`),
	})
}

// ---------------------------------------------------------------- bulk docs

// deleteDoc removes one document from the library (file untouched) and its
// cover cache. Returns the document's path.
func (s *Server) deleteDoc(id int64) (string, error) {
	var path string
	if err := s.conn.QueryRow("SELECT path FROM documents WHERE id=?", id).Scan(&path); err != nil {
		return "", err
	}
	if _, err := s.conn.Exec("DELETE FROM documents WHERE id=?", id); err != nil {
		return "", err
	}
	os.Remove(filepath.Join(s.cfg.BaseDir, "covers", fmt.Sprintf("cover-%d.png", id)))
	return path, nil
}

// removeDocuments removes many documents in one call (the files stay on
// disk; re-ingesting adds them back).
func (s *Server) removeDocuments(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	if len(body.IDs) == 0 {
		writeErr(w, 400, "ids[] required")
		return
	}
	removed := []int64{}
	failed := map[int64]string{}
	for _, id := range body.IDs {
		if _, err := s.deleteDoc(id); err != nil {
			if err == sql.ErrNoRows {
				failed[id] = "no such document"
			} else {
				failed[id] = err.Error()
			}
			continue
		}
		removed = append(removed, id)
	}
	writeJSON(w, 200, map[string]any{"removed": removed, "failed": failed})
}

// regenerateDocuments rebuilds the given fields for many documents in one
// job (used by the bulk action bar and the chat tool).
func (s *Server) regenerateDocuments(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs    []int64  `json:"ids"`
		Fields []string `json:"fields"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, "bad JSON body: "+err.Error())
		return
	}
	if len(body.IDs) == 0 {
		writeErr(w, 400, "ids[] required")
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
		fields = []string{"meta", "summary", "tags", "category"}
	}
	needLLM := false
	for _, f := range fields {
		if f != "kind" {
			needLLM = true
		}
	}
	if needLLM && !llm.AvailableFor(s.cfg.LLM.Backend, s.cfg.Tools.LLMURL) {
		writeErr(w, 503, "no chat backend at "+s.cfg.Tools.LLMURL+
			" (llm.backend="+s.cfg.LLM.Backend+")")
		return
	}
	v, err := vocab.Load(s.cfg.VocabPath)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ids := body.IDs
	label := fmt.Sprintf("regenerate %d document(s)", len(ids))
	s.runJob("regenerate", label, func(j *Job) error {
		for i, id := range ids {
			if j.Stopped() {
				return errJobCancelled
			}
			s.jobProgress(j, fmt.Sprintf("%d/%d: regenerate #%d", i+1, len(ids), id))
			if _, err := ingest.Regenerate(j.ctx, s.cfg, s.conn, v, id, fields,
				func(m string) { s.jobProgress(j, m) }); err != nil {
				if j.Stopped() {
					return errJobCancelled
				}
				log.Printf("regenerate #%d failed: %s", id, err)
			}
		}
		return nil
	})
	writeJSON(w, 200, map[string]any{"queued": len(ids), "fields": fields})
}

// ---------------------------------------------------------------- embed

// embedNow embeds every chunk that lacks a vector (a job, so it shows in the
// Jobs list and the status bar).
func (s *Server) embedNow(w http.ResponseWriter, r *http.Request) {
	if !llm.Available(s.cfg.Tools.EmbedURL) {
		writeErr(w, 503, "no embedding server at "+s.cfg.Tools.EmbedURL)
		return
	}
	var n int
	var embedErr error
	job := s.runJob("embed", "embed chunks", func(j *Job) error {
		n, embedErr = search.EmbedPendingCtx(j.ctx, s.cfg, s.conn)
		return embedErr
	})
	if job.Status == "cancelled" {
		writeJSON(w, 200, map[string]any{"embedded": n, "cancelled": true})
		return
	}
	if embedErr != nil {
		writeErr(w, 500, embedErr.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"embedded": n})
}
