#!/usr/bin/env python3
"""lc - list coding-agent sessions for the current repository.

Scans the local session stores of every coding agent installed on this machine
and prints the sessions whose working directory lives inside the current git
repository (or the current directory when not in a repo).

Supported agents: claude (Claude Code), codex, droid (Factory), opencode,
cursor, copilot, grok, kimi, gemini.

`lc -I` browses the same list with vim keys and resumes the selected session
in the agent that created it.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import time
import urllib.parse
from fnmatch import fnmatch
from pathlib import Path

from lc_core.codex_cache import CodexCache, CodexCacheEntry

HOME = Path.home()
CACHE_DIR = Path(os.environ.get("XDG_CACHE_HOME", HOME / ".cache")) / "lc"
CODEX_CACHE = CACHE_DIR / "codex-sessions.json"

# ---------------------------------------------------------------- primitives


def read_head(path: Path, limit: int) -> bytes:
    try:
        with open(path, "rb") as fh:
            return fh.read(limit)
    except OSError:
        return b""


def json_str(blob: bytes, key: str) -> str | None:
    """Pull a plain JSON string value out of a raw byte blob."""
    m = re.search(rb'"' + re.escape(key.encode()) + rb'"\s*:\s*"((?:[^"\\]|\\.)*)"', blob)
    if not m:
        return None
    try:
        return json.loads(b'"' + m.group(1) + b'"')
    except Exception:
        return None


def iter_lines(blob: bytes):
    """Yield parsed objects from raw jsonl bytes, skipping unparsable lines."""
    for line in blob.split(b"\n"):
        line = line.strip()
        if not line.startswith(b"{"):
            continue
        try:
            yield json.loads(line)
        except Exception:
            continue


def codex_session_names() -> dict[str, str]:
    """Read Codex's compact thread index, which owns display names."""
    names = {}
    path = HOME / ".codex" / "session_index.jsonl"
    try:
        with open(path, "rb") as fh:
            for line in fh:
                try:
                    entry = json.loads(line)
                except Exception:
                    continue
                sid = entry.get("id")
                name = clean_title(entry.get("thread_name"))
                if sid and name:
                    names[sid] = name
    except OSError:
        pass
    return names


def claude_session_names() -> dict[str, str]:
    """Read Claude Code's live-session records, which own display names."""
    names = {}
    base = HOME / ".claude" / "sessions"
    try:
        for path in base.glob("*.json"):
            entry = load_json(path) or {}
            sid = entry.get("sessionId")
            name = clean_title(entry.get("name"))
            if sid and name:
                names[sid] = name
    except OSError:
        pass
    return names


def jsonl_head(path: Path, limit: int):
    """Yield parsed objects from the first `limit` bytes of a jsonl file."""
    return iter_lines(read_head(path, limit))


def load_json(path: Path):
    try:
        with open(path, "rb") as fh:
            return json.load(fh)
    except Exception:
        return None


def mtime(path: Path) -> float:
    try:
        return path.stat().st_mtime
    except OSError:
        return 0.0


def size_of(path: Path) -> int:
    try:
        return path.stat().st_size
    except OSError:
        return 0


def parse_ts(value) -> float:
    """Accept ISO-8601 strings or epoch seconds/milliseconds."""
    if value is None:
        return 0.0
    if isinstance(value, (int, float)):
        v = float(value)
        return v / 1000.0 if v > 1e11 else v
    if isinstance(value, str):
        s = value.strip().replace("Z", "+00:00")
        # trim sub-microsecond precision that fromisoformat rejects
        s = re.sub(r"(\.\d{6})\d+", r"\1", s)
        try:
            import datetime

            return datetime.datetime.fromisoformat(s).timestamp()
        except Exception:
            return 0.0
    return 0.0


# Agents prepend injected context (repo instructions, plugin lists, hook
# output) as ordinary "user" turns; those are not what the human typed.
NOISE_PREFIXES = (
    "caveat: the messages below",
    "## memory",
    "# agents.md",
    "# claude.md",
    "this session is being continued",
    "please continue the conversation from where",
)
NOISE_RE = re.compile(r"^<[a-zA-Z_][\w:-]*[\s>]")  # an XML-ish wrapper tag
# injected blocks that surround (or replace) the human's text
WRAPPER_RE = re.compile(
    r"<(system-reminder|recommended_plugins|environment_context|user_instructions"
    r"|ide_context|ide_selection|local-command-\w+|attachment|system_context"
    r"|session_context)\b"
    r".*?</\1>",
    re.S,
)
TITLE_SOURCE_LIMIT = 1 << 16
TITLE_DISPLAY_LIMIT = 240


def clean_title(text) -> str | None:
    """Normalise a candidate title, or return None if it is machine noise."""
    if not isinstance(text, str):
        return None
    # Transcript turns may contain megabytes of injected context. A title is a
    # label, not a transcript export: bound regex work and cached output.
    text = text[:TITLE_SOURCE_LIMIT]
    cmd = re.search(r"<command-name>\s*(.*?)\s*</command-name>", text)
    if cmd:
        return cmd.group(1).strip() or None
    text = WRAPPER_RE.sub(" ", text)
    text = re.sub(r"\s+", " ", text).strip()
    if not text:
        return None
    low = text.lower()
    if any(low.startswith(p) for p in NOISE_PREFIXES) or NOISE_RE.match(text):
        return None
    if re.match(r"^#+ .{0,40}instructions for /", text):
        return None
    return text[:TITLE_DISPLAY_LIMIT].rstrip()


def strip_wrappers(text) -> str | None:
    """Like clean_title but for mid-conversation preview turns: strips injected
    wrapper blocks and collapses whitespace, without rejecting slash commands
    or short lines the way a title candidate would be."""
    if not isinstance(text, str):
        return None
    text = WRAPPER_RE.sub(" ", text)
    text = re.sub(r"\s+", " ", text).strip()
    return text or None


class TitlePick:
    """Pick the most descriptive early user turn as a durable title fallback."""

    __slots__ = ("best", "fallback")

    def __init__(self):
        self.best = self.fallback = None

    def offer(self, text) -> bool:
        title = clean_title(text)
        if not title:
            return False
        if title.startswith("/") and len(title) < 24 and "\n" not in title:
            self.fallback = self.fallback or title
            return False
        self.best = title
        return True

    @property
    def value(self):
        return self.best or self.fallback


