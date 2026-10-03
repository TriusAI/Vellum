"""Ollama client helpers: chat with structured output, and embeddings.

Structured output is the load-bearing feature here: the `format` parameter
sends a JSON schema to Ollama, which enforces it via grammar-constrained
decoding. An `enum` in the schema makes it *impossible* for the model to
emit a tag outside the vocabulary — that's how Vellum avoids tag drift.
"""

from __future__ import annotations

import json
import logging

from ollama import chat, embed

from .config import Config

log = logging.getLogger("vellum.llm")


def chat_json(cfg: Config, messages: list[dict], schema: dict) -> dict:
    """One chat call with a JSON-schema-constrained response."""
    resp = chat(
        model=cfg.get("models", "llm", default="qwen3:4b"),
        messages=messages,
        format=schema,
        think=cfg.get("llm", "think", default=False),
        options={
            "num_ctx": cfg.get("llm", "num_ctx", default=8192),
            "temperature": cfg.get("llm", "temperature", default=0.3),
        },
    )
    content = resp["message"]["content"]
    try:
        return json.loads(content)
    except json.JSONDecodeError:
        # With `format` this shouldn't happen, but never trust an LLM fully.
        start, end = content.find("{"), content.rfind("}")
        if start >= 0 and end > start:
            return json.loads(content[start:end + 1])
        raise


def embed_texts(cfg: Config, texts: list[str]) -> list[list[float]]:
    model = cfg.get("models", "embed", default="nomic-embed-text")
    batch = cfg.get("embed", "batch", default=32)
    out: list[list[float]] = []
    for i in range(0, len(texts), batch):
        resp = embed(model=model, input=texts[i:i + batch])
        out.extend(resp["embeddings"])
    return out