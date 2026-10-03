"""Controlled tag vocabulary.

The vocabulary lives in `vocab.yaml` next to the config file:

    tags:
      epistemology: "Theory of knowledge, justification, belief"
      cryptography: "Ciphers, codes, and cryptanalysis"

`vellum vocab review` shows freeform tags the LLM proposed via `tags_other`
(surviving the constrained enum), and `vellum vocab promote` moves a suggested
tag into the controlled vocabulary so future runs can emit it directly.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path

import yaml


@dataclass
class Vocabulary:
    path: Path
    tags: dict = field(default_factory=dict)  # name -> description

    @classmethod
    def load(cls, path: Path) -> "Vocabulary":
        data = {}
        if path.is_file():
            with open(path, "r", encoding="utf-8") as f:
                data = yaml.safe_load(f) or {}
        return cls(path=path, tags=data.get("tags") or {})

    def save(self):
        self.path.parent.mkdir(parents=True, exist_ok=True)
        with open(self.path, "w", encoding="utf-8") as f:
            f.write("# Vellum controlled vocabulary.\n")
            f.write("# tag name -> description shown to the LLM when choosing tags.\n")
            yaml.safe_dump({"tags": dict(sorted(self.tags.items()))},
                           f, allow_unicode=True, sort_keys=False)

    def add(self, name: str, description: str) -> bool:
        key = name.strip().lower()
        if key in self.tags:
            return False
        self.tags[key] = description
        return True

    def remove(self, name: str) -> bool:
        return self.tags.pop(name.strip().lower(), None) is not None

    def descriptions_block(self) -> str:
        return "\n".join(f"- {k}: {v}" for k, v in sorted(self.tags.items()))