def blocks_to_text(content) -> str | None:
    """Flatten an Anthropic/OpenAI style content field into plain text."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = []
        for b in content:
            if not isinstance(b, dict):
                continue
            if b.get("type") in ("text", "input_text") and isinstance(b.get("text"), str):
                parts.append(b["text"])
        return "\n".join(parts) if parts else None
    return None


class Session:
    __slots__ = ("agent", "sid", "cwd", "title", "ts", "path", "size", "note")

    def __init__(self, agent, sid, cwd, title, ts, path, size=0, note=""):
        self.agent = agent
        self.sid = sid or ""
        self.cwd = cwd or ""
        self.title = title or ""
        self.ts = ts or 0.0
        self.path = str(path)
        self.size = size
        self.note = note


# ------------------------------------------------------------------ adapters
# Each adapter yields Session objects. `keep(cwd)` decides repo membership;
# `dir_ok(name)` pre-filters path-mangled directory names so we never open
# files belonging to other repositories.


CWD_HEAD = 1 << 16  # cwd appears in the opening session metadata
HEAD = 1 << 18  # bytes of a matching transcript scanned for a title fallback
RESCAN = 2 << 20  # retry budget when that window held no usable title


def scan_twice(scan, size: int):
    """Run a cheap head scan, widening only when it found no usable title."""
    cwd, pick = scan(HEAD)
    if not pick.value and size > HEAD:
        cwd2, pick2 = scan(min(size, RESCAN))
        cwd = cwd or cwd2
        if pick2.value:
            pick = pick2
    return cwd, pick


def a_claude(keep, dir_ok):
    base = HOME / ".claude" / "projects"
    names = claude_session_names()
    for pdir in sorted(iter_dirs(base)):
        if not dir_ok(pdir.name):
            continue
        for f in pdir.glob("*.jsonl"):

            def scan(limit, f=f):
                cwd, name = None, names.get(f.stem)
                pick = TitlePick()
                if name:
                    pick.best = name
                for entry in jsonl_head(f, limit):
                    if cwd is None and isinstance(entry.get("cwd"), str):
                        cwd = entry["cwd"]
                    if name is None:
                        name = clean_title(entry.get("aiTitle"))
                        if name:
                            pick.best = name
                    if name is None and entry.get("type") == "user" and not entry.get("isMeta"):
                        pick.offer(blocks_to_text((entry.get("message") or {}).get("content")))
                    if cwd and (name or pick.best):
                        break
                return cwd, pick

            size = size_of(f)
            cwd, pick = scan_twice(scan, size)
            if cwd and not keep(cwd):
                continue
            yield Session("claude", f.stem, cwd or str(pdir.name), pick.value, mtime(f), f, size)


def a_codex(keep, dir_ok):
    base = HOME / ".codex" / "sessions"
    names = codex_session_names()
    cache = CodexCache(CODEX_CACHE)
    for f in base.rglob("rollout-*.jsonl"):
        path = str(f)
        try:
            stat = f.stat()
        except OSError:
            continue
        cached = cache.get(path, stat)
        if cached is not None:
            cwd = cached.cwd
            sid = cached.session_id
            cached_title = cached.title
            title_scanned = cached.title_scanned
            record_changed = False
        else:
            # Most rollouts belong to another project. Read only their compact
            # session metadata before deciding whether a title scan is needed.
            blob = read_head(f, CWD_HEAD)
            cwd = json_str(blob, "cwd")
            sid = re.sub(r"^rollout-\d{4}-\d\d-\d\dT[\d-]+-", "", f.stem)
            cached_title = None
            title_scanned = False
            record_changed = True
        if not isinstance(cwd, str) or not cwd:
            continue
        title = names.get(sid) or cached_title
        if not keep(cwd):
            cache.remember(path, CodexCacheEntry(
                stat.st_mtime_ns, stat.st_size, cwd, sid, title, title_scanned,
            ))
            continue
        title_cache_changed = False
        if title is None and not title_scanned:
            blob = read_head(f, HEAD)

            def scan(limit, f=f, blob=blob):
                pick = TitlePick()
                for entry in iter_lines(blob if limit <= len(blob) else read_head(f, limit)):
                    payload = entry.get("payload") or {}
                    if entry.get("type") == "response_item" and payload.get("role") == "user":
                        done = pick.offer(blocks_to_text(payload.get("content")))
                    elif entry.get("type") == "event_msg" and payload.get("type") == "user_message":
                        done = pick.offer(payload.get("message"))
                    else:
                        continue
                    if done:
                        break
                return cwd, pick

            _, pick = scan_twice(scan, stat.st_size)
            title = pick.value
            title_scanned = True
            title_cache_changed = not record_changed
        cache.remember(path, CodexCacheEntry(
            stat.st_mtime_ns, stat.st_size, cwd, sid, title, title_scanned,
        ), changed=title_cache_changed)
        yield Session("codex", sid, cwd, title, stat.st_mtime, f, stat.st_size)
    cache.flush()


def a_droid(keep, dir_ok):
    base = HOME / ".factory" / "sessions"
    for pdir in iter_dirs(base):
        if not dir_ok(pdir.name):
            continue
        for f in pdir.glob("*.jsonl"):

            def scan(limit, f=f):
                cwd, name = None, None
                pick = TitlePick()
                for entry in jsonl_head(f, limit):
                    if entry.get("type") == "session_start":
                        cwd = entry.get("cwd")
                        name = clean_title(entry.get("title"))
                        if name and name.lower() in ("new session", "start new chat"):
                            name = None
                    elif entry.get("type") == "message" and not pick.best:
                        message = entry.get("message") or {}
                        if message.get("role") == "user" and not message.get("hookEventName"):
                            pick.offer(blocks_to_text(message.get("content")))
                    if cwd and (name or pick.best):
                        break
                return cwd, name or pick.value

            size = size_of(f)
            cwd, name = scan(HEAD)
            if name is None and size > HEAD:
                cwd2, name = scan(min(size, RESCAN))
                cwd = cwd or cwd2
            if cwd and not keep(cwd):
                continue
            yield Session("droid", f.stem, cwd or pdir.name, name, mtime(f), f, size)


def a_opencode(keep, dir_ok):
    base = HOME / ".local" / "share" / "opencode" / "storage" / "session"
    for f in base.rglob("*.json"):
        d = load_json(f)
        if not isinstance(d, dict):
            continue
        cwd = d.get("directory")
        if not cwd or not keep(cwd):
            continue
        t = d.get("time") or {}
        ts = parse_ts(t.get("updated") or t.get("created")) or mtime(f)
        yield Session("opencode", d.get("id", f.stem), cwd, clean_title(d.get("title")), ts, f)


def a_cursor(keep, dir_ok, candidates=()):
    base = HOME / ".cursor" / "chats"
    if not base.is_dir():
        return
    for cwd in candidates:
        # candidates are a superset (root, its children, other agents' cwds);
        # re-check keep() since folder filters aren't applied to that set
        if not keep(cwd):
            continue
        wdir = base / hashlib.md5(cwd.encode()).hexdigest()
        if not wdir.is_dir():
            continue
        for sdir in iter_dirs(wdir):
            db = sdir / "store.db"
            if not db.exists():
                continue
            meta = {}
            try:
                con = sqlite3.connect(f"file:{db}?mode=ro&immutable=1", uri=True)
                con.text_factory = bytes
                row = con.execute("select value from meta order by key limit 1").fetchone()
                con.close()
                if row and row[0]:
                    raw = row[0]
                    if isinstance(raw, bytes):
                        raw = raw.decode("utf-8", "replace")
                    # cursor stores the metadata JSON hex-encoded
                    if re.fullmatch(r"(?:[0-9a-fA-F]{2})+", raw):
                        raw = bytes.fromhex(raw).decode("utf-8", "replace")
                    meta = json.loads(raw)
            except Exception:
                pass
            ts = parse_ts(meta.get("createdAt")) or mtime(db)
            yield Session(
                "cursor",
                meta.get("agentId", sdir.name),
                cwd,
                clean_title(meta.get("name")),
                max(ts, mtime(db)),
                db,
                size_of(db),
                meta.get("lastUsedModel", ""),
            )


def a_copilot(keep, dir_ok):
    base = HOME / ".copilot" / "session-state"
    for sdir in iter_dirs(base):
        wf = sdir / "workspace.yaml"
        if not wf.exists():
            continue
        info = {}
        for line in read_head(wf, 8192).decode("utf-8", "replace").splitlines():
            if ":" in line and not line.startswith((" ", "-")):
                k, _, v = line.partition(":")
                info[k.strip()] = v.strip().strip("'\"")
        cwd = info.get("cwd")
        if not cwd or not keep(cwd):
            continue
        ts = parse_ts(info.get("updated_at") or info.get("created_at")) or mtime(wf)
        events = sdir / "events.jsonl"
        yield Session("copilot", info.get("id", sdir.name), cwd, clean_title(info.get("summary")),
                      max(ts, mtime(events)), sdir, size_of(events))


def a_grok(keep, dir_ok):
    base = HOME / ".grok" / "sessions"
    for wdir in iter_dirs(base):
        cwd = urllib.parse.unquote(wdir.name)
        if not keep(cwd):
            continue
        prompts: dict[str, str] = {}
        history = wdir / "prompt_history.jsonl"
        if history.exists():
            for entry in jsonl_head(history, 1 << 20):
                sid, prompt = entry.get("session_id"), entry.get("prompt")
                if sid and prompt and sid not in prompts and not entry.get("is_bash"):
                    prompts[sid] = prompt
        for sdir in iter_dirs(wdir):
            summary = load_json(sdir / "summary.json") or {}
            sid = (summary.get("info") or {}).get("id", sdir.name)
            title = clean_title(summary.get("session_summary")) or clean_title(prompts.get(sid))
            ts = parse_ts(summary.get("last_active_at") or summary.get("updated_at")) or mtime(sdir)
            chat = sdir / "chat_history.jsonl"
            n = summary.get("num_messages")
            yield Session("grok", sid, cwd, title, max(ts, mtime(chat)), sdir, size_of(chat),
                          f"{n} msgs" if n else "")


def a_kimi(keep, dir_ok):
    index = HOME / ".kimi-code" / "session_index.jsonl"
    for e in jsonl_head(index, 1 << 20):
        cwd, sdir = e.get("workDir"), e.get("sessionDir")
        if not cwd or not sdir or not keep(cwd):
            continue
        sdir = Path(sdir)
        st = load_json(sdir / "state.json") or {}
        title = clean_title(st.get("title")) or clean_title(st.get("lastPrompt"))
        ts = parse_ts(st.get("updatedAt") or st.get("createdAt")) or mtime(sdir)
        yield Session("kimi", e.get("sessionId", sdir.name), cwd, title, ts, sdir)


def gemini_text(content) -> str | None:
    """Gemini message content is a plain string in older sessions, or a list
    of {"text": ...} blocks (no "type" tag, unlike Anthropic/OpenAI blocks)."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = [b.get("text", "") for b in content
                 if isinstance(b, dict) and isinstance(b.get("text"), str)]
        return "\n".join(parts) if parts else None
    return None


