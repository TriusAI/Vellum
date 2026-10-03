"""Chunk embeddings and semantic search (brute-force cosine — fine at
personal-library scale on CPU)."""

from __future__ import annotations

import struct
from pathlib import Path

from .config import Config
from .db import connect, get_meta, set_meta
from .llm import embed_texts


def embed_pending(cfg: Config, conn) -> int:
    """Embed all chunks that lack an embedding. Returns count."""
    model = cfg.get("models", "embed", default="nomic-embed-text")
    stored_model = get_meta(conn, "embed_model")
    if stored_model and stored_model != model:
        # Model changed: old vectors are not comparable, clear them.
        conn.execute("UPDATE chunks SET embedding=NULL")
        conn.commit()
    set_meta(conn, "embed_model", model)

    rows = conn.execute(
        "SELECT id, text FROM chunks WHERE embedding IS NULL ORDER BY id"
    ).fetchall()
    if not rows:
        return 0

    texts = [r["text"] or " " for r in rows]
    vecs = embed_texts(cfg, texts)
    conn.executemany(
        "UPDATE chunks SET embedding=? WHERE id=?",
        [(struct.pack(f"<{len(v)}f", *v), r["id"]) for r, v in zip(rows, vecs)],
    )
    conn.commit()
    return len(rows)


def _cosine(a: bytes, b: list[float]) -> float:
    va = struct.unpack(f"<{len(a)//4}f", a)
    num = sum(x * y for x, y in zip(va, b))
    na = sum(x * x for x in va) ** 0.5
    nb = sum(y * y for y in b) ** 0.5
    if na == 0 or nb == 0:
        return 0.0
    return num / (na * nb)


def semantic_search(cfg: Config, conn, query: str, k: int = 20) -> list[dict]:
    """Top-k chunks by cosine similarity, grouped per document."""
    embed_pending(cfg, conn)  # lazy backfill so search is never stale
    (qvec,) = embed_texts(cfg, [query])

    rows = conn.execute(
        "SELECT c.id, c.doc_id, c.page_no, c.embedding, c.text, "
        "       d.title, d.path "
        "FROM chunks c JOIN documents d ON d.id = c.doc_id "
        "WHERE c.embedding IS NOT NULL"
    ).fetchall()

    scored = sorted(
        ((row, _cosine(row["embedding"], qvec)) for row in rows),
        key=lambda t: t[1], reverse=True,
    )[:k]

    results, seen = [], {}
    for row, score in scored:
        if row["doc_id"] in seen:
            seen[row["doc_id"]]["snippets"].append(
                (row["page_no"], row["text"][:200], score))
            continue
        entry = {
            "doc_id": row["doc_id"], "title": row["title"] or Path(row["path"]).name,
            "path": row["path"],
            "snippets": [(row["page_no"], row["text"][:200], score)],
        }
        seen[row["doc_id"]] = entry
        results.append(entry)
    return results