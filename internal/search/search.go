// Package search provides FTS5 keyword search and brute-force cosine
// semantic search over chunk embeddings (fine at personal-library scale
// on CPU).
package search

import (
	"database/sql"
	"encoding/binary"
	"math"
	"strings"

	"vellum/internal/config"
	"vellum/internal/db"
	"vellum/internal/llm"
)

// KeywordHit is one FTS5 match.
type KeywordHit struct {
	DocID   int64  `json:"doc_id"`
	Title   string `json:"title"`
	Path    string `json:"path"`
	Page    int    `json:"page"`
	Snippet string `json:"snippet"`
}

// Keyword runs an FTS5 query. Queries that aren't valid FTS5 syntax
// (e.g. "C++") are retried as one literal phrase.
func Keyword(conn *sql.DB, query string, limit int) ([]KeywordHit, error) {
	const q = `
SELECT d.id, d.title, d.path, c.page_no,
       snippet(fts, 0, '[', ']', '…', 12) AS snip
FROM fts
JOIN chunks c ON c.id = fts.rowid
JOIN documents d ON d.id = c.doc_id
WHERE fts MATCH ?
ORDER BY bm25(fts)
LIMIT ?`

	rows, err := conn.Query(q, query, limit)
	if err != nil {
		quoted := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
		rows, err = conn.Query(q, quoted, limit)
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()

	var hits []KeywordHit
	for rows.Next() {
		var h KeywordHit
		var page sql.NullInt64
		if err := rows.Scan(&h.DocID, &h.Title, &h.Path, &page, &h.Snippet); err != nil {
			return nil, err
		}
		h.Page = int(page.Int64)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// SemanticHit is one document-grouped semantic result.
type SemanticHit struct {
	DocID    int64        `json:"doc_id"`
	Title    string       `json:"title"`
	Path     string       `json:"path"`
	Snippets []SemSnippet `json:"snippets"`
}

// SemSnippet is one matching chunk.
type SemSnippet struct {
	Page  int     `json:"page"`
	Text  string  `json:"text"`
	Score float64 `json:"score"`
}

// Semantic embeds the query and ranks chunks by cosine similarity, grouping
// per document. Pending chunks are embedded lazily so search is never
// stale.
func Semantic(cfg *config.Config, conn *sql.DB, query string, k int) ([]SemanticHit, error) {
	if _, err := EmbedPending(cfg, conn); err != nil {
		return nil, err
	}
	vecs, err := llm.Embed(cfg.Tools.EmbedURL, []string{query})
	if err != nil {
		return nil, err
	}
	qvec := vecs[0]

	rows, err := conn.Query(`
SELECT c.id, c.doc_id, c.page_no, c.embedding, c.text, d.title, d.path
FROM chunks c JOIN documents d ON d.id = c.doc_id
WHERE c.embedding IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type scored struct {
		docID, id int64
		page      int
		title     string
		path      string
		text      string
		score     float64
	}
	var all []scored
	for rows.Next() {
		var (
			docID, id int64
			page      sql.NullInt64
			emb       []byte
			text      string
			title     string
			path      string
		)
		if err := rows.Scan(&id, &docID, &page, &emb, &text, &title, &path); err != nil {
			return nil, err
		}
		s := scored{
			docID: docID, id: id, page: int(page.Int64),
			title: title, path: path, text: text,
			score: cosine(decodeF32(emb), qvec),
		}
		if title == "" {
			s.title = path
		}
		all = append(all, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// top-k chunks: keep a sorted-descending window of size k*3 and replace
	// its tail whenever a later element beats it (correct for any input order).
	max := k * 3
	if max > len(all) {
		max = len(all)
	}
	window := all[:max]
	for i := 1; i < len(window); i++ {
		for j := i; j > 0 && window[j].score > window[j-1].score; j-- {
			window[j], window[j-1] = window[j-1], window[j]
		}
	}
	for i := max; i < len(all); i++ {
		if all[i].score > window[len(window)-1].score {
			window[len(window)-1] = all[i]
			for j := len(window) - 1; j > 0 &&
				window[j].score > window[j-1].score; j-- {
				window[j], window[j-1] = window[j-1], window[j]
			}
		}
	}

	var results []SemanticHit
	index := map[int64]int{}
	n := 0
	for _, s := range all {
		if n >= k {
			break
		}
		snip := s.text
		if len(snip) > 200 {
			snip = snip[:200]
		}
		pos, ok := index[s.docID]
		if !ok {
			pos = len(results)
			index[s.docID] = pos
			results = append(results,
				SemanticHit{DocID: s.docID, Title: s.title, Path: s.path})
			results[pos].Snippets = append(results[pos].Snippets,
				SemSnippet{Page: s.page, Text: snip, Score: s.score})
			n++
			continue
		}
		if len(results[pos].Snippets) < 3 {
			results[pos].Snippets = append(results[pos].Snippets,
				SemSnippet{Page: s.page, Text: snip, Score: s.score})
		}
	}
	return results, nil
}

// EmbedPending embeds all chunks lacking embeddings. If the embedding
// server URL changed since last time, stale vectors are cleared.
func EmbedPending(cfg *config.Config, conn *sql.DB) (int, error) {
	model := cfg.Tools.EmbedURL
	if stored := db.MetaGet(conn, "embed_model"); stored != "" && stored != model {
		if _, err := conn.Exec("UPDATE chunks SET embedding=NULL"); err != nil {
			return 0, err
		}
	}
	if err := db.MetaSet(conn, "embed_model", model); err != nil {
		return 0, err
	}

	rows, err := conn.Query("SELECT id, text FROM chunks WHERE embedding IS NULL ORDER BY id")
	if err != nil {
		return 0, err
	}
	type chunkRow struct {
		id   int64
		text string
	}
	var pending []chunkRow
	for rows.Next() {
		var r chunkRow
		if err := rows.Scan(&r.id, &r.text); err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}

	done := 0
	batch := cfg.Embed.Batch
	for i := 0; i < len(pending); i += batch {
		end := i + batch
		if end > len(pending) {
			end = len(pending)
		}
		texts := make([]string, 0, end-i)
		for _, r := range pending[i:end] {
			if strings.TrimSpace(r.text) == "" {
				texts = append(texts, " ")
			} else {
				texts = append(texts, r.text)
			}
		}
		vecs, err := llm.Embed(cfg.Tools.EmbedURL, texts)
		if err != nil {
			return done, err
		}
		tx, err := conn.Begin()
		if err != nil {
			return done, err
		}
		for j, r := range pending[i:end] {
			if _, err := tx.Exec("UPDATE chunks SET embedding=? WHERE id=?",
				encodeF32(vecs[j]), r.id); err != nil {
				tx.Rollback()
				return done, err
			}
		}
		if err := tx.Commit(); err != nil {
			return done, err
		}
		done += end - i
	}
	return done, nil
}

// decodeF32 reads the little-endian float32 blob format (same as the Python
// version's struct.pack, so databases are interchangeable).
func decodeF32(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

func encodeF32(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

func cosine(a []float32, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