GEMINI_ROLE = {"user": "user", "gemini": "assistant"}


def gemini_messages(f: Path) -> list[tuple[str, str]]:
    """(role, text) turns for one gemini chat file, oldest first.

    The .jsonl variant is a log of `{"$set": {...}}` patches rather than one
    JSON document; when a patch sets "messages" it's the full array, not a
    delta, so the last one seen is the current state.
    """
    if f.suffix == ".json":
        raw = (load_json(f) or {}).get("messages") or []
    else:
        raw = []
        for e in iter_lines(read_head(f, 1 << 21)):
            m = (e.get("$set") or {}).get("messages")
            if isinstance(m, list):
                raw = m
    out = []
    for m in raw:
        if not isinstance(m, dict):
            continue
        role = GEMINI_ROLE.get(m.get("type"))
        text = strip_wrappers(gemini_text(m.get("content"))) if role else None
        if text:
            out.append((role, text))
    return out


def a_gemini(keep, dir_ok, candidates=()):
    base = HOME / ".gemini" / "tmp"
    by_hash = {hashlib.sha256(c.encode()).hexdigest(): c for c in candidates}
    for pdir in iter_dirs(base):
        root_file = pdir / ".project_root"
        cwd = None
        if root_file.exists():
            cwd = read_head(root_file, 4096).decode("utf-8", "replace").strip() or None
        if cwd is None:
            cwd = by_hash.get(pdir.name)
        if not cwd or not keep(cwd):
            continue
        chats = pdir / "chats"
        if not chats.is_dir():
            continue
        for f in sorted(chats.iterdir()):
            if f.suffix not in (".json", ".jsonl"):
                continue
            sid, ts = f.stem, mtime(f)
            if f.suffix == ".json":
                d = load_json(f) or {}
                sid = d.get("sessionId", sid)
                ts = parse_ts(d.get("lastUpdated") or d.get("startTime")) or ts
            else:
                blob = read_head(f, 1 << 16)
                sid = json_str(blob, "sessionId") or sid
                ts = parse_ts(json_str(blob, "lastUpdated") or json_str(blob, "startTime")) or ts
            title = next((text for role, text in gemini_messages(f) if role == "user"), None)
            yield Session("gemini", sid, cwd, title, ts, f, size_of(f))


def iter_dirs(base: Path):
    try:
        return [p for p in base.iterdir() if p.is_dir()]
    except OSError:
        return []


ADAPTERS = {
    "claude": a_claude,
    "codex": a_codex,
    "droid": a_droid,
    "opencode": a_opencode,
    "cursor": a_cursor,
    "copilot": a_copilot,
    "grok": a_grok,
    "kimi": a_kimi,
    "gemini": a_gemini,
}

# Catppuccin Mocha.  Use its named roles consistently instead of an unrelated
# color per element: hierarchy comes from text/overlay/surface, while color is
# reserved for agent identity and Markdown meaning.
CP_TEXT = "38;2;205;214;244"
CP_SUBTEXT = "38;2;166;173;200"
CP_OVERLAY = "38;2;108;112;134"
CP_SURFACE = "38;2;69;71;90"
CP_CRUST = "48;2;17;17;27"
CP_BLUE = "38;2;137;180;250"
CLAUDE_ORANGE = "38;2;210;115;84"  # supplied swatch: #d27354
CP_LAVENDER = "38;2;180;190;254"
CP_MAUVE = "38;2;203;166;247"
CP_GREEN = "38;2;166;227;161"
CP_TEAL = "38;2;148;226;213"
CP_SKY = "38;2;137;220;235"
CP_YELLOW = "38;2;249;226;175"
CP_PEACH = "38;2;250;179;135"
CP_PINK = "38;2;245;194;231"
CP_FLAMINGO = "38;2;242;205;205"
CP_SELECTED = "48;2;69;71;90;38;2;205;214;244"
CP_FACTS = f"1;{CP_CRUST};{CP_TEXT}"

COLORS = {
    "claude": CLAUDE_ORANGE,
    "codex": CP_BLUE,
    "droid": CP_PINK,
    "opencode": CP_GREEN,
    "cursor": CP_TEAL,
    "copilot": CP_BLUE,
    "grok": CP_MAUVE,
    "kimi": CP_FLAMINGO,
    "gemini": CP_YELLOW,
}

# --------------------------------------------------------------- previewers
# One function per agent: given a Session, return its last few (role, text)
# turns, oldest first. Each reads only a bounded tail window of its transcript
# (widening it if that window held no message at all), so opening the preview
# pane costs nothing proportional to a session's total size.

TAIL_SIZES = (1 << 18, 1 << 20, 1 << 22)  # 256K, 1M, 4M
MAX_TURNS = 8


def tail_bytes(path: Path, limit: int) -> bytes:
    try:
        size = path.stat().st_size
        with open(path, "rb") as fh:
            if size > limit:
                fh.seek(-limit, os.SEEK_END)
                fh.readline()  # drop the partial line the seek landed inside
            return fh.read()
    except OSError:
        return b""


def widen_tail(path: Path, parse):
    """Try successively larger tail windows until `parse` finds a turn."""
    found = []
    for size in TAIL_SIZES:
        found = parse(tail_bytes(path, size))
        if found or size >= size_of(path):
            break
    return found


def prev_claude(s: Session):
    def parse(blob):
        out = []
        for e in iter_lines(blob):
            if e.get("type") not in ("user", "assistant") or e.get("isMeta"):
                continue
            msg = e.get("message") or {}
            text = strip_wrappers(blocks_to_text(msg.get("content")))
            if text:
                out.append((msg.get("role") or e["type"], text))
        return out[-MAX_TURNS:]
    return widen_tail(Path(s.path), parse)


def prev_codex(s: Session):
    def parse(blob):
        out = []
        for e in iter_lines(blob):
            p = e.get("payload") or {}
            if e.get("type") != "event_msg" or p.get("type") not in ("user_message", "agent_message"):
                continue
            text = strip_wrappers(p.get("message"))
            if text:
                role = "user" if p["type"] == "user_message" else "assistant"
                out.append((role, text))
        return out[-MAX_TURNS:]
    return widen_tail(Path(s.path), parse)


def prev_droid(s: Session):
    def parse(blob):
        out = []
        for e in iter_lines(blob):
            if e.get("type") != "message":
                continue
            msg = e.get("message") or {}
            if msg.get("hookEventName") or msg.get("role") not in ("user", "assistant"):
                continue
            text = strip_wrappers(blocks_to_text(msg.get("content")))
            if text:
                out.append((msg["role"], text))
        return out[-MAX_TURNS:]
    return widen_tail(Path(s.path), parse)


