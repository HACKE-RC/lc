"""Persistent metadata cache for Codex rollout discovery.

This module is intentionally independent from transcript adapters and the TUI.
"""

from __future__ import annotations

from dataclasses import dataclass
import json
import os
from pathlib import Path


CACHE_VERSION = 1


@dataclass(frozen=True, slots=True)
class CodexCacheEntry:
    """The complete cache contract for one rollout file."""

    mtime_ns: int
    size: int
    cwd: str
    session_id: str
    title: str | None
    title_scanned: bool

    @classmethod
    def from_json(cls, value: object) -> CodexCacheEntry | None:
        if not isinstance(value, dict):
            return None
        try:
            mtime_ns = value["mtime_ns"]
            size = value["size"]
            cwd = value["cwd"]
            session_id = value["session_id"]
            title = value.get("title")
            title_scanned = value["title_scanned"]
        except KeyError:
            return None
        if not (isinstance(mtime_ns, int) and isinstance(size, int)
                and isinstance(cwd, str) and isinstance(session_id, str)
                and (title is None or isinstance(title, str))
                and isinstance(title_scanned, bool)):
            return None
        return cls(mtime_ns, size, cwd, session_id, title, title_scanned)

    def to_json(self) -> dict[str, object]:
        return {
            "mtime_ns": self.mtime_ns,
            "size": self.size,
            "cwd": self.cwd,
            "session_id": self.session_id,
            "title": self.title,
            "title_scanned": self.title_scanned,
        }

    def matches(self, stat_result: os.stat_result) -> bool:
        return self.mtime_ns == stat_result.st_mtime_ns and self.size == stat_result.st_size


class CodexCache:
    """Read unchanged entries and atomically persist meaningful batches.

    A single currently-active rollout is intentionally re-read instead of
    rewriting the entire cache on every agent turn.
    """

    def __init__(self, path: Path):
        self.path = path
        self._stored = self._load()
        self._next: dict[str, CodexCacheEntry] = {}
        self._changes = 0

    def _load(self) -> dict[str, CodexCacheEntry]:
        try:
            with open(self.path, encoding="utf-8") as fh:
                data = json.load(fh)
        except (OSError, json.JSONDecodeError):
            return {}
        if not isinstance(data, dict) or data.get("version") != CACHE_VERSION:
            return {}
        raw_entries = data.get("sessions")
        if not isinstance(raw_entries, dict):
            return {}
        return {
            path: entry
            for path, raw in raw_entries.items()
            if isinstance(path, str) and (entry := CodexCacheEntry.from_json(raw)) is not None
        }

    def get(self, path: str, stat_result: os.stat_result) -> CodexCacheEntry | None:
        entry = self._stored.get(path)
        if entry is not None and entry.matches(stat_result):
            return entry
        self._changes += 1
        return None

    def remember(self, path: str, entry: CodexCacheEntry, *, changed: bool = False) -> None:
        self._next[path] = entry
        if changed:
            self._changes += 1

    def flush(self) -> None:
        if self._stored and self._changes <= 1 and abs(len(self._next) - len(self._stored)) <= 1:
            return
        try:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            temporary = self.path.with_suffix(".tmp")
            with open(temporary, "w", encoding="utf-8") as fh:
                json.dump(
                    {"version": CACHE_VERSION,
                     "sessions": {path: entry.to_json() for path, entry in self._next.items()}},
                    fh,
                    separators=(",", ":"),
                )
            os.replace(temporary, self.path)
        except OSError:
            pass
