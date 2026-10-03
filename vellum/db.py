"""SQLite storage: documents, chunks (with embeddings), tags, and FTS5 index."""

from __future__ import annotations

import json
import sqlite3
from pathlib import Path

SCHEMA = """
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS meta(
  key TEXT PRIMARY KEY,
  value TEXT
);

CREATE TABLE IF NOT EXISTS documents(
  id INTEGER PRIMARY KEY,
  path TEXT UNIQUE NOT NULL,
  sha256 TEXT,
  title TEXT,
  authors TEXT,          -- comma-separated
  year TEXT,
  summary TEXT,
  status TEXT NOT NULL DEFAULT 'ingested',  -- ingested | done | error
  error TEXT,
  ocr_pages INTEGER DEFAULT 0,             -- how many pages needed OCR
  n_pages INTEGER,
  added_at TEXT DEFAULT (datetime('now')),
  processed_at TEXT
);

CREATE TABLE IF NOT EXISTS chunks(
  id INTEGER PRIMARY KEY,
  doc_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  page_no INTEGER,                          -- 1-based; NULL for unpaginated sources
  text TEXT NOT NULL,
  embedding BLOB,                           -- float32 little-endian
  UNIQUE(doc_id, seq)
);

CREATE TABLE IF NOT EXISTS doc_tags(
  doc_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  tag TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT 'vocab',     -- vocab | suggested
  PRIMARY KEY(doc_id, tag)
);

-- External-content FTS5: the text lives only in chunks; triggers keep the
-- index in sync. (Contentless tables can't serve snippet().)
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
"""


def connect(db_path: Path) -> sqlite3.Connection:
    db_path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    conn.executescript(SCHEMA)
    return conn


def get_meta(conn: sqlite3.Connection, key: str):
    row = conn.execute("SELECT value FROM meta WHERE key=?", (key,)).fetchone()
    return row["value"] if row else None


def set_meta(conn: sqlite3.Connection, key: str, value: str):
    conn.execute(
        "INSERT INTO meta(key, value) VALUES(?,?) "
        "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
        (key, value),
    )
    conn.commit()


def replace_document_text(conn: sqlite3.Connection, doc_id: int, chunks: list[dict]):
    """Replace all chunks of a document. `chunks`: [{seq, page_no, text}, ...].

    The FTS index follows automatically via triggers on `chunks`.
    """
    try:
        conn.execute("DELETE FROM chunks WHERE doc_id=?", (doc_id,))
        for c in chunks:
            conn.execute(
                "INSERT INTO chunks(doc_id, seq, page_no, text) VALUES(?,?,?,?)",
                (doc_id, c["seq"], c.get("page_no"), c["text"]),
            )
        conn.execute(
            "UPDATE documents SET n_pages=? WHERE id=?",
            (len(chunks), doc_id),
        )
        conn.commit()
    except Exception:
        conn.rollback()
        raise


def set_tags(conn: sqlite3.Connection, doc_id: int, tags: list[tuple[str, str]]):
    """Replace tags for a doc. `tags`: [(tag, source), ...]"""
    conn.execute("DELETE FROM doc_tags WHERE doc_id=?", (doc_id,))
    conn.executemany(
        "INSERT OR IGNORE INTO doc_tags(doc_id, tag, source) VALUES(?,?,?)",
        [(doc_id, t, s) for t, s in tags],
    )
    conn.commit()


def document_text(conn: sqlite3.Connection, doc_id: int) -> str:
    rows = conn.execute(
        "SELECT text FROM chunks WHERE doc_id=? ORDER BY seq", (doc_id,)
    ).fetchall()
    return "\n\n".join(r["text"] for r in rows)


def docs_json(conn: sqlite3.Connection, rows) -> list[dict]:
    return [
        {
            "id": r["id"], "path": r["path"], "title": r["title"],
            "authors": r["authors"], "year": r["year"], "summary": r["summary"],
            "status": r["status"], "ocr_pages": r["ocr_pages"],
            "n_pages": r["n_pages"],
            "tags": [
                t["tag"] for t in conn.execute(
                    "SELECT tag FROM doc_tags WHERE doc_id=? ORDER BY tag", (r["id"],)
                )
            ],
        }
        for r in rows
    ]