def prev_opencode(s: Session):
    mdir = HOME / ".local" / "share" / "opencode" / "storage" / "message" / s.sid
    pdir_base = HOME / ".local" / "share" / "opencode" / "storage" / "part"
    try:
        files = sorted(mdir.iterdir(), key=mtime)
    except OSError:
        return []
    out = []
    for f in files[-MAX_TURNS:]:
        d = load_json(f) or {}
        role = d.get("role")
        if role not in ("user", "assistant"):
            continue
        texts = []
        try:
            for pf in sorted((pdir_base / d.get("id", "")).iterdir()):
                pd = load_json(pf) or {}
                if pd.get("type") == "text" and isinstance(pd.get("text"), str):
                    texts.append(pd["text"])
        except OSError:
            pass
        text = strip_wrappers("\n".join(texts))
        if text:
            out.append((role, text))
    return out


def prev_copilot(s: Session):
    def parse(blob):
        out = []
        for e in iter_lines(blob):
            t, d = e.get("type"), e.get("data") or {}
            if t not in ("user.message", "assistant.message"):
                continue
            text = strip_wrappers(d.get("content"))
            if text:
                out.append(("user" if t == "user.message" else "assistant", text))
        return out[-MAX_TURNS:]
    return widen_tail(Path(s.path) / "events.jsonl", parse)


def prev_grok(s: Session):
    def parse(blob):
        out = []
        for e in iter_lines(blob):
            role = e.get("type")
            if role not in ("user", "assistant"):
                continue
            text = strip_wrappers(blocks_to_text(e.get("content")))
            if text:
                out.append((role, text))
        return out[-MAX_TURNS:]
    return widen_tail(Path(s.path) / "chat_history.jsonl", parse)


def prev_kimi(s: Session):
    def parse(blob):
        out = []
        for e in iter_lines(blob):
            if e.get("type") != "context.append_message":
                continue
            msg = e.get("message") or {}
            if msg.get("role") not in ("user", "assistant"):
                continue
            text = strip_wrappers(blocks_to_text(msg.get("content")))
            if text:
                out.append((msg["role"], text))
        return out[-MAX_TURNS:]
    return widen_tail(Path(s.path) / "agents" / "main" / "wire.jsonl", parse)


def prev_gemini(s: Session):
    return gemini_messages(Path(s.path))[-MAX_TURNS:]


PREVIEW = {
    "claude": prev_claude,
    "codex": prev_codex,
    "droid": prev_droid,
    "opencode": prev_opencode,
    "copilot": prev_copilot,
    "grok": prev_grok,
    "kimi": prev_kimi,
    "gemini": prev_gemini,
    # cursor's transcript lives in an undocumented sqlite blob format we don't
    # decode, so it has no previewer and falls back to a plain notice.
}

ANSI_RE = re.compile(r"\x1b\[[0-9;]*m")
ROLE_LABEL = {"user": "you", "assistant": "agent"}
ROLE_COLOR = {"user": CP_SKY, "assistant": CP_TEXT}
MD_HEADING = f"1;{CP_LAVENDER}"
MD_EMPHASIS = f"3;{CP_TEXT}"
MD_STRONG = f"1;{CP_PEACH}"
MD_CODE = CP_YELLOW
MD_LINK = f"4;{CP_BLUE}"
MD_URL = f"2;{CP_OVERLAY}"
MD_QUOTE = CP_GREEN
MD_LIST = f"1;{CP_MAUVE}"
MD_RULE = f"2;{CP_SURFACE}"
MD_FENCE = f"2;{CP_PEACH}"


def vlen(s: str) -> int:
    return len(ANSI_RE.sub("", s))


def vpad(s: str, width: int) -> str:
    return s + " " * max(0, width - vlen(s))


def clip(s: str, width: int) -> str:
    """Hard-truncate a line to `width` visible columns, ANSI codes aside.

    Backstop against terminal auto-wrap: the split-pane browser redraws with
    absolute cursor positioning, so a single line one column too wide makes
    the terminal wrap it, pushing every later line down and turning the whole
    screen into an unrecoverable scroll of overlapping frames. Every width
    computation upstream should already respect `width`, but a stale
    COLUMNS/LINES env var or an untested wrapping edge case only has to be
    wrong once for that to happen — this makes it structurally impossible.
    """
    if width <= 0:
        return ""
    csi = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")  # any CSI sequence, not just color
    if len(csi.sub("", s)) <= width:
        return s
    out, visible, i = [], 0, 0
    while i < len(s) and visible < width:
        m = csi.match(s, i)
        if m:
            out.append(m.group())
            i = m.end()
            continue
        out.append(s[i])
        visible += 1
        i += 1
    out.append("\033[0m")
    return "".join(out)


def term_size(fd=None, fallback=(100, 24)):
    """Real terminal size via ioctl, bypassing shutil.get_terminal_size()'s
    preference for COLUMNS/LINES env vars — those go stale in embedded or
    wrapped terminals (never resized after the shell started), and trusting
    them over the actual display is exactly what causes the wrap-and-scroll
    corruption `clip()` above exists to contain.
    """
    for candidate in (fd, 1):  # 1 = stdout fd, always valid in a real process
        if candidate is None:
            continue
        try:
            return os.get_terminal_size(candidate)
        except OSError:
            continue
    return os.terminal_size(fallback)


def span_add(spans, style: str | None, text: str):
    """Append text to styled spans, coalescing adjacent equal styles."""
    if not text:
        return
    if spans and spans[-1][0] == style:
        spans[-1] = style, spans[-1][1] + text
    else:
        spans.append((style, text))


def markdown_inline(text: str, base_style: str) -> list[tuple[str | None, str]]:
    """Render the useful inline Markdown forms without an external dependency.

    This intentionally favors a bounded, forgiving terminal preview over a
    full CommonMark implementation. Unmatched delimiters stay visible as
    ordinary text, which is much less surprising in an in-progress response.
    """
    spans: list[tuple[str | None, str]] = []
    plain, i = [], 0

    def flush_plain():
        nonlocal plain
        span_add(spans, base_style, "".join(plain))
        plain = []

    while i < len(text):
        # Images and links: terminal previews cannot open a link, so retain a
        # compact dim URL after its readable label.
        image = text.startswith("![", i)
        if image or text.startswith("[", i):
            label_start = i + 2 if image else i + 1
            label_end = text.find("](", label_start)
            if label_end >= 0:
                url_end = text.find(")", label_end + 2)
                if url_end >= 0:
                    flush_plain()
                    label, url = text[label_start:label_end], text[label_end + 2:url_end]
                    if image:
                        span_add(spans, MD_EMPHASIS, "image: ")
                    span_add(spans, MD_LINK, label)
                    if url:
                        span_add(spans, MD_URL, f" ({url})")
                    i = url_end + 1
                    continue

        if text[i] == "`":
            end = text.find("`", i + 1)
            if end >= 0:
                flush_plain()
                span_add(spans, MD_CODE, text[i + 1:end])
                i = end + 1
                continue

        matched = False
        for delimiter, style in (("**", MD_STRONG), ("__", MD_STRONG),
                                 ("~~", MD_EMPHASIS), ("*", MD_EMPHASIS),
                                 ("_", MD_EMPHASIS)):
            if not text.startswith(delimiter, i):
                continue
            end = text.find(delimiter, i + len(delimiter))
            if end < 0 or end == i + len(delimiter):
                continue
            # An underscore inside a word is not emphasis.
            if delimiter == "_" and i and text[i - 1].isalnum():
                continue
            flush_plain()
            span_add(spans, style, text[i + len(delimiter):end])
            i = end + len(delimiter)
            matched = True
            break
        if matched:
            continue

        plain.append(text[i])
        i += 1
    flush_plain()
    return spans


def ansi_spans(spans, color: bool) -> str:
    """Join style spans, adding only ANSI SGR sequences when colors are on."""
    if not color:
        return "".join(text for _, text in spans)
    return "".join(f"\033[{style}m{text}\033[0m" if style else text
                   for style, text in spans)


