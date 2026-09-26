// Command demo builds a fake home directory holding 30 coding-agent sessions
// in every store format lc reads, for demos and screenshots:
//
//	go run ./scripts/demo            # writes /tmp/lc-demo
//	/tmp/lc-demo/run.sh              # opens lc -I on the fake repository
//
// Nothing outside the target directory is touched. Resuming a demo session
// would start the real agent with a made-up id; press p instead of Enter.
package main

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const marker = ".lc-demo"

type turn struct{ role, text string }

type convo struct {
	agent, dir, title string
	age               time.Duration
	turns             []turn
}

func u(text string) turn { return turn{"user", text} }
func a(text string) turn { return turn{"assistant", text} }

type demo struct {
	home, repo string
	now        time.Time
	codexIndex []any
	kimiIndex  []any
}

func main() {
	target := "/tmp/lc-demo"
	if len(os.Args) > 1 {
		target = os.Args[1]
	}
	target, err := filepath.Abs(target)
	check(err)
	if _, err := os.Stat(target); err == nil {
		if _, err := os.Stat(filepath.Join(target, marker)); err != nil {
			fail("%s exists and was not made by this script; refusing to overwrite it", target)
		}
		check(os.RemoveAll(target))
	}
	d := &demo{home: target, repo: filepath.Join(target, "projects", "nimbus"), now: time.Now()}
	check(os.MkdirAll(d.repo, 0o755))
	write(filepath.Join(target, marker), "")
	for _, sub := range []string{"api", "web", "infra", "docs"} {
		check(os.MkdirAll(filepath.Join(d.repo, sub), 0o755))
	}
	write(filepath.Join(d.repo, "README.md"), "# nimbus\n\nForecast API, dashboard, and infrastructure.\n")
	if git, err := exec.LookPath("git"); err == nil {
		check(exec.Command(git, "-C", d.repo, "init", "-q").Run())
	}

	for i, c := range conversations {
		d.write(i, c)
	}
	writeJSONL(filepath.Join(target, ".codex", "session_index.jsonl"), d.codexIndex...)
	writeJSONL(filepath.Join(target, ".kimi-code", "session_index.jsonl"), d.kimiIndex...)

	run := filepath.Join(target, "run.sh")
	write(run, fmt.Sprintf(`#!/bin/sh
# Browse the lc demo sessions. Extra arguments go to lc (e.g. --json, -n 5).
export HOME=%[1]q XDG_CACHE_HOME=%[1]q/.cache XDG_DATA_HOME= PI_CODING_AGENT_DIR=
cd %[2]q
if [ $# -eq 0 ]; then set -- -I; fi
exec lc "$@"
`, target, d.repo))
	check(os.Chmod(run, 0o755))
	fmt.Printf("wrote %d demo sessions to %s\n\n  %s        # browse (press p, not Enter: ids are fake)\n  %s -n 0   # table\n",
		len(conversations), target, run, run)
}

// -------------------------------------------------------------- per agent

func (d *demo) write(i int, c convo) {
	cwd := d.repo
	if c.dir != "" {
		cwd = filepath.Join(d.repo, c.dir)
	}
	id := fakeID(c.title)
	ts := d.now.Add(-c.age)
	switch c.agent {
	case "claude":
		d.claude(c, cwd, id, ts)
	case "codex":
		d.codex(c, cwd, id, ts, i)
	case "droid":
		d.droid(c, cwd, id, ts)
	case "opencode":
		d.opencode(c, cwd, id, ts)
	case "cursor":
		d.cursor(c, cwd, id, ts)
	case "copilot":
		d.copilot(c, cwd, id, ts)
	case "grok":
		d.grok(c, cwd, id, ts)
	case "kimi":
		d.kimi(c, cwd, id, ts)
	case "pi":
		d.pi(c, cwd, id, ts)
	case "omp":
		d.omp(c, cwd, id, ts)
	case "gemini":
		d.gemini(c, cwd, id, ts)
	default:
		fail("unknown agent %q", c.agent)
	}
}

func (d *demo) claude(c convo, cwd, id string, ts time.Time) {
	lines := []any{map[string]any{"type": "summary", "aiTitle": c.title}}
	for _, t := range c.turns {
		content := any(t.text)
		if t.role == "assistant" {
			content = []any{map[string]any{"type": "text", "text": t.text}}
		}
		lines = append(lines, map[string]any{"type": t.role, "cwd": cwd, "sessionId": id,
			"message": map[string]any{"role": t.role, "content": content}})
	}
	f := filepath.Join(d.home, ".claude", "projects", mangle(cwd), id+".jsonl")
	writeJSONL(f, lines...)
	touch(f, ts)
}

