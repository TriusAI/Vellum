// Package db owns the SQLite schema and storage helpers.
//
// The schema is byte-for-byte the one used by the retired Python version, so
// an existing library.db keeps working. Storage is external-content FTS5 kept
// in sync by triggers (contentless tables can't serve snippet()).
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta(
  key TEXT PRIMARY KEY,
  value TEXT
);

CREATE TABLE IF NOT EXISTS documents(
  id INTEGER PRIMARY KEY,
  path TEXT UNIQUE NOT NULL,
  sha256 TEXT,
  title TEXT,
  authors TEXT,
  year TEXT,
  summary TEXT,
  status TEXT NOT NULL DEFAULT 'ingested',
  error TEXT,
  ocr_pages INTEGER DEFAULT 0,
  n_pages INTEGER,
  added_at TEXT DEFAULT (datetime('now')),
  processed_at TEXT,
  kind TEXT DEFAULT '',
  summary_source TEXT DEFAULT '',
  category TEXT DEFAULT '',
  ocr_pending INTEGER DEFAULT 0,
  kind_user INTEGER DEFAULT 0,
  category_user INTEGER DEFAULT 0,
  ocr_done_pages TEXT DEFAULT '',
  skip_pages TEXT DEFAULT ''
);

CREATE TABLE IF NOT EXISTS chunks(
  id INTEGER PRIMARY KEY,
  doc_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  page_no INTEGER,
  text TEXT NOT NULL,
  embedding BLOB,
  UNIQUE(doc_id, seq)
);

CREATE TABLE IF NOT EXISTS doc_tags(
  doc_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  tag TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT 'vocab',
  PRIMARY KEY(doc_id, tag)
);

