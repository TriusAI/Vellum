"""Configuration loading.

Everything (db file, cache) lives relative to the directory containing the
config file, so the whole library is portable as one folder.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path

import yaml

DEFAULTS = {
    "library_dir": "~/VellumLibrary",
    "db": "library.db",
    "models": {
        "llm": "qwen3:4b",
        "embed": "nomic-embed-text",
    },
    "llm": {
        "think": False,          # qwen3 thinking mode: better but much slower on CPU
        "num_ctx": 8192,
        "temperature": 0.3,
    },
    "ocr": {
        "langs": "eng+chi_sim+fin",  # tesseract language codes, '+'-separated
        "dpi": 300,
        "min_chars_per_page": 50,    # fewer extractable chars than this -> page is scanned
        "workers": 3,                # parallel tesseract workers
    },
    "summarize": {
        "chunk_chars": 6000,        # map-phase chunk size (characters)
        "max_tags": 8,
    },
    "embed": {
        "batch": 32,
    },
}


def _deep_merge(base: dict, override: dict) -> dict:
    out = dict(base)
    for k, v in (override or {}).items():
        if isinstance(v, dict) and isinstance(out.get(k), dict):
            out[k] = _deep_merge(out[k], v)
        else:
            out[k] = v
    return out


@dataclass
class Config:
    base_dir: Path
    data: dict = field(default_factory=dict)

    @property
    def db_path(self) -> Path:
        return (self.base_dir / self.data["db"]).resolve()

    @property
    def library_dir(self) -> Path:
        p = Path(self.data["library_dir"]).expanduser()
        return p if p.is_absolute() else (self.base_dir / p).resolve()

    @property
    def vocab_path(self) -> Path:
        return self.base_dir / "vocab.yaml"

    def get(self, *keys, default=None):
        node = self.data
        for k in keys:
            if not isinstance(node, dict) or k not in node:
                return default
            node = node[k]
        return node


def load_config(path: str | os.PathLike | None = None) -> Config:
    """Load config from `path`, else the first of: $VELLUM_CONFIG, ./config.yaml."""
    candidates = []
    if path:
        candidates.append(Path(path))
    env = os.environ.get("VELLUM_CONFIG")
    if env:
        candidates.append(Path(env))
    candidates.append(Path.cwd() / "config.yaml")

    cfg_path = next((p for p in candidates if p.is_file()), None)
    user_data = {}
    base_dir = cfg_path.parent if cfg_path else Path.cwd()
    if cfg_path:
        with open(cfg_path, "r", encoding="utf-8") as f:
            user_data = yaml.safe_load(f) or {}

    return Config(base_dir=base_dir, data=_deep_merge(DEFAULTS, user_data))