func (d *demo) codex(c convo, cwd, id string, ts time.Time, i int) {
	start := ts.Add(-time.Duration(len(c.turns)) * 4 * time.Minute)
	name := fmt.Sprintf("rollout-%s-%s.jsonl", start.Format("2006-01-02T15-04-05"), id)
	f := filepath.Join(d.home, ".codex", "sessions", start.Format("2006"), start.Format("01"), start.Format("02"), name)
	meta := map[string]any{"id": id, "cwd": cwd, "cli_version": "0.155.1", "originator": "codex-tui", "source": "cli", "thread_source": "user"}
	subagent := strings.HasPrefix(c.title, "subagent: ")
	if subagent {
		meta["thread_source"] = "subagent"
		meta["agent_path"], meta["agent_nickname"] = "/root/review", "Curie"
	} else {
		d.codexIndex = append(d.codexIndex, map[string]any{"id": id, "thread_name": c.title, "updated_at": ts.UTC().Format(time.RFC3339Nano)})
	}
	lines := []any{map[string]any{"type": "session_meta", "payload": meta}}
	if !subagent {
		lines = append(lines, map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "<environment_context>\n  <cwd>" + cwd + "</cwd>\n</environment_context>"}}}})
	}
	// Alternate between the event log older Codex versions write and the
	// item_completed records of 0.150+, so both code paths show up.
	legacy := i%2 == 0
	for _, t := range c.turns {
		if subagent && t.role == "user" {
			continue
		}
		switch {
		case legacy && t.role == "user":
			lines = append(lines,
				map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": t.text}}}},
				map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": t.text}})
		case legacy:
			lines = append(lines, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "agent_message", "message": t.text}})
		default:
			kind, block := "UserMessage", "text"
			if t.role == "assistant" {
				kind, block = "AgentMessage", "Text"
			}
			lines = append(lines, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed",
				"item": map[string]any{"type": kind, "content": []any{map[string]any{"type": block, "text": t.text}}}}})
		}
	}
	writeJSONL(f, lines...)
	touch(f, ts)
}

func (d *demo) droid(c convo, cwd, id string, ts time.Time) {
	lines := []any{map[string]any{"type": "session_start", "cwd": cwd, "title": c.title}}
	for _, t := range c.turns {
		lines = append(lines, map[string]any{"type": "message", "message": map[string]any{"role": t.role, "content": t.text}})
	}
	f := filepath.Join(d.home, ".factory", "sessions", mangle(cwd), id+".jsonl")
	writeJSONL(f, lines...)
	touch(f, ts)
}

func (d *demo) opencode(c convo, cwd, id string, ts time.Time) {
	sid := "ses_" + strings.ReplaceAll(id, "-", "")[:24]
	storage := filepath.Join(d.home, ".local", "share", "opencode", "storage")
	created := ts.Add(-time.Duration(len(c.turns)) * 3 * time.Minute)
	writeJSON(filepath.Join(storage, "session", "prj_nimbus", sid+".json"), map[string]any{
		"id": sid, "directory": cwd, "title": c.title,
		"time": map[string]any{"created": created.UnixMilli(), "updated": ts.UnixMilli()},
	})
	for n, t := range c.turns {
		mid := fmt.Sprintf("msg_%s_%02d", sid[4:12], n)
		mf := filepath.Join(storage, "message", sid, mid+".json")
		writeJSON(mf, map[string]any{"id": mid, "sessionID": sid, "role": t.role})
		touch(mf, created.Add(time.Duration(n)*time.Minute)) // previews order by mtime
		writeJSON(filepath.Join(storage, "part", mid, "prt_01.json"), map[string]any{"type": "text", "text": t.text})
	}
}

func (d *demo) cursor(c convo, cwd, id string, ts time.Time) {
	sum := md5.Sum([]byte(cwd))
	db := filepath.Join(d.home, ".cursor", "chats", hex.EncodeToString(sum[:]), id, "store.db")
	check(os.MkdirAll(filepath.Dir(db), 0o755))
	con, err := sql.Open("sqlite", db)
	check(err)
	meta, _ := json.Marshal(map[string]any{"agentId": id, "name": c.title, "createdAt": ts.Add(-20 * time.Minute).UnixMilli(), "lastUsedModel": "claude-4.6-sonnet"})
	_, err = con.Exec(`create table meta (key text primary key, value text); insert into meta values ('0', ?)`, hex.EncodeToString(meta))
	check(err)
	check(con.Close())
	touch(db, ts)
}

func (d *demo) copilot(c convo, cwd, id string, ts time.Time) {
	sdir := filepath.Join(d.home, ".copilot", "session-state", id)
	write(filepath.Join(sdir, "workspace.yaml"), fmt.Sprintf("id: %s\ncwd: %s\nsummary: %s\ncreated_at: %s\nupdated_at: %s\n",
		id, cwd, c.title, ts.Add(-30*time.Minute).UTC().Format(time.RFC3339), ts.UTC().Format(time.RFC3339)))
	var lines []any
	for _, t := range c.turns {
		lines = append(lines, map[string]any{"type": t.role + ".message", "data": map[string]any{"content": t.text}})
	}
	events := filepath.Join(sdir, "events.jsonl")
	writeJSONL(events, lines...)
	touch(events, ts)
}