-- Collections: user-managed groups of documents (research projects).
-- A document can sit in any number of collections, so membership is a
-- join table; both sides cascade (deleting a document or a collection
-- cleans up membership rows).
CREATE TABLE IF NOT EXISTS collections(
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  description TEXT DEFAULT '',
  created_at TEXT DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS collection_docs(
  collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
  doc_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  added_at TEXT DEFAULT (datetime('now')),
  PRIMARY KEY(collection_id, doc_id)
);

-- Notes: freeform scratchpad entries (ideas jotted while reading). Only
-- a body plus timestamps — no other metadata.
CREATE TABLE IF NOT EXISTS notes(
  id INTEGER PRIMARY KEY,
  body TEXT NOT NULL DEFAULT '',
  created_at TEXT DEFAULT (datetime('now')),
  updated_at TEXT DEFAULT (datetime('now'))
);

CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(
  text, content='chunks', content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
  INSERT INTO fts(rowid, text) VALUES(new.id, new.text);
END;
CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
  INSERT INTO fts(fts, rowid, text) VALUES('delete', old.id, old.text);
END;
CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE OF text ON chunks BEGIN
  INSERT INTO fts(fts, rowid, text) VALUES('delete', old.id, old.text);
  INSERT INTO fts(rowid, text) VALUES(new.id, new.text);
END;
`

// Open opens (creating if needed) the library database, applies the schema
// and runs lightweight additive migrations (libraries from older versions
// keep working; nothing is ever dropped).
func Open(dbPath string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + dbPath + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, err
	}
	if err := migrate(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// migrate applies additive column migrations. Old SQLite versions can't
// ALTER TABLE ... IF NOT EXISTS, so check pragma table_info first.
func migrate(conn *sql.DB) error {
	migrations := []struct{ table, column, ddl string }{
		{"documents", "kind", "ALTER TABLE documents ADD COLUMN kind TEXT DEFAULT ''"},
		{"documents", "summary_source", "ALTER TABLE documents ADD COLUMN summary_source TEXT DEFAULT ''"},
		{"documents", "category", "ALTER TABLE documents ADD COLUMN category TEXT DEFAULT ''"},
		{"documents", "ocr_pending", "ALTER TABLE documents ADD COLUMN ocr_pending INTEGER DEFAULT 0"},
		{"documents", "kind_user", "ALTER TABLE documents ADD COLUMN kind_user INTEGER DEFAULT 0"},
		{"documents", "category_user", "ALTER TABLE documents ADD COLUMN category_user INTEGER DEFAULT 0"},
		{"documents", "ocr_done_pages", "ALTER TABLE documents ADD COLUMN ocr_done_pages TEXT DEFAULT ''"},
		{"documents", "skip_pages", "ALTER TABLE documents ADD COLUMN skip_pages TEXT DEFAULT ''"},
	}
	for _, m := range migrations {
		rows, err := conn.Query("PRAGMA table_info(" + m.table + ")")
		if err != nil {
			return err
		}
		found := false
		for rows.Next() {
			var cid int
			var name, ctype string
			var notnull int
			var dflt sql.NullString
			var pk int
			rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk)
			if name == m.column {
				found = true
			}
		}
		rows.Close()
		if !found {
			if _, err := conn.Exec(m.ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

// MetaGet reads a key from the meta table ("" if absent).
func MetaGet(conn *sql.DB, key string) string {
	var v string
	err := conn.QueryRow("SELECT value FROM meta WHERE key=?", key).Scan(&v)
	if err != nil {
		return ""
	}
	return v
}

// MetaSet upserts a meta key.
func MetaSet(conn *sql.DB, key, value string) error {
	_, err := conn.Exec(
		"INSERT INTO meta(key, value) VALUES(?,?) "+
			"ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}

// ReplaceDocumentText swaps all chunks of a document. The FTS index follows
// automatically via triggers. chunks must carry seq, page (0 = unpaginated)
// and text in reading order.
func ReplaceDocumentText(conn *sql.DB, docID int64, chunks []Chunk) error {
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM chunks WHERE doc_id=?", docID); err != nil {
		return err
	}
	for _, c := range chunks {
		var page any
		if c.Page > 0 {
			page = c.Page
		}
		if _, err := tx.Exec(
			"INSERT INTO chunks(doc_id, seq, page_no, text) VALUES(?,?,?,?)",
			docID, c.Seq, page, c.Text); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("UPDATE documents SET n_pages=? WHERE id=?",
		len(chunks), docID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetTags replaces the tag set of a document. pairs: (tag, source).
func SetTags(conn *sql.DB, docID int64, pairs [][2]string) error {
	if _, err := conn.Exec("DELETE FROM doc_tags WHERE doc_id=?", docID); err != nil {
		return err
	}
	for _, p := range pairs {
		if _, err := conn.Exec(
			"INSERT OR IGNORE INTO doc_tags(doc_id, tag, source) VALUES(?,?,?)",
			docID, p[0], p[1]); err != nil {
			return err
		}
	}
	return nil
}

// DocumentText concatenates a document's chunks in reading order,
// EXCLUDING skipped pages (documents.skip_pages — pages the user hid
// must not inform summarization or regeneration either).
func DocumentText(conn *sql.DB, docID int64) (string, error) {
	var skipPages string
	if err := conn.QueryRow("SELECT skip_pages FROM documents WHERE id=?", docID).
		Scan(&skipPages); err != nil && err != sql.ErrNoRows {
		return "", err
	}
	skip := map[int]struct{}{}
	if skipPages != "" && skipPages != "all" {
		for _, part := range strings.Split(skipPages, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
				skip[n] = struct{}{}
			}
		}
	}
	rows, err := conn.Query(
		"SELECT text, page_no FROM chunks WHERE doc_id=? ORDER BY seq", docID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	out := ""
	first := true
	for rows.Next() {
		var t string
		var page any
		if err := rows.Scan(&t, &page); err != nil {
			return "", err
		}
		if page != nil {
			if p, ok := page.(int64); ok {
				if _, hidden := skip[int(p)]; hidden {
					continue
				}
			}
		}
		if !first {
			out += "\n\n"
		}
		out += t
		first = false
	}
	return out, rows.Err()
}

// Chunk is one page/slice of extracted text.
type Chunk struct {
	Seq  int
	Page int // 1-based; 0 = unpaginated source
	Text string
}

// ValidateLibrary checks that path is a readable SQLite database with a
// vellum documents table — WITHOUT modifying it (query-only open) — and
// returns its document count. Used before an import replaces the live
// library, so a random/garbage upload never gets swapped in.
func ValidateLibrary(path string) (int, error) {
	conn, err := sql.Open("sqlite", "file:"+path+"?_pragma=query_only(1)")
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var n int
	if err := conn.QueryRow("SELECT COUNT(*) FROM documents").Scan(&n); err != nil {
		return 0, fmt.Errorf("no documents table: %w", err)
	}
	return n, nil
}

// ExistingCategories lists distinct non-empty categories in the library
// (the model reuses these when auto-categorizing, so shelves consolidate
// instead of multiplying).
func ExistingCategories(conn *sql.DB) ([]string, error) {
	rows, err := conn.Query(
		"SELECT DISTINCT category FROM documents WHERE category != '' ORDER BY category")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------------
// Collections: user-managed groups of documents (research projects).

// Collection is a named group of documents.
type Collection struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Documents   int    `json:"documents"`
}

// ListCollections lists all collections with their document counts.
func ListCollections(conn *sql.DB) ([]Collection, error) {
	rows, err := conn.Query(`
SELECT c.id, c.name, c.description, COUNT(cd.doc_id)
FROM collections c LEFT JOIN collection_docs cd ON cd.collection_id = c.id
GROUP BY c.id ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Collection{}
	for rows.Next() {
		var c Collection
		if err := rows.Scan(&c.ID, &c.Name, &c.Description, &c.Documents); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateCollection creates a collection and returns its id.
func CreateCollection(conn *sql.DB, name, description string) (Collection, error) {
	res, err := conn.Exec(
		"INSERT INTO collections(name, description) VALUES(?,?)", name, description)
	if err != nil {
		return Collection{}, err
	}
	id, _ := res.LastInsertId()
	return Collection{ID: id, Name: name, Description: description}, nil
}

// UpdateCollection renames/re-describes a collection.
func UpdateCollection(conn *sql.DB, id int64, name, description string) error {
	_, err := conn.Exec(
		"UPDATE collections SET name=?, description=? WHERE id=?", name, description, id)
	return err
}

// DeleteCollection drops a collection (membership rows cascade).
func DeleteCollection(conn *sql.DB, id int64) error {
	_, err := conn.Exec("DELETE FROM collections WHERE id=?", id)
	return err
}

// CollectionDocIDs returns the document ids in a collection.
func CollectionDocIDs(conn *sql.DB, id int64) ([]int64, error) {
	rows, err := conn.Query(
		"SELECT doc_id FROM collection_docs WHERE collection_id=? ORDER BY doc_id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var docID int64
		if err := rows.Scan(&docID); err != nil {
			return nil, err
		}
		out = append(out, docID)
	}
	return out, rows.Err()
}

// AddDocsToCollection adds documents to a collection (idempotent); returns
// how many were newly added.
func AddDocsToCollection(conn *sql.DB, id int64, docIDs []int64) (int, error) {
	added := 0
	for _, docID := range docIDs {
		res, err := conn.Exec(
			"INSERT OR IGNORE INTO collection_docs(collection_id, doc_id) VALUES(?,?)",
			id, docID)
		if err != nil {
			return added, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, nil
}

// RemoveDocFromCollection removes one document from a collection.
func RemoveDocFromCollection(conn *sql.DB, id, docID int64) error {
	_, err := conn.Exec(
		"DELETE FROM collection_docs WHERE collection_id=? AND doc_id=?", id, docID)
	return err
}

// CollectionsOfDoc lists the collections a document belongs to.
func CollectionsOfDoc(conn *sql.DB, docID int64) ([]Collection, error) {
	rows, err := conn.Query(`
SELECT c.id, c.name FROM collections c
JOIN collection_docs cd ON cd.collection_id = c.id
WHERE cd.doc_id = ? ORDER BY c.name`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Collection{}
	for rows.Next() {
		var c Collection
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------------
// Notes: freeform scratchpad entries (body + timestamps only).

// Note is one scratchpad entry.
type Note struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ListNotes lists notes, most recently updated first.
func ListNotes(conn *sql.DB) ([]Note, error) {
	rows, err := conn.Query(
		"SELECT id, body, created_at, updated_at FROM notes ORDER BY updated_at DESC, id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Note{}
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Body, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CreateNote inserts a note and returns it.
func CreateNote(conn *sql.DB, body string) (Note, error) {
	res, err := conn.Exec("INSERT INTO notes(body) VALUES(?)", body)
	if err != nil {
		return Note{}, err
	}
	id, _ := res.LastInsertId()
	return GetNote(conn, id)
}

// GetNote reads one note.
func GetNote(conn *sql.DB, id int64) (Note, error) {
	var n Note
	err := conn.QueryRow(
		"SELECT id, body, created_at, updated_at FROM notes WHERE id=?", id).
		Scan(&n.ID, &n.Body, &n.CreatedAt, &n.UpdatedAt)
	return n, err
}

// UpdateNote replaces a note's body and bumps updated_at.
func UpdateNote(conn *sql.DB, id int64, body string) (Note, error) {
	res, err := conn.Exec(
		"UPDATE notes SET body=?, updated_at=datetime('now') WHERE id=?", body, id)
	if err != nil {
		return Note{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Note{}, sql.ErrNoRows
	}
	return GetNote(conn, id)
}

// DeleteNote removes a note.
func DeleteNote(conn *sql.DB, id int64) error {
	_, err := conn.Exec("DELETE FROM notes WHERE id=?", id)
	return err
}
