"""Summarization (map-reduce) and constrained tagging."""

from __future__ import annotations

import json
import logging

from .config import Config
from .db import document_text
from .llm import chat_json
from .vocab import Vocabulary

log = logging.getLogger("vellum.summarize")

MAP_PROMPT = """\
You are summarizing one excerpt of a longer document for a personal library index.
Write a concise factual summary (max 150 words) of the key points, arguments,
names, and topics of the excerpt. Do not speculate about missing context.
Plain text only, no headings or lists.

Excerpt:
{text}"""

DIRECT_PROMPT = """\
You are writing the index entry for a document in a personal library.
Write ONE flowing paragraph (150-300 words) summarizing the whole document
below: its topic, its main arguments or findings, and what kind of text it
is. Do NOT copy passages from the document — describe it in your own words.
Plain text only, no headings, no lists, no markdown.

Document:
{text}"""

REDUCE_PROMPT = """\
You are writing the index entry for a document in a personal library.
Below are summaries of consecutive excerpts of the document, in order.
Write ONE flowing paragraph (150-300 words) summarizing the whole document:
its topic, its main arguments or findings, and what kind of text it is.
Plain text only, no headings, no lists, no markdown.

Excerpt summaries:
{summaries}"""

TAG_PROMPT = """\
You are cataloguing a document in a personal library.

Allowed tags (you MUST choose only from this list; a tag applies only if the
document substantively addresses the topic, not if it merely mentions it):
{descriptions}

Rules:
- Choose the most relevant tags, usually 2-6, at most {max_tags}.
- If the document clearly and substantively belongs to a topic that is missing
  from the allowed list, put ONE short lowercase-hyphenated English label for
  it in tags_other (e.g. "marine-biology"). Otherwise leave tags_other empty.
- If you can confidently infer the document's title, authors, or publication
  year from the text, fill them in; otherwise leave them empty ("" / []).

Document text (possibly truncated):
{text}"""

REDUCE_SCHEMA = {
    "type": "object",
    "properties": {"summary": {"type": "string"}},
    "required": ["summary"],
}

TAG_SCHEMA = {
    "type": "object",
    "properties": {
        "title": {"type": "string"},
        "authors": {"type": "array", "items": {"type": "string"}},
        "year": {"type": "string"},
        "tags": {"type": "array", "items": {"type": "string"}},
        "tags_other": {"type": "array", "items": {"type": "string"}},
    },
    "required": ["title", "authors", "year", "tags", "tags_other"],
}


def _chunk_text(text: str, chunk_chars: int) -> list[str]:
    # Split on paragraph boundaries close to the target size.
    paras = text.split("\n\n")
    chunks, cur, size = [], [], 0
    for p in paras:
        if size + len(p) > chunk_chars and cur:
            chunks.append("\n\n".join(cur))
            cur, size = [], 0
        cur.append(p)
        size += len(p) + 2
    if cur:
        chunks.append("\n\n".join(cur))
    return [c for c in chunks if c.strip()]


def summarize(cfg: Config, text: str) -> str:
    """Map-reduce summary. Short documents get a single direct pass."""
    chunk_chars = cfg.get("summarize", "chunk_chars", default=6000)
    chunks = _chunk_text(text, chunk_chars)

    if len(chunks) <= 1:
        prompt = DIRECT_PROMPT.format(text=text[:chunk_chars * 3])
    else:
        log.info("map phase: %d chunks", len(chunks))
        summaries = []
        for c in chunks:
            out = chat_json(cfg, [{"role": "user",
                                   "content": MAP_PROMPT.format(text=c)}],
                            REDUCE_SCHEMA)
            summaries.append(out["summary"].strip())
        prompt = REDUCE_PROMPT.format(summaries="\n---\n".join(summaries))

    log.info("reduce phase")
    out = chat_json(cfg, [{"role": "user", "content": prompt}], REDUCE_SCHEMA)
    return out["summary"].strip()


def tag_document(cfg: Config, vocab: Vocabulary, text: str) -> dict:
    """Constrained tagging. Returns the validated LLM response."""
    chunk_chars = cfg.get("summarize", "chunk_chars", default=6000)
    max_tags = cfg.get("summarize", "max_tags", default=8)
    context = text[:chunk_chars * 3]  # leading ~18k chars is plenty for topic ID

    schema = json.loads(json.dumps(TAG_SCHEMA))  # deep copy before injecting enum
    schema["properties"]["tags"]["items"]["enum"] = sorted(vocab.tags.keys())

    prompt = TAG_PROMPT.format(
        descriptions=vocab.descriptions_block(),
        max_tags=max_tags,
        text=context,
    )
    out = chat_json(cfg, [{"role": "user", "content": prompt}], schema)

    vocab_set = set(vocab.tags.keys())
    out["tags"] = [t for t in out.get("tags", []) if t in vocab_set][:max_tags]
    out["tags_other"] = [t.strip().lower() for t in out.get("tags_other", [])
                          if t.strip()][:3]
    return out