func (d *demo) grok(c convo, cwd, id string, ts time.Time) {
	sdir := filepath.Join(d.home, ".grok", "sessions", strings.ReplaceAll(url.QueryEscape(cwd), "+", "%20"), id)
	writeJSON(filepath.Join(sdir, "summary.json"), map[string]any{
		"info": map[string]any{"id": id}, "session_summary": c.title,
		"last_active_at": ts.UTC().Format(time.RFC3339), "num_messages": len(c.turns),
	})
	var lines []any
	for _, t := range c.turns {
		lines = append(lines, map[string]any{"type": t.role, "content": t.text})
	}
	chat := filepath.Join(sdir, "chat_history.jsonl")
	writeJSONL(chat, lines...)
	touch(chat, ts)
}

func (d *demo) kimi(c convo, cwd, id string, ts time.Time) {
	sid := "session_" + id
	sdir := filepath.Join(d.home, ".kimi-code", "sessions", "wd_nimbus", sid)
	writeJSON(filepath.Join(sdir, "state.json"), map[string]any{"title": c.title, "updatedAt": ts.UTC().Format(time.RFC3339)})
	var lines []any
	for _, t := range c.turns {
		lines = append(lines, map[string]any{"type": "context.append_message", "message": map[string]any{"role": t.role, "content": t.text}})
	}
	writeJSONL(filepath.Join(sdir, "agents", "main", "wire.jsonl"), lines...)
	d.kimiIndex = append(d.kimiIndex, map[string]any{"workDir": cwd, "sessionDir": sdir, "sessionId": sid})
}

// piEntries is the append-only message tree pi and omp share.
func piEntries(c convo) []any {
	var lines []any
	var parent any
	for n, t := range c.turns {
		mid := fmt.Sprintf("m%02d", n)
		lines = append(lines, map[string]any{"type": "message", "id": mid, "parentId": parent,
			"message": map[string]any{"role": t.role, "content": []any{map[string]any{"type": "text", "text": t.text}}}})
		parent = mid
	}
	return lines
}

func (d *demo) pi(c convo, cwd, id string, ts time.Time) {
	lines := append([]any{map[string]any{"type": "session", "version": 3, "id": id, "cwd": cwd}}, piEntries(c)...)
	// Every pi v3 entry, session_info included, is a node of the tree.
	lines = append(lines, map[string]any{"type": "session_info", "id": "info", "parentId": fmt.Sprintf("m%02d", len(c.turns)-1), "name": c.title})
	f := filepath.Join(d.home, ".pi", "agent", "sessions", "-"+mangle(cwd)+"--", ts.UTC().Format("2006-01-02T15-04-05")+"_"+id+".jsonl")
	writeJSONL(f, lines...)
	touch(f, ts)
}

func (d *demo) omp(c convo, cwd, id string, ts time.Time) {
	lines := append([]any{
		map[string]any{"type": "title", "v": 1, "title": c.title, "pad": strings.Repeat(" ", 32)},
		map[string]any{"type": "session", "version": 3, "id": id, "cwd": cwd},
	}, piEntries(c)...)
	f := filepath.Join(d.home, ".omp", "agent", "sessions", "-nimbus", ts.UTC().Format("2006-01-02T15-04-05")+"_"+id+".jsonl")
	writeJSONL(f, lines...)
	touch(f, ts)
}

func (d *demo) gemini(c convo, cwd, id string, ts time.Time) {
	sum := sha256.Sum256([]byte(cwd))
	pdir := filepath.Join(d.home, ".gemini", "tmp", hex.EncodeToString(sum[:]))
	write(filepath.Join(pdir, ".project_root"), cwd)
	var msgs []any
	for _, t := range c.turns {
		kind := "user"
		if t.role == "assistant" {
			kind = "gemini"
		}
		msgs = append(msgs, map[string]any{"type": kind, "content": t.text})
	}
	f := filepath.Join(pdir, "chats", "session-"+ts.UTC().Format("2006-01-02T15-04")+"-"+id[:8]+".json")
	writeJSON(f, map[string]any{"sessionId": id, "startTime": ts.Add(-15 * time.Minute).UTC().Format(time.RFC3339),
		"lastUpdated": ts.UTC().Format(time.RFC3339), "messages": msgs})
	touch(f, ts)
}

// ---------------------------------------------------------------- helpers

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// mangle is how Claude Code and droid name per-project directories.
func mangle(cwd string) string { return nonAlnum.ReplaceAllString(cwd, "-") }

// fakeID derives a stable UUID-shaped id. It starts with a letter so Codex's
// rollout-name parsing can't mistake it for part of the timestamp.
func fakeID(seed string) string {
	h := sha1.Sum([]byte(seed))
	s := hex.EncodeToString(h[:16])
	s = string(rune('a'+h[0]%6)) + s[1:]
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

func write(path, content string) {
	check(os.MkdirAll(filepath.Dir(path), 0o755))
	check(os.WriteFile(path, []byte(content), 0o644))
}

func writeJSON(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	check(err)
	write(path, string(b)+"\n")
}

func writeJSONL(path string, lines ...any) {
	var b strings.Builder
	for _, l := range lines {
		j, err := json.Marshal(l)
		check(err)
		b.Write(j)
		b.WriteByte('\n')
	}
	write(path, b.String())
}

func touch(path string, ts time.Time) { check(os.Chtimes(path, ts, ts)) }

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "demo: "+format+"\n", args...)
	os.Exit(1)
}