def wrap_spans(spans, width: int, first_indent="", next_indent=""):
    """Word-wrap styled spans by visible columns, never counting ANSI bytes."""
    width = max(1, width)
    indent = first_indent
    line, visible, has_text, pending_space = [(None, indent)], len(indent), False, False
    wrapped = []

    def finish():
        nonlocal line, visible, has_text, pending_space, indent
        wrapped.append(line)
        indent = next_indent
        line, visible, has_text, pending_space = [(None, indent)], len(indent), False, False

    for style, text in spans:
        for part in re.findall(r"\s+|\S+", text):
            if part.isspace():
                pending_space = pending_space or has_text
                continue
            separator = " " if pending_space and has_text else ""
            needed = len(separator) + len(part)
            if has_text and visible + needed > width:
                finish()
                separator = ""
            if separator:
                span_add(line, None, separator)
                visible += 1
            # URLs and unbroken code tokens still have to fit the pane. Split
            # them only after exhausting a full visible line.
            while part and visible + len(part) > width:
                available = width - visible
                if available <= 0:
                    finish()
                    continue
                span_add(line, style, part[:available])
                part = part[available:]
                visible += available
                has_text, pending_space = True, False
                finish()
            if part:
                span_add(line, style, part)
                visible += len(part)
                has_text = True
            pending_space = False
    if has_text or not wrapped:
        wrapped.append(line)
    return wrapped


def wrap_verbatim(text: str, style: str, width: int, first_indent="", next_indent=""):
    """Hard-wrap code while retaining its whitespace instead of collapsing it."""
    width = max(1, width)
    indent, rest, wrapped = first_indent, text.expandtabs(2), []
    while True:
        available = max(1, width - len(indent))
        chunk, rest = rest[:available], rest[available:]
        wrapped.append([(None, indent), (style, chunk)])
        if not rest:
            return wrapped
        indent = next_indent


def render_markdown(text: str, width: int, role: str, color: bool) -> list[str]:
    """Render a chat turn as compact, colorized Markdown for the preview pane."""
    base_style = ROLE_COLOR.get(role, "0")
    role_prefix = f"{ROLE_LABEL.get(role, role)}: "
    continuation = " " * len(role_prefix)
    lines: list[str] = []
    first_content = True
    fenced = False

    def indents(block_first="", block_next=""):
        prefix = role_prefix if first_content else continuation
        return prefix + block_first, continuation + block_next

    def add_wrapped(spans, block_first="", block_next=""):
        nonlocal first_content
        first_indent, next_indent = indents(block_first, block_next)
        lines.extend(ansi_spans(line, color)
                     for line in wrap_spans(spans, width, first_indent, next_indent))
        first_content = False

    def add_code(raw, block_first="  ", block_next="  "):
        nonlocal first_content
        first_indent, next_indent = indents(block_first, block_next)
        lines.extend(ansi_spans(line, color)
                     for line in wrap_verbatim(raw, MD_CODE, width, first_indent, next_indent))
        first_content = False

    for raw in text.splitlines() or [""]:
        fence = re.match(r"^\s*(`{3,}|~{3,})\s*([^`]*)$", raw)
        if fence:
            if not fenced:
                language = fence.group(2).strip()
                add_wrapped([(MD_FENCE, f"┌─ {language or 'code'}")])
                fenced = True
            else:
                add_wrapped([(MD_FENCE, "└─")])
                fenced = False
            continue
        if fenced:
            add_code(raw)
            continue
        if not raw.strip():
            if lines:
                lines.append("")
            continue

        heading = re.match(r"^\s{0,3}#{1,6}\s+(.+?)(?:\s+#+)?$", raw)
        if heading:
            add_wrapped(markdown_inline(heading.group(1), MD_HEADING), "▍ ", "  ")
            continue
        if re.match(r"^\s{0,3}([-*_])(?:\s*\1){2,}\s*$", raw):
            first_indent, next_indent = indents()
            lines.extend(ansi_spans(line, color) for line in wrap_verbatim(
                "─" * max(3, width - len(first_indent)), MD_RULE, width,
                first_indent, next_indent,
            ))
            first_content = False
            continue
        quote = re.match(r"^\s*>\s?(.*)$", raw)
        if quote:
            add_wrapped([(MD_QUOTE, "│ ")] + markdown_inline(quote.group(1), MD_QUOTE),
                        "", "  ")
            continue
        bullet = re.match(r"^(\s*)((?:[-+*])|(?:\d+[.)]))\s+(.+)$", raw)
        if bullet:
            padding, marker, body = bullet.groups()
            marker_text = padding + marker + " "
            add_wrapped([(MD_LIST, marker_text)] + markdown_inline(body, base_style),
                        "", " " * len(marker_text))
            continue
        add_wrapped(markdown_inline(raw, base_style))

    return lines or [ansi_spans([(base_style, role_prefix.rstrip())], color)]


def render_preview(session: Session, width: int, height: int, color: bool) -> list[str]:
    """Last few turns of `session`, wrapped to `width`, bottom-aligned to
    `height` lines like a chat scrollback — always returns exactly `height`
    lines so the caller can paste it next to the list unconditionally."""
    fn = PREVIEW.get(session.agent)
    if fn is None:
        body = [f"({session.agent}: no transcript preview — press p for the path)"]
    else:
        try:
            turns = fn(session)
        except Exception as exc:
            turns = None
            body = [f"(preview failed: {exc})"]
        if turns is not None:
            if not turns:
                body = ["(no preview available)"]
            else:
                blocks = []
                for role, text in turns:
                    blocks.append(render_markdown(text, max(10, width), role, color))
                body = []
                for wrapped in reversed(blocks):
                    body = wrapped + ([""] if body else []) + body
    if color:
        body = [f"\033[2;{CP_OVERLAY}m{ln}\033[0m" if not ln.startswith("\033") else ln
                for ln in body]
    body = body[-height:]
    return [""] * (height - len(body)) + body


# ------------------------------------------------------------------ plumbing


def repo_root(start: Path) -> Path:
    try:
        out = subprocess.run(
            ["git", "-C", str(start), "rev-parse", "--show-toplevel"],
            capture_output=True, text=True, timeout=5,
        )
        if out.returncode == 0 and out.stdout.strip():
            return Path(out.stdout.strip())
    except Exception:
        pass
    return start


def dir_matcher(root: str):
    """Match session-store directory names that mangle a path into one token.

    Claude Code and droid name their per-project directories after the cwd with
    every non-alphanumeric character replaced by '-', so compare positionally.
    """
    pat = "".join(re.escape(c) if c.isalnum() else "[^a-zA-Z0-9]" for c in root)
    rx = re.compile(pat + r"($|[^a-zA-Z0-9])")
    return lambda name: bool(rx.match(name))


def rel_age(ts: float, now: float) -> str:
    if ts <= 0:
        return "?"
    d = max(0, now - ts)
    for cut, div, unit in (
        (90, 1, "s"), (5400, 60, "m"), (172800, 3600, "h"),
        (1209600, 86400, "d"), (7776000, 604800, "w"),
    ):
        if d < cut:
            return f"{int(d / div)}{unit}"
    return f"{int(d / 2592000)}mo"


def human_size(n: int) -> str:
    if n <= 0:
        return "-"
    for unit, div in (("G", 1 << 30), ("M", 1 << 20), ("K", 1 << 10)):
        if n >= div:
            v = n / div
            return f"{v:.1f}{unit}" if v < 10 else f"{v:.0f}{unit}"
    return f"{n}B"


def folder_matcher(root: str, patterns: list[str], empty=False):
    """Build a `cwd -> bool` predicate shared by --except-folder and --only-folder.

    A pattern may be a subdirectory (`ws/r5`, `./ws/r5`), an absolute path, a
    glob (`ws/*`, `*-old`), a bare name matching any path component, or `.`
    for sessions started at the repo root itself. `empty` is the predicate's
    return value when no patterns were given.
    """
    base = root.rstrip("/")
    norm = []
    for raw in patterns:
        p = raw.strip().rstrip("/")
        if p.startswith("./"):
            p = p[2:]
        if p:
            norm.append(p)
    if not norm:
        return lambda cwd: empty

    def matches(cwd: str) -> bool:
        rel = cwd[len(base) + 1:] if cwd.startswith(base + "/") else ("" if cwd == base else cwd)
        parts = [p for p in rel.split("/") if p]
        for p in norm:
            if p == ".":
                if not parts:
                    return True
            elif p.startswith(("/", "~")):
                ap = os.path.expanduser(p).rstrip("/")
                if cwd == ap or cwd.startswith(ap + "/"):
                    return True
            elif (rel == p or rel.startswith(p + "/")
                  or fnmatch(rel, p) or fnmatch(rel, p + "/*")
                  or any(fnmatch(part, p) for part in parts)):
                return True
        return False

    return matches


