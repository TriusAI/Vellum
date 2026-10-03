"""Vellum command-line interface."""

from __future__ import annotations

import argparse
import logging
import sys

from . import __version__
from .config import load_config
from .vocab import Vocabulary


def _setup_logging(verbose: bool):
    logging.basicConfig(
        level=logging.DEBUG if verbose else logging.INFO,
        format="%(message)s",
        stream=sys.stderr,
    )
    logging.getLogger("httpx").setLevel(logging.WARNING)


# ---------------------------------------------------------------- commands

def cmd_ingest(args):
    from .ingest import ingest
    cfg = load_config(args.config)
    stats = ingest(cfg, args.paths, reprocess=args.reprocess, verbose=True)
    print(f"added={stats['added']} updated={stats['updated']} "
          f"skipped={stats['skipped']} failed={stats['failed']}")
    print("next: vellum process   (summarize + tag with the LLM)")


def cmd_process(args):
    from .ingest import process_pending
    cfg = load_config(args.config)
    vocab = Vocabulary.load(cfg.vocab_path)
    if not vocab.tags:
        print("vocab.yaml is empty — add tags first with `vellum vocab add`. "
              "Tagging needs a controlled vocabulary to constrain the LLM.",
              file=sys.stderr)
        sys.exit(1)
    n = process_pending(cfg, vocab, limit=args.limit)
    print(f"processed {n} document(s)")


def cmd_search(args):
    import sqlite3
    from .db import connect
    from .embed import semantic_search
    cfg = load_config(args.config)
    conn = connect(cfg.db_path)

    if args.semantic:
        results = semantic_search(cfg, conn, args.query, k=args.limit)
        for r in results:
            top = r["snippets"][0]
            print(f"[{top[2]:.3f}] {r['title']}  ({r['path']})")
            for page, snippet, score in r["snippets"][:2]:
                loc = f"p.{page}" if page else "chunk"
                print(f"    {loc}: {snippet[:160]}...")
            print()
    else:
        # FTS5 keyword search with snippet highlighting.
        q = args.query
        try:
            rows = conn.execute(
                "SELECT d.title, d.path, c.page_no, "
                "       snippet(fts, 0, '[', ']', '…', 12) AS snip, "
                "       bm25(fts) AS rank "
                "FROM fts JOIN chunks c ON c.id = fts.rowid "
                "JOIN documents d ON d.id = c.doc_id "
                "WHERE fts MATCH ? ORDER BY rank LIMIT ?",
                (q, args.limit),
            ).fetchall()
        except sqlite3.OperationalError:
            # raw query wasn't valid FTS5 syntax — treat it as one literal phrase
            rows = conn.execute(
                "SELECT d.title, d.path, c.page_no, "
                "       snippet(fts, 0, '[', ']', '…', 12) AS snip, "
                "       bm25(fts) AS rank "
                "FROM fts JOIN chunks c ON c.id = fts.rowid "
                "JOIN documents d ON d.id = c.doc_id "
                "WHERE fts MATCH ? ORDER BY rank LIMIT ?",
                ('"' + q.replace('"', '""') + '"', args.limit),
            ).fetchall()
        if not rows:
            print("no matches")
            return
        current = None
        for r in rows:
            if (r["path"], ) != current:
                current = (r["path"], )
                print(f"\n{r['title'] or r['path']}  ({r['path']})")
            loc = f"p.{r['page_no']}" if r["page_no"] else "chunk"
            print(f"  {loc}: {r['snip']}")
    conn.close()


def cmd_show(args):
    from .db import connect, docs_json
    cfg = load_config(args.config)
    conn = connect(cfg.db_path)
    if args.doc_id == "all":
        rows = conn.execute(
            "SELECT * FROM documents ORDER BY id").fetchall()
        for d in docs_json(conn, rows):
            head = f"#{d['id']} {d['title'] or d['path']}"
            if d["status"] != "done":
                head += f"  [{d['status']}]"
            print(head)
            if d["authors"] or d["year"]:
                print(f"    {d['authors']} {d['year']}".rstrip())
            if d["tags"]:
                print(f"    tags: {', '.join(d['tags'])}")
            if d["summary"]:
                import textwrap
                print(textwrap.fill(d["summary"], 80,
                                    initial_indent="    ", subsequent_indent="    "))
            print()
    else:
        row = conn.execute("SELECT * FROM documents WHERE id=?",
                           (int(args.doc_id),)).fetchone()
        if not row:
            print(f"no document #{args.doc_id}")
            sys.exit(1)
        for d in docs_json(conn, [row]):
            for k, v in d.items():
                if v not in (None, "", []):
                    print(f"{k}: {v}")
    conn.close()


