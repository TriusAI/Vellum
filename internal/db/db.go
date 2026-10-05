// Package db owns the SQLite schema and storage helpers.
//
// The schema is byte-for-byte the one used by the retired Python version, so
// an existing library.db keeps working. Storage is external-content FTS5 kept
// in sync by triggers (contentless tables can't serve snippet()).
package db

import (
	"database/sql"
	"os"
	"path/filepath"

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
  category_user INTEGER DEFAULT 0
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

// DocumentText concatenates a document's chunks in reading order.
func DocumentText(conn *sql.DB, docID int64) (string, error) {
	rows, err := conn.Query("SELECT text FROM chunks WHERE doc_id=? ORDER BY seq", docID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	out := ""
	first := true
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return "", err
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