def collect(root: str, want_all: bool, agents: list[str], skip_dir=None, only_dir=None) -> list[Session]:
    skip_dir = skip_dir or (lambda c: False)
    only_dir = only_dir or (lambda c: True)
    in_repo = (lambda c: True) if want_all else (
        lambda c: c == root or c.startswith(root.rstrip("/") + "/")
    )

    def keep(cwd):
        return in_repo(cwd) and not skip_dir(cwd) and only_dir(cwd)

    dir_ok = (lambda n: True) if want_all else dir_matcher(root)

    sessions: list[Session] = []
    for name in agents:
        if name in ("cursor", "gemini"):
            continue  # need candidate paths, run last
        try:
            sessions.extend(ADAPTERS[name](keep, dir_ok))
        except Exception as exc:  # a broken store must not kill the listing
            print(f"lc: {name}: {exc}", file=sys.stderr)

    # cursor and gemini key their stores by a hash of the cwd, so they can only
    # be looked up from candidate paths: the repo root, its immediate children,
    # and every cwd the other agents reported.
    cands = {root} | {s.cwd for s in sessions if s.cwd.startswith("/")}
    try:
        cands |= {str(p) for p in Path(root).iterdir() if p.is_dir()}
    except OSError:
        pass
    if want_all:
        cands |= {str(p) for p in iter_dirs(HOME)} | {str(HOME)}
    for name in ("cursor", "gemini"):
        if name in agents:
            try:
                sessions.extend(ADAPTERS[name](keep, dir_ok, sorted(cands)))
            except Exception as exc:
                print(f"lc: {name}: {exc}", file=sys.stderr)

    seen, out = set(), []
    for s in sorted(sessions, key=lambda s: s.ts, reverse=True):
        key = (s.agent, s.sid or s.path)
        if key in seen:
            continue
        seen.add(key)
        out.append(s)
    return out


def subdir_of(cwd: str, root: str) -> str:
    base = root.rstrip("/")
    if cwd == root:
        return "."
    if cwd.startswith(base + "/"):
        return "./" + cwd[len(base) + 1:]
    return (cwd or "?").replace(str(HOME), "~", 1)


class Table:
    """Column layout shared by the flat listing and the interactive browser."""

    HEADS = ["AGENT", "AGE", "SIZE", "ID", "DIR", "TITLE"]
    DIM = {1: CP_SUBTEXT, 2: CP_OVERLAY, 3: CP_OVERLAY, 4: CP_TEAL}

    def __init__(self, sessions, root, *, ids=False, sizes=True, show_age=True, width=None, color=True):
        now = time.time()
        self.sessions = sessions
        self.color = color
        show_dir = any(s.cwd != root for s in sessions)
        self.keep = [True, show_age, sizes, ids, show_dir, True]
        self.rows = [[s.agent, rel_age(s.ts, now), human_size(s.size), s.sid,
                      subdir_of(s.cwd, root) if show_dir else "", s.title or "(unnamed session)"]
                     for s in sessions]
        self.widths = [max([len(h)] + [len(r[i]) for r in self.rows]) if self.keep[i] else 0
                       for i, h in enumerate(self.HEADS)]
        total = width or term_size().columns
        fixed = sum(w + 2 for i, w in enumerate(self.widths[:-1]) if self.keep[i])
        self.widths[-1] = max(20, total - fixed - 1)

    def _cells(self, cells, pad_last=False):
        for i, cell in enumerate(cells):
            if not self.keep[i]:
                continue
            w = self.widths[i]
            if len(cell) > w:
                cell = cell[: w - 1] + "…"
            last = not any(self.keep[j] for j in range(i + 1, len(cells)))
            text = cell.ljust(w) if (not last or pad_last) else cell
            yield i, cell, text

    @property
    def total_width(self) -> int:
        return sum(w + 2 for i, w in enumerate(self.widths) if self.keep[i]) - 2

    def header(self, pad_last=False) -> str:
        line = "  ".join(text for _, _, text in self._cells(self.HEADS, pad_last)).rstrip()
        return f"\033[1;{CP_LAVENDER}m{line}\033[0m" if self.color else line

    def line(self, idx: int, selected: bool = False, pad_last: bool = False) -> str:
        agent = self.sessions[idx].agent
        parts = []
        for i, cell, text in self._cells(self.rows[idx], pad_last or selected):
            if self.color and not selected:
                code = COLORS.get(agent, CP_TEXT) if i == 0 else self.DIM.get(i, CP_TEXT)
                if code and cell.strip():
                    text = f"\033[{code}m{text}\033[0m"
            parts.append(text)
        line = "  ".join(parts)
        if not pad_last and not selected:
            line = line.rstrip()
        if selected:
            return f"\033[{CP_SELECTED}m{line}\033[0m" if self.color else f"> {line}"
        return line


def render(sessions, root, args):
    table = Table(sessions, root, ids=args.ids, sizes=not args.no_size,
                  color=sys.stdout.isatty() and not os.environ.get("NO_COLOR"))
    print(table.header())
    for i in range(len(sessions)):
        print(table.line(i))


# ---------------------------------------------------------------- interactive

# How each agent reopens a session; {id} is the session id. Gemini resumes by
# index rather than id, so it has no entry and Enter falls back to the path.
RESUME = {
    "claude": ["claude", "--resume", "{id}"],
    "codex": ["codex", "resume", "{id}"],
    "droid": ["droid", "--resume", "{id}"],
    "opencode": ["opencode", "--session", "{id}"],
    "cursor": ["cursor-agent", "--resume", "{id}"],
    "copilot": ["copilot", "--resume", "{id}"],
    "grok": ["grok", "--resume", "{id}"],
    "kimi": ["kimi", "--session", "{id}"],
}

HINTS = ("j/k gg/G ^d/^u move · drag │ resize panes · [ and ] resize keys · / filter · "
         "h hide dir · u undo · H unhide · i ids · enter resume · p path · q quit")


def read_key(fh) -> str | tuple[str, int, int, int, str]:
    """Block for one input event, including SGR mouse reports when enabled.

    The mouse tuple is ``("mouse", code, column, row, phase)``.  SGR mouse
    reports are deliberately decoded here instead of treating their bytes as
    keystrokes: otherwise dragging the splitter leaks ``[<...`` into the
    normal key handler.
    """
    import select

    ch = fh.read(1)
    if not ch:
        return "q"
    if ch != b"\x1b":
        return ch.decode("utf-8", "replace")
    # escape: either a lone Esc or a CSI sequence already in the buffer
    if not select.select([fh], [], [], 0.05)[0]:
        return "esc"
    if fh.read(1) != b"[":
        return ""
    if not select.select([fh], [], [], 0.05)[0]:
        return ""
    first = fh.read(1)
    if first == b"<":  # xterm SGR: ESC [ < button ; column ; row M/m
        payload = bytearray()
        while len(payload) < 32:
            if not select.select([fh], [], [], 0.05)[0]:
                return ""
            part = fh.read(1)
            if part in (b"M", b"m"):
                try:
                    code, col, row = (int(v) for v in payload.decode().split(";"))
                except ValueError:
                    return ""
                return "mouse", code, col, row, "release" if part == b"m" else "press"
            payload.extend(part)
        return ""

    seq = bytearray(b"[" + first)
    # CSI final bytes live in the 0x40-0x7e range. Finish every CSI sequence
    # even when we do not bind it, so unsupported keys never leave tail bytes
    # to be misread as ordinary input on the next pass.
    while not (b"@" <= seq[-1:] <= b"~") and len(seq) < 8:
        if not select.select([fh], [], [], 0.05)[0]:
            return ""
        seq.extend(fh.read(1))
    return {b"[A": "k", b"[B": "j", b"[5~": "^b", b"[6~": "^f",
            b"[H": "g", b"[F": "G", b"[D": "[", b"[C": "]"}.get(bytes(seq), "")


