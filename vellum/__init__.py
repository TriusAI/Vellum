"""Vellum — a small local-model library manager.

Pipeline: ingest (text extraction / OCR) -> summarize -> tag (constrained) -> embed -> search.
Storage is a single SQLite file with FTS5 full-text search.
"""

__version__ = "0.1.0"