def cmd_vocab(args):
    import sqlite3
    from .db import connect
    cfg = load_config(args.config)
    vocab = Vocabulary.load(cfg.vocab_path)
    conn = connect(cfg.db_path)

    if args.action == "list":
        for name, desc in sorted(vocab.tags.items()):
            print(f"{name}: {desc}")
    elif args.action == "add":
        ok = vocab.add(args.name, args.description)
        vocab.save()
        print("added" if ok else f"'{args.name}' already in vocabulary")
    elif args.action == "remove":
        ok = vocab.remove(args.name)
        vocab.save()
        if ok:
            conn.execute("DELETE FROM doc_tags WHERE tag=?", (args.name.lower(),))
            conn.commit()
            print("removed")
        else:
            print(f"'{args.name}' not in vocabulary")
    elif args.action == "review":
        rows = conn.execute(
            "SELECT tag, COUNT(*) AS n, MIN(d.title) AS example "
            "FROM doc_tags t JOIN documents d ON d.id = t.doc_id "
            "WHERE t.source='suggested' "
            "GROUP BY tag ORDER BY n DESC, tag").fetchall()
        if not rows:
            print("no suggested tags to review")
            return
        print("tags suggested by the LLM (outside the vocabulary):")
        for r in rows:
            print(f"  {r['tag']}  ×{r['n']}  (e.g. {r['example']})")
        print("\npromote the keepers:  vellum vocab promote <tag> \"description\"")
    elif args.action == "promote":
        if not args.description:
            print("a description is required, e.g. "
                  "vellum vocab promote marine-biology \"study of ocean life\"",
                  file=sys.stderr)
            sys.exit(1)
        name = args.name.strip().lower()
        vocab.add(name, args.description)
        vocab.save()
        moved = conn.execute(
            "UPDATE doc_tags SET source='vocab' WHERE tag=?", (name,)).rowcount
        conn.commit()
        print(f"promoted '{name}'; {moved} existing document tag(s) now controlled")
    conn.close()


def cmd_embed(args):
    from .db import connect
    from .embed import embed_pending
    cfg = load_config(args.config)
    conn = connect(cfg.db_path)
    n = embed_pending(cfg, conn)
    print(f"embedded {n} chunk(s)")
    conn.close()


# ---------------------------------------------------------------- parser

def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="vellum",
        description="Small local-model library manager: OCR, summaries, "
                     "controlled-vocabulary tagging, full-text + semantic search.")
    p.add_argument("--config", help="path to config.yaml "
                                    "(default: $VELLUM_CONFIG or ./config.yaml)")
    p.add_argument("-V", "--version", action="version",
                   version=f"vellum {__version__}")
    p.add_argument("-v", "--verbose", action="store_true",
                   help="debug logging")
    sub = p.add_subparsers(dest="cmd", required=True)

    sp = sub.add_parser("ingest", help="index files/directories "
                                       "(text extraction + OCR fallback)")
    sp.add_argument("paths", nargs="+")
    sp.add_argument("--reprocess", action="store_true",
                    help="re-extract even if the file is unchanged")
    sp.set_defaults(func=cmd_ingest)

    sp = sub.add_parser("process", help="summarize + tag pending documents")
    sp.add_argument("--limit", type=int)
    sp.set_defaults(func=cmd_process)

    sp = sub.add_parser("search", help="full-text (default) or semantic search")
    sp.add_argument("query")
    sp.add_argument("--semantic", action="store_true",
                    help="cosine similarity over chunk embeddings")
    sp.add_argument("--limit", type=int, default=20)
    sp.set_defaults(func=cmd_search)

    sp = sub.add_parser("show", help="list library or show one document")
    sp.add_argument("doc_id", help="'all' or a document id")
    sp.set_defaults(func=cmd_show)

    sp = sub.add_parser("vocab", help="manage the controlled tag vocabulary")
    sp.add_argument("action", choices=["list", "add", "remove", "review", "promote"])
    sp.add_argument("name", nargs="?", help="tag name (add/remove/promote)")
    sp.add_argument("description", nargs="?", help="tag description")
    sp.set_defaults(func=cmd_vocab)

    sp = sub.add_parser("embed", help="embed chunks lacking embeddings")
    sp.set_defaults(func=cmd_embed)

    return p


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)
    _setup_logging(getattr(args, "verbose", False)
                   or getattr(args, "reprocess", False))
    args.func(args)


if __name__ == "__main__":
    main()