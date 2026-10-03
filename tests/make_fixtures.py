"""Generate sample documents for testing Vellum:

  1. born-digital PDF (has a real text layer)
  2. "scanned" PDF (text rendered into an image, no text layer)
  3. plain markdown file

Run:  python tests/make_fixtures.py <output_dir>
"""

from __future__ import annotations

import sys
from pathlib import Path

import pymupdf as fitz
from PIL import Image, ImageDraw, ImageFont

LOREM = """On the Origin of Cognitive Machines

This paper argues that contemporary accounts of machine intelligence
systematically underrate the role of embodiment. We review three
schools of thought: symbolic AI, connectionism, and the enactivist
program. Against the tabula rasa assumptions of large-scale
pretraining, we defend the position that learning is always
structured by an agent's sensorimotor loops.

Section 2 surveys the history of the debate, from cybernetics to
deep learning. Section 3 presents our main argument. Section 4
considers objections, in particular the success of language models
that appear to learn without a body. We conclude that the burden of
proof lies with accounts of intelligence that abstract away from
perception and action.
"""


def make_digital_pdf(path: Path):
    doc = fitz.open()
    for pno in range(2):
        page = doc.new_page()
        y = 72
        lines = (LOREM * 3).split("\n")
        for line in lines[pno * 24:(pno + 1) * 24]:
            if line.strip():
                page.insert_text((72, y), line, fontsize=11,
                                 fontname="helv")
            y += 16
    doc.save(path)
    doc.close()


def make_scanned_pdf(path: Path):
    img = Image.new("RGB", (1654, 2339), "white")  # A4 @ 200dpi
    draw = ImageDraw.Draw(img)
    try:
        font = ImageFont.truetype(
            "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", 28)
    except OSError:
        font = ImageFont.load_default()
    y = 100
    for line in LOREM.split("\n"):
        if line.strip():
            draw.text((120, y), line, fill="black", font=font)
        y += 44
    tmp = path.with_suffix(".png")
    img.save(tmp)
    doc = fitz.open()
    doc.new_page(width=595, height=842).insert_image(
        fitz.Rect(0, 0, 595, 842), filename=str(tmp))
    doc.save(path)
    doc.close()
    tmp.unlink()


def make_md(path: Path):
    path.write_text(
        "# Field Notes: Epistemic Humility in Engineering\n\n"
        "A short essay on knowing what you don't know when building systems.\n\n"
        "Cryptography is the discipline of assuming adversaries. Good key "
        "management matters more than exotic ciphers. Log analysis is a form "
        "of applied epistemology: what does the evidence actually support?\n",
        encoding="utf-8")


def main(out_dir: Path):
    out_dir.mkdir(parents=True, exist_ok=True)
    make_digital_pdf(out_dir / "cognitive_machines_digital.pdf")
    make_scanned_pdf(out_dir / "cognitive_machines_scan.pdf")
    make_md(out_dir / "engineering_notes.md")
    print(f"fixtures written to {out_dir}")


if __name__ == "__main__":
    main(Path(sys.argv[1] if len(sys.argv) > 1 else "fixtures"))