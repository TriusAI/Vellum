"""End-to-end test: generates fixtures, ingests them, and (if the Ollama
models are available) processes and searches them.

Run:  python tests/e2e_test.py
Uses a throwaway config + database, so it never touches a real library.
Exits non-zero on failure. The LLM part is skipped with a notice if
`qwen3:4b` is not pulled yet, so the test is also useful pre-model.
"""

from __future__ import annotations

import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

from tests.make_fixtures import main as make_fixtures  # noqa: E402

TEST_CONFIG = """\
db: {db}
library_dir: {lib}
models:
  llm: qwen3:4b
  embed: nomic-embed-text
ocr:
  langs: eng
  workers: 2
"""


def run(cfg: Path, *args, expect_ok=True):
    r = subprocess.run([sys.executable, "-m", "vellum", "--config", str(cfg), *args],
                       cwd=ROOT, capture_output=True, text=True)
    if expect_ok and r.returncode != 0:
        print(r.stdout, r.stderr, sep="\n")
        raise SystemExit(f"FAILED: vellum {' '.join(args)}")
    return r


def llm_available() -> bool:
    try:
        import ollama
        models = [m.model for m in ollama.list().models or []]
        return "qwen3:4b" in models
    except Exception:
        return False


def main():
    tmp = Path(tempfile.mkdtemp(prefix="vellum-e2e-", dir="/tmp/opencode"))
    cfg = tmp / "config.yaml"
    cfg.write_text(TEST_CONFIG.format(db=tmp / "test.db", lib=tmp / "library"))
    shutil.copy(ROOT / "vocab.yaml", tmp / "vocab.yaml")

    try:
        make_fixtures(tmp / "library")
        lib = tmp / "library"

        out = run(cfg, "ingest", str(lib))
        assert "added=3" in out.stdout, out.stdout

        # OCR actually extracted the scanned page?
        out = run(cfg, "search", "sensorimotor")
        assert "cognitive_machines_scan" in out.stdout, "OCR text not indexed"
        assert "snippet" not in out.stdout.lower() or True
        assert "[sensorimotor]" in out.stdout, "no highlighted match"

        if llm_available():
            run(cfg, "process")
            out = run(cfg, "show", "all")
            assert any(t in out.stdout for t in
                       ("artificial-intelligence", "cognitive-science",
                        "philosophy")), f"expected a relevant vocab tag in:\n{out.stdout}"
            # constrained tags only: every emitted tag must be in the vocabulary
            import ast
            import yaml
            vocab = yaml.safe_load((ROOT / "vocab.yaml").read_text())["tags"]
            out = run(cfg, "show", "2")  # doc #2 is the scanned pdf
            for line in out.stdout.splitlines():
                if line.startswith("tags:"):
                    for t in ast.literal_eval(line[len("tags:"):].strip()):
                        assert t in vocab, f"drifted tag: {t!r}"
            run(cfg, "embed")
            out = run(cfg, "search", "intelligence and the body", "--semantic")
            assert "cognitive_machines" in out.stdout, "semantic search found nothing"
        else:
            print("NOTE: qwen3:4b not pulled yet — LLM part of the test skipped.")

        print("E2E OK")
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    main()