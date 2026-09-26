package sessions

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const repo = "/work/repo"

func testEnv(t *testing.T) *Env {
	t.Helper()
	home := t.TempDir()
	return &Env{Home: home, CacheDir: filepath.Join(home, "cache"), PiAgentDir: filepath.Join(home, ".pi/agent")}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jsonl(t *testing.T, path string, entries ...any) {
	t.Helper()
	var b strings.Builder
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	writeFile(t, path, b.String())
}

func run(env *Env, fn adapter) []Session {
	var out []Session
	fn(env, func(cwd string) bool { return cwd == repo }, func(string) bool { return true }, func(s Session) { out = append(out, s) })
	return out
}

func titles(ss []Session) map[string]string {
	m := map[string]string{}
	for _, s := range ss {
		m[s.ID] = s.Title
	}
	return m
}

type M = map[string]any

func TestDroidUsesUserPromptWhenSessionTitleIsGeneric(t *testing.T) {
	env := testEnv(t)
	jsonl(t, filepath.Join(env.Home, ".factory/sessions/project/one.jsonl"),
		M{"type": "session_start", "cwd": repo, "title": "New session"},
		M{"type": "message", "message": M{"role": "user", "content": "Fix Droid titles"}})
	if got := run(env, aDroid); len(got) != 1 || got[0].Title != "Fix Droid titles" {
		t.Fatalf("got %+v", got)
	}
}

func TestGrokUsesPromptHistoryWhenSummaryHasNoTitle(t *testing.T) {
	env := testEnv(t)
	ws := filepath.Join(env.Home, ".grok/sessions", url.PathEscape(repo))
	jsonl(t, filepath.Join(ws, "prompt_history.jsonl"), M{"session_id": "one", "prompt": "Grok fallback"})
	writeFile(t, filepath.Join(ws, "one/summary.json"), `{"info": {"id": "one"}}`)
	if got := run(env, aGrok); len(got) != 1 || got[0].Title != "Grok fallback" || got[0].Cwd != repo {
		t.Fatalf("got %+v", got)
	}
}

func TestKimiUsesLastPromptWhenTitleIsAbsent(t *testing.T) {
	env := testEnv(t)
	sdir := filepath.Join(env.Home, "kimi-session")
	writeFile(t, filepath.Join(sdir, "state.json"), `{"lastPrompt": "Kimi fallback"}`)
	jsonl(t, filepath.Join(env.Home, ".kimi-code/session_index.jsonl"),
		M{"workDir": repo, "sessionDir": sdir, "sessionId": "one"})
	if got := run(env, aKimi); len(got) != 1 || got[0].Title != "Kimi fallback" {
		t.Fatalf("got %+v", got)
	}
}

func TestGeminiUsesFirstUserMessageWhenDisplayNameIsAbsent(t *testing.T) {
	env := testEnv(t)
	project := filepath.Join(env.Home, ".gemini/tmp/project")
	writeFile(t, filepath.Join(project, ".project_root"), repo)
	writeFile(t, filepath.Join(project, "chats/one.json"), `{"sessionId": "one", "messages": [
		{"type": "user", "content": "Gemini\n\n  fallback"},
		{"type": "gemini", "content": "Acknowledged"}]}`)
	var got []Session
	aGemini(env, func(cwd string) bool { return cwd == repo }, nil, func(s Session) { got = append(got, s) })
	if len(got) != 1 || got[0].Title != "Gemini fallback" {
		t.Fatalf("got %+v", got)
	}
	turns, ok, err := Preview(env, got[0])
	want := []Turn{{"user", "Gemini\n\n  fallback"}, {"assistant", "Acknowledged"}}
	if !ok || err != nil || !reflect.DeepEqual(turns, want) {
		t.Fatalf("preview %q %v %v", turns, ok, err)
	}
}

func TestPiPrefersSessionNameAndFallsBackToFirstUserPrompt(t *testing.T) {
	env := testEnv(t)
	dir := filepath.Join(env.PiAgentDir, "sessions/--work-repo--")
	jsonl(t, filepath.Join(dir, "named.jsonl"),
		M{"type": "session", "version": 3, "id": "one", "cwd": repo},
		M{"type": "message", "message": M{"role": "user", "content": "Pi fallback"}},
		M{"type": "session_info", "name": "Named pi session"})
	jsonl(t, filepath.Join(dir, "unnamed.jsonl"),
		M{"type": "session", "version": 3, "id": "two", "cwd": repo},
		M{"type": "message", "message": M{"role": "user", "content": []any{M{"type": "text", "text": "Pi fallback"}}}})
	want := map[string]string{"one": "Named pi session", "two": "Pi fallback"}
	if got := titles(run(env, aPi)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestPiPreviewFollowsTheActiveBranch(t *testing.T) {
	env := testEnv(t)
	jsonl(t, filepath.Join(env.PiAgentDir, "sessions/--work-repo--/one.jsonl"),
		M{"type": "session", "version": 3, "id": "one", "cwd": repo},
		M{"type": "message", "id": "a", "parentId": nil, "message": M{"role": "user", "content": "First"}},
		M{"type": "message", "id": "b", "parentId": "a", "message": M{"role": "assistant", "content": []any{
			M{"type": "thinking", "thinking": "hidden"}, M{"type": "text", "text": "Old branch"}}}},
		M{"type": "message", "id": "c", "parentId": "a", "message": M{"role": "user", "content": "Active branch"}})
	got := run(env, aPi)
	if len(got) != 1 {
		t.Fatalf("sessions %+v", got)
	}
	turns, ok, err := Preview(env, got[0])
	want := []Turn{{"user", "First"}, {"user", "Active branch"}}
	if !ok || err != nil || !reflect.DeepEqual(turns, want) {
		t.Fatalf("preview %q %v %v", turns, ok, err)
	}
}

func TestOmpTitleSlotThenHeaderThenUserPrompt(t *testing.T) {
	env := testEnv(t)
	base := filepath.Join(env.Home, ".omp/agent/sessions/-repo")
	prompt := M{"type": "message", "message": M{"role": "user", "content": []any{M{"type": "text", "text": "Omp fallback"}}}}
	reminder := M{"type": "message", "message": M{"role": "developer", "content": "Stop early"}}
	omp := func(path, sid string, slot *string, headerTitle string, entries ...any) {
		header := M{"type": "session", "version": 3, "id": sid, "cwd": repo}
		if headerTitle != "" {
			header["title"] = headerTitle
		}
		var lines []any
		if slot != nil {
			lines = append(lines, M{"type": "title", "v": 1, "title": *slot, "pad": strings.Repeat(" ", 64)})
		}
		jsonl(t, filepath.Join(base, path), append(append(lines, header), entries...)...)
	}
	renamed, blank, sub := "Renamed", "", "Subagent"
	omp("slot.jsonl", "slot", &renamed, "Old", prompt)
	omp("legacy.jsonl", "legacy", nil, "Legacy title", prompt)
	omp("blank.jsonl", "blank", &blank, "", reminder, prompt)
	// Subagent transcripts sit under the parent session's directory.
	omp("slot/Worker.jsonl", "worker", &sub, "", prompt)
	want := map[string]string{"slot": "Renamed", "legacy": "Legacy title", "blank": "Omp fallback"}
	if got := titles(run(env, aOmp)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestTitlesAreBoundedLabelsNotTranscriptExports(t *testing.T) {
	title, ok := CleanTitle(strings.Repeat("é", 10_000))
	if !ok || len([]rune(title)) != TitleDisplayLimit {
		t.Fatalf("got %d runes, ok=%v", len([]rune(title)), ok)
	}
}

func TestCodexCacheRoundTrip(t *testing.T) {
	root := t.TempDir()
	rollout := filepath.Join(root, "rollout.jsonl")
	writeFile(t, rollout, "{}")
	fi, _ := os.Stat(rollout)
	title := "Tïtle \"q\" 😀"
	entry := codexEntry{fi.ModTime().UnixNano(), fi.Size(), "/repo", "id", &title, true}
	path := filepath.Join(root, "cache/sessions.json")
	c := newCodexCache(path)
	c.remember(rollout, entry, false)
	c.flush()
	got, ok := newCodexCache(path).get(rollout, fi)
	if !ok || !reflect.DeepEqual(got, entry) {
		t.Fatalf("got %+v %v", got, ok)
	}
	if _, err := os.Stat(filepath.Join(root, "cache/sessions.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind: %v", err)
	}
}

func TestMalformedCodexCacheIsIgnored(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions.json")
	for _, content := range []string{
		`{"version": 2, "sessions": {"bad": []}}`,
		`{"version": 2, "sessions": {"bad": {"mtime_ns": 1.5, "size": 1, "cwd": "/", "session_id": "x", "title_scanned": true}}}`,
		`{"version": 2, "sessions": {}}`,
		`not json`,
	} {
		writeFile(t, path, content)
		fi, _ := os.Stat(root)
		if _, ok := newCodexCache(path).get("bad", fi); ok {
			t.Fatalf("accepted %s", content)
		}
	}
}

// Version 1 caches hold titles scanned before item_completed/goal/subagent
// support; even a matching entry must be rescanned.
func TestOldCodexCacheVersionIsIgnored(t *testing.T) {
	root := t.TempDir()
	rollout := filepath.Join(root, "rollout.jsonl")
	writeFile(t, rollout, "{}")
	fi, _ := os.Stat(rollout)
	path := filepath.Join(root, "sessions.json")
	entry := fmt.Sprintf(`{"mtime_ns": %d, "size": %d, "cwd": "/repo", "session_id": "x", "title": null, "title_scanned": true}`,
		fi.ModTime().UnixNano(), fi.Size())
	writeFile(t, path, fmt.Sprintf(`{"version": 2, "sessions": {%q: %s}}`, rollout, entry))
	if _, ok := newCodexCache(path).get(rollout, fi); !ok {
		t.Fatal("current-version entry not accepted")
	}
	writeFile(t, path, fmt.Sprintf(`{"version": 1, "sessions": {%q: %s}}`, rollout, entry))
	if _, ok := newCodexCache(path).get(rollout, fi); ok {
		t.Fatal("version 1 entry accepted")
	}
}

// A cache hit must stand in for reading the rollout: the cached title wins
// even when the file itself says otherwise.
func TestCodexUsesCacheForUnchangedRollouts(t *testing.T) {
	env := testEnv(t)
	f := filepath.Join(env.Home, ".codex/sessions/2025/01/02/rollout-2025-01-02T10-11-12-abc.jsonl")
	jsonl(t, f,
		M{"type": "session_meta", "payload": M{"cwd": repo}},
		M{"type": "event_msg", "payload": M{"type": "user_message", "message": "From file"}})
	if got := run(env, aCodex); len(got) != 1 || got[0].ID != "abc" || got[0].Title != "From file" {
		t.Fatalf("first run %+v", got)
	}
	fi, _ := os.Stat(f)
	c := newCodexCache(filepath.Join(env.CacheDir, "codex-sessions.json"))
	e, ok := c.get(f, fi)
	if !ok || !e.TitleScanned || e.Cwd != repo {
		t.Fatalf("cache entry %+v %v", e, ok)
	}
	cached := "From cache"
	e.Title = &cached
	c.remember(f, e, true)
	c.stored = nil // force the write
	c.flush()
	if got := run(env, aCodex); len(got) != 1 || got[0].Title != "From cache" {
		t.Fatalf("second run %+v", got)
	}
}

// Codex 0.150+ writes chat turns only as item_completed events.
func codexItemEvent(kind, blockType, text string) M {
	return M{"type": "event_msg", "payload": M{"type": "item_completed", "item": M{
		"type": kind, "content": []any{M{"type": blockType, "text": text}},
	}}}
}

func TestCodexItemCompletedRollouts(t *testing.T) {
	env := testEnv(t)
	f := filepath.Join(env.Home, ".codex/sessions/2026/09/23/rollout-2026-09-23T22-16-44-new.jsonl")
	jsonl(t, f,
		M{"type": "session_meta", "payload": M{"cwd": repo}},
		M{"type": "response_item", "payload": M{"type": "message", "role": "user", "content": []any{
			M{"type": "input_text", "text": "<environment_context>\n<cwd>/work/repo</cwd>\n</environment_context>"}}}},
		codexItemEvent("UserMessage", "text", "Fix the **parser**"),
		codexItemEvent("Reasoning", "Text", "hidden"),
		codexItemEvent("AgentMessage", "Text", "Done:\n\n- one\n- two"))
	got := run(env, aCodex)
	if len(got) != 1 || got[0].Title != "Fix the **parser**" {
		t.Fatalf("sessions %+v", got)
	}
	want := []Turn{{"user", "Fix the **parser**"}, {"assistant", "Done:\n\n- one\n- two"}}
	if turns := prevCodex(env, got[0]); !reflect.DeepEqual(turns, want) {
		t.Fatalf("preview %q, want %q", turns, want)
	}
}

func TestCodexPreviewPrefersLegacyEventsInMixedRollouts(t *testing.T) {
	env := testEnv(t)
	f := filepath.Join(env.Home, "mixed.jsonl")
	jsonl(t, f,
		M{"type": "event_msg", "payload": M{"type": "user_message", "message": "Hello"}},
		codexItemEvent("UserMessage", "text", "Hello"),
		M{"type": "event_msg", "payload": M{"type": "agent_message", "message": "Hi"}},
		codexItemEvent("AgentMessage", "Text", "Hi"))
	want := []Turn{{"user", "Hello"}, {"assistant", "Hi"}}
	if turns := prevCodex(env, Session{Agent: "codex", Path: f}); !reflect.DeepEqual(turns, want) {
		t.Fatalf("preview %q, want %q", turns, want)
	}
}

// Goal-driven threads and subagent threads carry no human prompt; the goal
// objective and the subagent's path/nickname stand in for it.
func TestCodexTitlesWithoutUserPrompt(t *testing.T) {
	env := testEnv(t)
	dir := filepath.Join(env.Home, ".codex/sessions/2026/09/23")
	goal := M{"type": "event_msg", "payload": M{"type": "thread_goal_updated", "goal": M{"objective": "Prove the  lemma"}}}
	internal := M{"type": "response_item", "payload": M{"type": "message", "role": "user", "content": []any{
		M{"type": "input_text", "text": "<codex_internal_context source=\"goal\">Continue</codex_internal_context>"}}}}
	jsonl(t, filepath.Join(dir, "rollout-2026-09-23T22-16-44-parent.jsonl"),
		M{"type": "session_meta", "payload": M{"cwd": repo, "thread_source": "user"}}, goal, internal)
	jsonl(t, filepath.Join(dir, "rollout-2026-09-23T22-17-00-child.jsonl"),
		M{"type": "session_meta", "payload": M{"cwd": repo, "thread_source": "subagent",
			"agent_path": "/root/audit", "agent_nickname": "Heisenberg"}}, goal, internal)
	want := map[string]string{"parent": "Prove the lemma", "child": "subagent: audit (Heisenberg)"}
	if got := titles(run(env, aCodex)); !reflect.DeepEqual(got, want) {
		t.Fatalf("titles %v, want %v", got, want)
	}
}

func TestRemoveWrappers(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a<system-reminder>x</system-reminder>b", "a b"},
		{"<system-reminder>unclosed <attachment>x</attachment>tail", "<system-reminder>unclosed  tail"},
		// the first close tag ends the block, like the lazy .*? in Python
		{"<ide_context><ide_context>a</ide_context>b</ide_context>c", " b</ide_context>c"},
		{"<system-reminder-x>a</system-reminder>ok", " ok"},                  // \b holds before '-'
		{"<attachmentX>a</attachmentX>ok", "<attachmentX>a</attachmentX>ok"}, // \b fails
		{"<local-command-stdout>x</local-command-stdout>y", " y"},
		{"<local-command-stdout>x</local-command-std>y", "<local-command-stdout>x</local-command-std>y"},
		{"<local-command-></local-command->", "<local-command-></local-command->"},
		{"<session_context\nmulti\nline</session_context>!", " !"},
	} {
		if got := removeWrappers(c.in); got != c.want {
			t.Errorf("removeWrappers(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPreviewStripKeepsMarkdownStructure(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"\r\n\n# Head  \r\n\r\n\r\n\n- a\n  - b\t\n\n```\ncode\n```\n\n\n", "# Head\n\n- a\n  - b\n\n```\ncode\n```"},
		{"<system-reminder>x</system-reminder>\n\nHello", "Hello"},
		{" \n\t\n<system-reminder>x</system-reminder>", ""},
	} {
		if got := previewStrip(c.in); got != c.want {
			t.Errorf("previewStrip(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFnmatchMatchesPythonSemantics(t *testing.T) {
	for _, c := range []struct {
		pat, name string
		want      bool
	}{
		{"ws/*", "ws/a/b", true}, // * crosses '/'
		{"*-old", "x/y-old", true},
		{"a?c", "a/c", true},
		{"[a-c]x", "bx", true},
		{"[!a-c]x", "bx", false},
		{"[!a-c]x", "dx", true},
		{"[z-a]", "z", false}, // empty range never matches
		{"[a-]", "-", true},
		{"[]]", "]", true},
		{"[", "[", true}, // unclosed set is literal
		{"[!]", "[!]", true},
		{"[^a]", "^", true},
		{"a.b", "axb", false},
		{"*", "", true},
	} {
		if got := fnmatchRe(c.pat).MatchString(c.name); got != c.want {
			t.Errorf("fnmatch(%q, %q) = %v, want %v", c.name, c.pat, got, c.want)
		}
	}
}

func TestFolderMatcherPatternKinds(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	cases := []struct {
		pats []string
		yes  []string
		no   []string
	}{
		{[]string{"."}, []string{"/r"}, []string{"/r/a"}},
		{[]string{"./ws/r5/"}, []string{"/r/ws/r5", "/r/ws/r5/x"}, []string{"/r/ws/r50", "/r/ws"}},
		{[]string{"ws/*"}, []string{"/r/ws/a/b"}, []string{"/r/ws"}},
		{[]string{"b"}, []string{"/r/a/b/c"}, []string{"/r/ab"}}, // bare name: any component
		{[]string{"/other"}, []string{"/other", "/other/x"}, []string{"/others"}},
		{[]string{"~/p"}, []string{"/home/u/p/q"}, []string{"/home/u/pq"}},
	}
	for _, c := range cases {
		m := FolderMatcher("/r", c.pats, false)
		for _, cwd := range c.yes {
			if !m(cwd) {
				t.Errorf("%v should match %q", c.pats, cwd)
			}
		}
		for _, cwd := range c.no {
			if m(cwd) {
				t.Errorf("%v should not match %q", c.pats, cwd)
			}
		}
	}
	if !FolderMatcher("/r", []string{" ", "/"}, true)("/x") {
		t.Error("blank patterns must fall back to empty")
	}
}

func TestParseTS(t *testing.T) {
	local := func(y, mo, d, h, mi, s int) float64 {
		return float64(time.Date(y, time.Month(mo), d, h, mi, s, 0, time.Local).Unix())
	}
	for _, c := range []struct {
		in   any
		want float64
	}{
		{"2024-01-02T03:04:05Z", 1704164645},
		{"2024-01-02T03:04:05.123456789Z", 1704164645.123456},
		{"2024-01-02T03:04:05.5+05:30", 1704144845.5},
		{"2024-01-02 03:04:05-0100", 1704168245},
		{"20240102T030405+00", 1704164645},
		{"2024-01-02T24:00:00Z", 1704240000},
		{"2024-01-02T24:00:01Z", 0},
		{"2024-02-30", 0},
		{"2024-01-02T03:04:05 +00:00", 1704164645},
		{"2024-01-02T03:04:05.", 0},
		{"2024-01", 0},
		{"  2024-01-02T03:04Z  ", 1704164640},
		{"2024-01-02T03:04:05+05:30:10.5", 1704144834.5},
		{"2024-01-02T03:04:05,25Z", 1704164645.25},
		{"2024-01-02T03:04:05+25:00", 0},
		{"2024-01-02X03:04:05Z", 1704164645},
		{"2024-01-02T03Z", 1704164400},
		{"2024-01-02T03:04:05ZZ", 0},
		{"2024-01-02", local(2024, 1, 2, 0, 0, 0)},
		{"2024-01-02T03:04:05", local(2024, 1, 2, 3, 4, 5)},
		{"nope", 0},
		{1700000000.0, 1700000000},
		{1700000000123.0, 1700000000.123},
		{1e11, 1e11},
		{true, 1},
		{nil, 0},
	} {
		if got := parseTS(c.in); got != c.want && !(math.Abs(got-c.want) < 1e-9 && c.want != 0) {
			t.Errorf("parseTS(%#v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCodexSidStripping(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"rollout-2025-01-02T10-11-12-0199a-b", "0199a-b"},
		// greedy [\d-]+ backtracks to the last '-' before a non-digit, as in Python
		{"rollout-2025-01-02T10-11-12-01997123-4abc", "4abc"},
		{"rollout-x", "rollout-x"},
		{"rollout-2025-01-02T10-11-12-", ""},
	} {
		if got := codexSidRe.ReplaceAllString(c.in, ""); got != c.want {
			t.Errorf("sid(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Transcripts without a recorded cwd fall back to their store directory
// name; folder filters must still apply to them.
func TestFolderFiltersApplyToCwdlessTranscripts(t *testing.T) {
	env := testEnv(t)
	jsonl(t, filepath.Join(env.Home, ".claude/projects/-work-repo/stub.jsonl"), M{"type": "bridge-session"})
	collect := func(only, skip []string) int {
		return len(Collect(env, Options{Root: repo, Agents: []string{"claude"},
			OnlyDir: FolderMatcher(repo, only, true), SkipDir: FolderMatcher(repo, skip, false)}, nil))
	}
	if n := collect(nil, nil); n != 1 {
		t.Fatalf("unfiltered: %d sessions, want 1", n)
	}
	if n := collect([]string{"nothing"}, nil); n != 0 {
		t.Fatalf("--only-folder nothing: %d sessions, want 0", n)
	}
	if n := collect(nil, []string{"-work-repo"}); n != 0 {
		t.Fatalf("--except-folder on the store name: %d sessions, want 0", n)
	}
}

func TestTildifyNeedsAPathBoundary(t *testing.T) {
	for _, c := range []struct{ path, want string }{
		{"/home/u", "~"},
		{"/home/u/x", "~/x"},
		{"/home/user2", "/home/user2"},
		{"/data/home/u/x", "/data/home/u/x"},
	} {
		if got := Tildify(c.path, "/home/u"); got != c.want {
			t.Errorf("Tildify(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
