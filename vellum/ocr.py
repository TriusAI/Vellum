"""Text extraction and OCR.

Strategy for each page of a PDF/EPUB:
 1. Try embedded text (PyMuPDF get_text).
 2. If a page has fewer than `min_chars_per_page` extractable characters,
    treat it as scanned: render at `dpi` and run Tesseract.
For plain text files, no PDF machinery is involved.
"""

from __future__ import annotations

import logging
import os
import re
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import pymupdf as fitz  # PyMuPDF (the legacy `fitz` import is deprecated)
import pytesseract
from PIL import Image

from .config import Config

log = logging.getLogger("vellum.ocr")

SUPPORTED_EXT = {".pdf", ".epub", ".mobi", ".azw", ".azw3", ".fb2",
                 ".txt", ".md", ".markdown", ".rst"}

_WHITESPACE = re.compile(r"[ \t]+\n|\n{3,}")


def _clean(text: str) -> str:
    return _WHITESPACE.sub("\n\n", text).strip()


def _needs_ocr(text: str, min_chars: int) -> bool:
    return len(text.strip()) < min_chars


def _tessdata_prefix(cfg: Config):
    """Point Tesseract at a project-local tessdata/ dir if present (no root
    needed to add languages: drop .traineddata files there)."""
    for cand in (cfg.base_dir / "tessdata",
                 Path(__file__).resolve().parent.parent / "tessdata"):
        if cand.is_dir() and any(cand.glob("*.traineddata")):
            os.environ["TESSDATA_PREFIX"] = str(cand)
            return
    # fall back to tesseract's default search path


def _check_langs(langs: str):
    wanted = [l for l in langs.split("+") if l and l != "osd"]
    try:
        available = set(pytesseract.get_languages(config=""))
    except Exception as e:
        raise RuntimeError(f"cannot query tesseract: {e}") from e
    missing = [l for l in wanted if l not in available]
    if missing:
        raise RuntimeError(
            f"tesseract traineddata missing for {missing} — download the "
            f".traineddata file(s) into the tessdata/ directory (see README)")


def _ocr_page(page: fitz.Page, dpi: int, langs: str) -> str:
    pix = page.get_pixmap(dpi=dpi)
    img = Image.frombytes("RGB", (pix.width, pix.height), pix.samples)
    return pytesseract.image_to_string(img, lang=langs)


def extract_document(path: Path, cfg: Config) -> dict:
    """Return {chunks, meta, ocr_pages}.

    chunks: [{seq, page_no, text}, ...] in reading order.
    """
    ext = path.suffix.lower()
    if ext in (".txt", ".md", ".markdown", ".rst"):
        raw = path.read_text(encoding="utf-8", errors="replace")
        chunk_chars = cfg.get("summarize", "chunk_chars", default=6000)
        chunks = [
            {"seq": i, "page_no": None, "text": _clean(raw[i:i + chunk_chars])}
            for i in range(0, len(raw), chunk_chars)
        ]
        return {"chunks": chunks, "meta": {}, "ocr_pages": 0}

    doc = fitz.open(path)
    min_chars = cfg.get("ocr", "min_chars_per_page", default=50)
    dpi = cfg.get("ocr", "dpi", default=300)
    langs = cfg.get("ocr", "langs", default="eng")
    workers = cfg.get("ocr", "workers", default=3)

    chunks: list[dict] = []
    ocr_jobs: list[tuple[int, fitz.Page]] = []

    for pno, page in enumerate(doc):
        text = _clean(page.get_text())
        if _needs_ocr(text, min_chars) and page.get_images():
            # little/no extractable text but images present -> likely a scan
            ocr_jobs.append((pno, page))
            chunks.append({"seq": pno, "page_no": pno + 1, "text": ""})
        else:
            chunks.append({"seq": pno, "page_no": pno + 1, "text": text})

    if ocr_jobs:
        _tessdata_prefix(cfg)
        _check_langs(langs)
        log.info("%s: OCR on %d/%d pages (%s)",
                 path.name, len(ocr_jobs), len(chunks), langs)
        with ThreadPoolExecutor(max_workers=workers) as pool:
            futures = {
                pno: pool.submit(_ocr_page, page, dpi, langs)
                for pno, page in ocr_jobs
            }
            for pno, fut in futures.items():
                try:
                    chunks[pno]["text"] = _clean(fut.result())
                except Exception as e:  # OCR failure on one page is not fatal
                    log.warning("OCR failed on page %d of %s: %s",
                                pno + 1, path.name, e)

    meta = {}
    if doc.metadata:
        meta = {
            "title": doc.metadata.get("title", ""),
            "authors": doc.metadata.get("author", ""),
            "year": "",
        }

    n = doc.page_count
    doc.close()
    return {"chunks": chunks, "meta": meta, "ocr_pages": len(ocr_jobs)}