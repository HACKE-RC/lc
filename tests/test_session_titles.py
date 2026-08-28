import json
import sys
import tempfile
import unittest
from pathlib import Path
from urllib.parse import quote

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))
from lc.cache import CodexCache, CodexCacheEntry
from lc import cli as lc


def jsonl(path, entries):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps(entry) + "\n" for entry in entries))


class AdapterTitleFallbackTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.home = Path(self.temp.name)
        self.cwd = "/work/repo"
        self.original_home, self.original_cache = lc.HOME, lc.CODEX_CACHE
        lc.HOME = self.home
        lc.CODEX_CACHE = self.home / "cache.json"

    def tearDown(self):
        lc.HOME, lc.CODEX_CACHE = self.original_home, self.original_cache
        self.temp.cleanup()

    def sessions(self, adapter, *extra):
        return list(adapter(lambda cwd: cwd == self.cwd, lambda _: True, *extra))

    def test_droid_uses_user_prompt_when_session_title_is_generic(self):
        jsonl(self.home / ".factory/sessions/project/one.jsonl", [
            {"type": "session_start", "cwd": self.cwd, "title": "New session"},
            {"type": "message", "message": {"role": "user", "content": "Fix Droid titles"}},
        ])
        self.assertEqual(self.sessions(lc.a_droid)[0].title, "Fix Droid titles")

    def test_grok_uses_prompt_history_when_summary_has_no_title(self):
        workspace = self.home / ".grok/sessions" / quote(self.cwd, safe="")
        jsonl(workspace / "prompt_history.jsonl", [{"session_id": "one", "prompt": "Grok fallback"}])
        (workspace / "one").mkdir(parents=True)
        (workspace / "one/summary.json").write_text(json.dumps({"info": {"id": "one"}}))
        self.assertEqual(self.sessions(lc.a_grok)[0].title, "Grok fallback")

    def test_kimi_uses_last_prompt_when_title_is_absent(self):
        session = self.home / "kimi-session"
        session.mkdir()
        (session / "state.json").write_text(json.dumps({"lastPrompt": "Kimi fallback"}))
        jsonl(self.home / ".kimi-code/session_index.jsonl", [{
            "workDir": self.cwd, "sessionDir": str(session), "sessionId": "one",
        }])
        self.assertEqual(self.sessions(lc.a_kimi)[0].title, "Kimi fallback")

    def test_gemini_uses_first_user_message_when_display_name_is_absent(self):
        chat = self.home / ".gemini/tmp/project/chats/one.json"
        chat.parent.mkdir(parents=True)
        (chat.parent.parent / ".project_root").write_text(self.cwd)
        chat.write_text(json.dumps({"sessionId": "one", "messages": [
            {"type": "user", "content": "Gemini fallback"},
            {"type": "gemini", "content": "Acknowledged"},
        ]}))
        self.assertEqual(self.sessions(lc.a_gemini)[0].title, "Gemini fallback")

    def test_titles_are_bounded_labels_not_transcript_exports(self):
        self.assertEqual(len(lc.clean_title("x" * 10_000)), lc.TITLE_DISPLAY_LIMIT)


class CodexCacheTests(unittest.TestCase):
    def test_round_trip_uses_named_entry_contract(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            rollout = root / "rollout.jsonl"
            rollout.write_text("{}")
            stat = rollout.stat()
            entry = CodexCacheEntry(stat.st_mtime_ns, stat.st_size, "/repo", "id", "Title", True)
            cache_path = root / "cache/sessions.json"
            cache = CodexCache(cache_path)
            cache.remember(str(rollout), entry)
            cache.flush()
            self.assertEqual(CodexCache(cache_path).get(str(rollout), stat), entry)

    def test_malformed_cache_is_ignored(self):
        with tempfile.TemporaryDirectory() as temp:
            cache_path = Path(temp) / "sessions.json"
            cache_path.write_text('{"version": 1, "sessions": {"bad": []}}')
            self.assertIsNone(CodexCache(cache_path).get("bad", Path(temp).stat()))


if __name__ == "__main__":
    unittest.main()
