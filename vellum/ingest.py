"""Ingestion: walk paths, extract text (with OCR fallback), store chunks."""

from __future__ import annotations

import hashlib
import logging
import sqlite3
import time
from pathlib import Path

from .config import Config
from .db import connect, replace_document_text
from .ocr import SUPPORTED_EXT, extract_document

log = logging.getLogger("vellum.ingest")


def _sha256(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def _iter_files(paths: list[str]):
    for p in paths:
        path = Path(p).expanduser()
        if path.is_file():
            yield path
        elif path.is_dir():
            for f in sorted(path.rglob("*")):
                if f.is_file() and f.suffix.lower() in SUPPORTED_EXT:
                    yield f
        else:
            log.warning("skipping (not found): %s", p)


def ingest(cfg: Config, paths: list[str], reprocess: bool = False,
           verbose: bool = False) -> dict:
    conn = connect(cfg.db_path)
    stats = {"added": 0, "updated": 0, "skipped": 0, "failed": 0}

    for path in _iter_files(paths):
        if path.suffix.lower() not in SUPPORTED_EXT:
            continue
        try:
            digest = _sha256(path)
            row = conn.execute(
                "SELECT id, sha256 FROM documents WHERE path=?",
                (str(path.resolve()),),
            ).fetchone()
            if row and row["sha256"] == digest and not reprocess:
                stats["skipped"] += 1
                continue
            t0 = time.time()
            result = extract_document(path, cfg)
            if not any(c["text"] for c in result["chunks"]):
                log.warning("no text extracted from %s — skipping", path)
                stats["failed"] += 1
                continue

            meta = result["meta"]
            if row:
                doc_id = row["id"]
                conn.execute(
                    "UPDATE documents SET sha256=?, status='ingested', "
                    "error=NULL, processed_at=NULL WHERE id=?",
                    (digest, doc_id),
                )
                stats["updated"] += 1
            else:
                cur = conn.execute(
                    "INSERT INTO documents(path, sha256, title, authors, year, "
                    "ocr_pages, status) VALUES(?,?,?,?,?,?, 'ingested')",
                    (str(path.resolve()), digest, meta.get("title", ""),
                     meta.get("authors", ""), meta.get("year", ""),
                     result["ocr_pages"]),
                )
                doc_id = cur.lastrowid
                stats["added"] += 1

            replace_document_text(conn, doc_id, result["chunks"])
            if verbose:
                log.info("%s: %d chunks (%d OCR pages) in %.1fs",
                         path.name, len(result["chunks"]),
                         result["ocr_pages"], time.time() - t0)
        except Exception as e:
            log.error("failed to ingest %s: %s", path, e)
            stats["failed"] += 1

    conn.commit()
    conn.close()
    return stats


def process_pending(cfg: Config, vocab, limit: int | None = None) -> int:
    """Summarize + tag all documents with status 'ingested'."""
    from .db import document_text, set_tags  # late import: no cycle
    from .summarize import summarize, tag_document

    conn = connect(cfg.db_path)
    query = "SELECT * FROM documents WHERE status='ingested' ORDER BY id"
    if limit:
        query += f" LIMIT {int(limit)}"
    rows = conn.execute(query).fetchall()
    done = 0
    for row in rows:
        doc_id = row["id"]
        try:
            text = document_text(conn, doc_id)
            log.info("processing %s", row["path"])
            summary = summarize(cfg, text)
            tags_out = tag_document(cfg, vocab, text)

            conn.execute(
                "UPDATE documents SET summary=?, title=?, authors=?, year=?, "
                "status='done', processed_at=datetime('now') WHERE id=?",
                (summary,
                 row["title"] or tags_out.get("title", ""),
                 row["authors"] or ", ".join(tags_out.get("authors", [])),
                 row["year"] or tags_out.get("year", ""),
                 doc_id),
            )
            tag_pairs = [(t, "vocab") for t in tags_out["tags"]]
            tag_pairs += [(t, "suggested") for t in tags_out["tags_other"]]
            set_tags(conn, doc_id, tag_pairs)
            conn.commit()
            done += 1
            log.info("done: %s — tags: %s%s", row["path"],
                     ", ".join(tags_out["tags"]) or "-",
                     (" | suggested: " + ", ".join(tags_out["tags_other"]))
                     if tags_out["tags_other"] else "")
        except Exception as e:
            log.error("processing failed for %s: %s", row["path"], e)
            conn.execute(
                "UPDATE documents SET status='error', error=? WHERE id=?",
                (str(e), doc_id),
            )
            conn.commit()
    conn.close()
    return done