def browse(sessions, root, args):
    """Scrollable session list with a mouse-draggable preview splitter."""
    try:
        tty_in = open("/dev/tty", "rb", buffering=0)
    except OSError:
        return "plain", None, []
    import termios
    import tty as tty_mod

    fd = tty_in.fileno()
    saved = termios.tcgetattr(fd)
    out = sys.stdout
    ids, cur, top, query, typing = args.ids, 0, 0, "", False
    hidden: list[str] = []
    action, chosen = "quit", None

    def buried(cwd):
        # hiding a directory hides its subtree, except at the repo root, where
        # that would hide every session
        return any(cwd == d or (d != root and cwd.startswith(d.rstrip("/") + "/"))
                   for d in hidden)

    def refresh():
        q = query.lower()
        return [s for s in sessions if not buried(s.cwd)
                and (not q or q in f"{s.agent} {subdir_of(s.cwd, root)} {s.title}".lower())]

    view = refresh()
    color = not os.environ.get("NO_COLOR")
    preview_key, preview_lines = None, []
    MIN_SPLIT_COLS = 90  # below this, the preview pane would be too cramped to help
    MIN_DIR_COLS = 18
    MIN_TITLE_COLS = 30
    MIN_PREVIEW_COLS = 20
    SPLITTER_COLS = 3  # one pad column on either side of the visible divider
    preferred_dir_width = preferred_title_width = None
    dragging_splitter = None

    def clamp_dir_width(width, cols, title_width):
        return max(MIN_DIR_COLS, min(width, cols - 2 * SPLITTER_COLS - title_width - MIN_PREVIEW_COLS))

    def clamp_title_width(width, cols, dir_width):
        return max(MIN_TITLE_COLS, min(width, cols - 2 * SPLITTER_COLS - dir_width - MIN_PREVIEW_COLS))

    def pane_text(text, width):
        return text if len(text) <= width else text[: max(0, width - 1)] + "…"

    def pane_line(text, width, selected=False, style=CP_TEXT):
        text = pane_text(text, width).ljust(width)
        if not color:
            if selected:
                return "> " + pane_text(text.rstrip(), max(0, width - 2)).ljust(max(0, width - 2))
            return text
        code = CP_SELECTED if selected else style
        return f"\033[{code}m{text}\033[0m"

    def relayout():
        cols, rows = term_size(fd)
        split = cols >= MIN_SPLIT_COLS and bool(view)
        if split:
            default_dir = max(MIN_DIR_COLS, cols // 5)
            dir_width = clamp_dir_width(preferred_dir_width or default_dir, cols, MIN_TITLE_COLS)
            default_title = max(MIN_TITLE_COLS, (cols - dir_width - 2 * SPLITTER_COLS) // 2)
            title_width = clamp_title_width(preferred_title_width or default_title, cols, dir_width)
            prev_width = cols - dir_width - title_width - 2 * SPLITTER_COLS
        else:
            dir_width = title_width = 0
            prev_width = 0
        # The selected session's age and size live in the bottom-right status
        # area, leaving this dense browsing list almost entirely for names.
        table = Table(view, root, ids=ids, sizes=False, show_age=False, width=cols)
        return table, rows, cols, split, dir_width, title_width, prev_width

    try:
        tty_mod.setraw(fd)
        # Button-event tracking only reports mouse motion while a button is
        # held, which is exactly what a draggable splitter needs. SGR encoding
        # gives us unambiguous, unlimited terminal coordinates.
        out.write("\033[?1049h\033[?25l\033[?1000h\033[?1002h\033[?1006h")
        while True:
            table, height, cols, split, dir_width, title_width, prev_width = relayout()
            page = max(1, height - 3)
            cur = min(max(cur, 0), max(0, len(view) - 1))
            top = min(max(top, cur - page + 1), cur)
            top = max(0, min(top, max(0, len(view) - page)))

            eol = "\033[K\r\n"  # erase-to-end-of-line: no bleed-through from a longer prior frame
            if split:
                key = (view[cur].agent, view[cur].sid, view[cur].path, prev_width, page)
                if key != preview_key:
                    preview_lines = render_preview(view[cur], prev_width, page, color)
                    preview_key = key
                def bar(which):
                    divider = "║" if dragging_splitter == which else "│"
                    style = CP_LAVENDER if dragging_splitter == which else CP_OVERLAY
                    return f"\033[{style}m{divider}\033[0m " if color else f"{divider} "
                bar_dir, bar_title = bar("dir"), bar("title")
                preview_name = view[cur].title or "(unnamed session)"
                name_width = max(0, prev_width - len("PREVIEW · "))
                if len(preview_name) > name_width:
                    preview_name = preview_name[: max(0, name_width - 1)] + ("…" if name_width else "")
                if color:
                    header_r = (f"\033[1;{CP_LAVENDER}mPREVIEW\033[0m · "
                                f"\033[{CP_TEXT}m{preview_name}\033[0m")
                else:
                    header_r = f"PREVIEW · {preview_name}"
                header_dir = pane_line("AGENT / DIR", dir_width, style=CP_LAVENDER)
                header_title = pane_line("TITLE", title_width, style=CP_LAVENDER)
                frame = ["\033[H\033[2J",
                         clip(header_dir + " " + bar_dir + header_title + " " + bar_title + header_r, cols - 1),
                         eol]
                for row, i in enumerate(range(top, min(top + page, len(view)))):
                    session, selected = view[i], i == cur
                    directory = subdir_of(session.cwd, root)
                    context = (
                        session.agent
                        if directory == "."
                        else f"{session.agent}  {directory}"
                    )
                    left = pane_line(context, dir_width, selected, COLORS.get(session.agent, CP_TEXT))
                    title = pane_line(session.title or "(unnamed session)", title_width, selected)
                    frame.append(clip(left + " " + bar_dir + title + " " + bar_title + preview_lines[row], cols - 1) + eol)
                for row in range(min(top + page, len(view)) - top, page):
                    frame.append(clip(" " * dir_width + " " + bar_dir + " " * title_width + " " + bar_title + preview_lines[row], cols - 1) + eol)
            else:
                preview_key = None
                frame = ["\033[H\033[2J", clip(table.header(), cols - 1), eol]
                for i in range(top, min(top + page, len(view))):
                    frame.append(clip(table.line(i, selected=(i == cur)), cols - 1) + eol)
            if not view:
                frame.append(f"\033[2;{CP_OVERLAY}m(nothing matches)\033[0m" + eol)
            status = f"/{query}" if typing else f"{cur + 1}/{len(view)}"
            if not typing and query:
                status += f" f:{query}"
            if not typing and hidden:
                status += f" hid:{len(hidden)}"
            facts = []
            if view:
                facts.append(rel_age(view[cur].ts, time.time()))
                if not args.no_size:
                    facts.append(human_size(view[cur].size))
            facts = " · ".join(facts)
            hint_width = max(0, cols - 24 - len(facts) - (1 if facts else 0))
            status_line = clip(
                f"\033[{height};1H\033[2;{CP_SUBTEXT}m{status[:22].ljust(22)}"
                f"{HINTS[:hint_width]}\033[0m",
                cols - 1,
            )
            if facts:
                facts_col = max(1, cols - len(facts))
                status_line += f"\033[{height};{facts_col}H\033[{CP_FACTS}m{facts}\033[0m"
            frame.append(status_line + "\033[K")
            out.write("".join(frame))
            out.flush()

            key = read_key(tty_in)
            if isinstance(key, tuple):
                _, mouse_code, mouse_col, _, mouse_phase = key
                mouse_button = mouse_code & 3
                mouse_motion = bool(mouse_code & 32)
                mouse_wheel = bool(mouse_code & 64)
                # The divider itself is a single cell, but accepting its two
                # padding cells makes it practical to grab in a terminal.
                on_dir = split and dir_width + 1 <= mouse_col <= dir_width + SPLITTER_COLS
                title_bar_col = dir_width + SPLITTER_COLS + title_width
                on_title = split and title_bar_col + 1 <= mouse_col <= title_bar_col + SPLITTER_COLS
                if mouse_phase == "release":
                    dragging_splitter = None
                elif not mouse_wheel:
                    if not mouse_motion and mouse_button == 0:
                        dragging_splitter = "dir" if on_dir else ("title" if on_title else None)
                    if dragging_splitter and (mouse_motion or mouse_button == 0):
                        if dragging_splitter == "dir":
                            preferred_dir_width = clamp_dir_width(mouse_col - 2, cols, title_width)
                        else:
                            preferred_title_width = clamp_title_width(
                                mouse_col - title_bar_col - 2 + title_width, cols, dir_width,
                            )
                        preview_key = None
                continue
            if typing:  # incremental filter
                if key in ("\r", "\n", "esc"):
                    typing = False
                    if key == "esc":
                        query, cur = "", 0
                elif key in ("\x7f", "\b"):
                    query = query[:-1]
                elif key == "\x03":
                    break
                elif key and key.isprintable():
                    query += key
                if typing or key not in ("\r", "\n"):
                    view = refresh()
                    cur = top = 0
                continue

            if key in ("q", "esc", "\x03"):
                break
            elif key == "j":
                cur += 1
            elif key == "k":
                cur -= 1
            elif key == "G":
                cur = len(view) - 1
            elif key == "g":
                cur = 0 if read_key(tty_in) == "g" else cur
            elif key == "\x04":  # ctrl-d
                cur += page // 2
            elif key == "\x15":  # ctrl-u
                cur -= page // 2
            elif key in ("\x06", "^f", " "):
                cur += page
            elif key in ("\x02", "^b"):
                cur -= page
            elif key == "i":
                ids = not ids
            elif key in ("[", "]") and split:
                # Keyboard fallback for terminals that do not expose mouse
                # tracking (and useful for precise adjustments).
                preferred_title_width = clamp_title_width(
                    title_width + (-4 if key == "[" else 4), cols, dir_width,
                )
                preview_key = None
            elif key == "/":
                typing, query = True, ""
            elif key == "h" and view:  # hide every session from this directory
                if view[cur].cwd not in hidden:
                    hidden.append(view[cur].cwd)
                    view = refresh()
            elif key == "u" and hidden:  # undo the last hide
                hidden.pop()
                view = refresh()
            elif key == "H" and hidden:  # bring everything back
                hidden.clear()
                view = refresh()
            elif key in ("\r", "\n", "p") and view:
                action, chosen = ("resume" if key != "p" else "path"), view[cur]
                break
    finally:
        out.write("\033[?1006l\033[?1002l\033[?1000l\033[?25h\033[?1049l")
        out.flush()
        termios.tcsetattr(fd, termios.TCSADRAIN, saved)
        tty_in.close()
    return action, chosen, hidden


def resume(s: Session) -> int:
    """Hand the terminal over to the agent that owns this session."""
    template = RESUME.get(s.agent)
    argv = [part.format(id=s.sid) for part in template] if template else None
    if not argv or not shutil.which(argv[0]):
        why = "no resume-by-id support" if not argv else f"{argv[0]} not on PATH"
        print(f"{s.path}\n\033[2;{CP_SUBTEXT}m({s.agent}: {why})\033[0m")
        return 0
    try:
        os.chdir(s.cwd)  # agents scope sessions by working directory
    except OSError:
        print(f"lc: {s.cwd} is gone; resuming from {os.getcwd()}", file=sys.stderr)
    print(f"\033[2;{CP_SUBTEXT}m$ {' '.join(argv)}\033[0m")
    os.execvp(argv[0], argv)
    return 127


def main() -> int:
    ap = argparse.ArgumentParser(
        prog="lc",
        description="List coding-agent sessions for the current repository or a supplied directory.",
    )
    ap.add_argument("targets_pos", nargs="*", default=[], metavar="AGENT|PATH",
                    help="agent filters and/or one existing directory, e.g. `lc ~/ codex`")
    ap.add_argument("-p", "--path", default=None, help="use this path instead of the cwd")
    ap.add_argument("-a", "--all", action="store_true", help="every repository, not just this one")
    ap.add_argument("-n", "--limit", type=int, default=40, help="max rows (0 = all, default 40)")
    ap.add_argument("--agent", action="append", default=[],
                    help="only these agents (repeatable, comma-separated, "
                         "or just list them positionally: `lc codex claude`)")
    ap.add_argument("-X", "--except-agent", "--except-agents", action="append", default=[],
                    dest="except_agent", metavar="AGENT",
                    help="skip these agents (repeatable, comma-separated)")
    ap.add_argument("-x", "--except-folder", "--except-folders", action="append", default=[],
                    dest="except_folder", metavar="DIR",
                    help="skip sessions from these folders: subdir, absolute path, "
                         "glob, or bare directory name (repeatable, comma-separated)")
    ap.add_argument("-o", "--only-folder", "--only", action="append", default=[],
                    dest="only_folder", metavar="DIR",
                    help="show only sessions from these folders (same pattern rules "
                         "as --except-folder; repeatable, comma-separated)")
    ap.add_argument("-i", "--ids", action="store_true", help="show session ids")
    ap.add_argument("--no-size", action="store_true", help="hide the transcript size column")
    ap.add_argument("-I", "--interactive", action="store_true",
                    help="browse with vim keys (j/k, gg/G, ^d/^u, /, enter resumes)")
    ap.add_argument("--json", action="store_true", help="machine-readable output")
    args = ap.parse_args()

    def split(specs):
        out = []
        for spec in specs:
            out.extend(x.strip() for x in spec.split(",") if x.strip())
        return out

    positional_agents, positional_paths = [], []
    for target in args.targets_pos:
        # A known agent always remains an agent filter. Every other existing
        # directory is a positional scope, making `lc -I ~/` natural while
        # retaining `lc codex claude` unchanged.
        if target in ADAPTERS:
            positional_agents.append(target)
        elif Path(target).expanduser().is_dir():
            positional_paths.append(target)
        else:
            positional_agents.extend(split([target]))
    if len(positional_paths) > 1:
        print(f"lc: expected at most one positional path, got: {', '.join(positional_paths)}",
              file=sys.stderr)
        return 2
    if args.path is not None and positional_paths:
        print("lc: use either -p/--path or a positional path, not both", file=sys.stderr)
        return 2

    start = Path(args.path or (positional_paths[0] if positional_paths else ".")).expanduser().resolve()
    root = str(repo_root(start))

    agents, skipped = split(args.agent) + positional_agents, split(args.except_agent)
    unknown = [a for a in agents + skipped if a not in ADAPTERS]
    if unknown:
        print(f"lc: unknown agent(s): {', '.join(unknown)}\n"
              f"    known: {', '.join(ADAPTERS)}", file=sys.stderr)
        return 2
    agents = [a for a in (agents or list(ADAPTERS)) if a not in skipped]
    if not agents:
        print("lc: every agent was excluded", file=sys.stderr)
        return 2

    sessions = collect(root, args.all, agents,
                       folder_matcher(root, split(args.except_folder)),
                       folder_matcher(root, split(args.only_folder), empty=True))
    if args.limit > 0:
        shown = sessions[: args.limit]
    else:
        shown = sessions

    if args.json:
        json.dump([
            {"agent": s.agent, "id": s.sid, "cwd": s.cwd, "name": s.title, "title": s.title,
             "updated": s.ts, "path": s.path, "bytes": s.size, "note": s.note}
            for s in shown
        ], sys.stdout, indent=2)
        print()
        return 0

    if not sessions:
        where = "anywhere" if args.all else root.replace(str(HOME), "~", 1)
        print(f"no coding sessions found for {where}")
        return 0

    if args.interactive and sys.stdout.isatty():
        action, picked, hidden = browse(sessions, root, args)
        if hidden and action in ("quit", "path"):
            dirs = ",".join(subdir_of(d, root) for d in hidden)
            print(f"\033[2;{CP_SUBTEXT}mto keep those hidden: lc -x {dirs}\033[0m", file=sys.stderr)
        if action == "resume":
            return resume(picked)
        if action == "path":
            print(picked.path)
            return 0
        if action == "quit":
            return 0

    render(shown, root, args)
    hidden = len(sessions) - len(shown)
    if sys.stdout.isatty():
        tail = f"{len(sessions)} sessions"
        if not args.all:
            tail += f" in {root.replace(str(HOME), '~', 1)}"
        if hidden > 0:
            tail += f" ({hidden} more, use -n 0)"
        print(f"\033[2;{CP_SUBTEXT}m{tail}\033[0m")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (BrokenPipeError, KeyboardInterrupt):
        sys.exit(130)
