package sessions

import (
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	_ "modernc.org/sqlite" // cursor's store.db
)

// Each adapter reports Session values through add. keep(cwd) decides repo
// membership; dirOK(name) pre-filters path-mangled directory names so we never
// open files belonging to other repositories.

// scanTwice runs a cheap head scan, widening only when it found no usable title.
func scanTwice(scan func(limit int64) (string, titlePick), size int64) (string, titlePick) {
	cwd, pick := scan(head)
	if pick.value() == "" && size > head {
		cwd2, pick2 := scan(min(size, rescan))
		if cwd == "" {
			cwd = cwd2
		}
		if pick2.value() != "" {
			pick = pick2
		}
	}
	return cwd, pick
}

// rglob is Path.rglob(prefix+"*"+suffix): every entry kind below base,
// symlinked directories not followed.
func rglob(base, prefix, suffix string, fn func(path string)) {
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != base {
				return fs.SkipDir
			}
			return nil
		}
		if p == base {
			return nil
		}
		n := d.Name()
		if len(n) >= len(prefix)+len(suffix) && strings.HasPrefix(n, prefix) && strings.HasSuffix(n, suffix) {
			fn(p)
		}
		return nil
	})
}

func exists(path string) bool {
	_, ok := stat(path)
	return ok
}

// decodeReplace is bytes.decode("utf-8", "replace").
func decodeReplace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b) + 8)
	for len(b) > 0 {
		r, sz := utf8.DecodeRune(b)
		if r == utf8.RuneError && sz == 1 {
			sb.WriteRune(utf8.RuneError)
		} else {
			sb.Write(b[:sz])
		}
		b = b[sz:]
	}
	return sb.String()
}

// pyFormat is f"{v}" for a decoded JSON value.
func pyFormat(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return pyNum(x)
	case bool:
		if x {
			return "True"
		}
		return "False"
	case nil:
		return "None"
	}
	return fmt.Sprint(v)
}

// ------------------------------------------------------------------- claude

func claudeSessionNames(env *Env) map[string]string {
	names := map[string]string{}
	for _, p := range globSuffix(pyJoin(env.Home, ".claude/sessions"), ".json") {
		entry := loadObj(p)
		sid, _ := str(entry, "sessionId")
		if name := cleanStr(entry["name"]); sid != "" && name != "" {
			names[sid] = name
		}
	}
	return names
}

func aClaude(env *Env, keep, dirOK func(string) bool, add func(Session)) {
	base := pyJoin(env.Home, ".claude/projects")
	names := claudeSessionNames(env)
	pdirs := iterDirs(base)
	sort.Strings(pdirs)
	for _, pdir := range pdirs {
		pname := pyBase(pdir)
		if !dirOK(pname) {
			continue
		}
		for _, f := range globSuffix(pdir, ".jsonl") {
			stem := pyStem(pyBase(f))
			scan := func(limit int64) (string, titlePick) {
				cwd, cwdSet := "", false
				name, named := names[stem]
				var pick titlePick
				if named {
					pick.best = name
				}
				jsonlHead(f, limit, func(e obj) bool {
					if !cwdSet {
						cwd, cwdSet = str(e, "cwd")
					}
					if !named {
						if name, named = cleanAny(e["aiTitle"]); named {
							pick.best = name
						}
					}
					if !named && e["type"] == "user" && !truthy(e["isMeta"]) {
						pick.offer(blocksToText(sub(e, "message")["content"]))
					}
					return !(cwd != "" && (named || pick.best != ""))
				})
				return cwd, pick
			}
			size := sizeOf(f)
			cwd, pick := scanTwice(scan, size)
			if cwd != "" && !keep(cwd) {
				continue
			}
			if cwd == "" {
				cwd = pname
			}
			add(Session{Agent: "claude", ID: stem, Cwd: cwd, Title: pick.value(), Updated: mtime(f), Path: f, Size: size})
		}
	}
}

// -------------------------------------------------------------------- codex

// codexSessionNames reads Codex's compact thread index, which owns display names.
func codexSessionNames(env *Env) map[string]string {
	names := map[string]string{}
	blob, err := os.ReadFile(pyJoin(env.Home, ".codex/session_index.jsonl"))
	if err != nil {
		return names
	}
	for line := range strings.SplitSeq(string(blob), "\n") {
		var e obj
		if !utf8.ValidString(line) || json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		sid, _ := str(e, "id")
		if name := cleanStr(e["thread_name"]); sid != "" && name != "" {
			names[sid] = name
		}
	}
	return names
}

var codexSidRe = regexp.MustCompile(`^rollout-\p{Nd}{4}-\p{Nd}\p{Nd}-\p{Nd}\p{Nd}T[\p{Nd}-]+-`)

func aCodex(env *Env, keep, _ func(string) bool, add func(Session)) {
	names := codexSessionNames(env)
	cache := newCodexCache(pyJoin(env.CacheDir, "codex-sessions.json"))
	rglob(pyJoin(env.Home, ".codex/sessions"), "rollout-", ".jsonl", func(f string) {
		fi, err := os.Stat(f)
		if err != nil {
			return
		}
		var (
			cwd, sid      string
			title         *string
			titleScanned  bool
			recordChanged bool
		)
		if e, ok := cache.get(f, fi); ok {
			cwd, sid, title, titleScanned = e.Cwd, e.SessionID, e.Title, e.TitleScanned
		} else {
			// Most rollouts belong to another project. Read only their compact
			// session metadata before deciding whether a title scan is needed.
			cwd, _ = jsonStr(readHead(f, cwdHead), "cwd")
			sid = codexSidRe.ReplaceAllString(pyStem(filepath.Base(f)), "")
			recordChanged = true
		}
		if cwd == "" {
			return
		}
		if n, ok := names[sid]; ok {
			title = &n
		}
		entry := func() codexEntry {
			return codexEntry{fi.ModTime().UnixNano(), fi.Size(), cwd, sid, title, titleScanned}
		}
		if !keep(cwd) {
			cache.remember(f, entry(), false)
			return
		}
		titleChanged := false
		if title == nil && !titleScanned {
			blob := readHead(f, head)
			scan := func(limit int64) (string, titlePick) {
				var pick titlePick
				b := blob
				if limit != head {
					b = readHead(f, limit)
				}
				iterLinesWith(b, codexNeedles, func(e obj) bool {
					payload := sub(e, "payload")
					switch {
					case e["type"] == "response_item" && payload["role"] == "user":
						return !pick.offer(blocksToText(payload["content"]))
					case e["type"] == "event_msg" && payload["type"] == "user_message":
						return !pick.offerAny(payload["message"])
					case e["type"] == "event_msg":
						if role, text, ok := codexItem(payload); ok && role == "user" {
							return !pick.offer(text, true)
						}
					}
					return true
				})
				return cwd, pick
			}
			_, pick := scanTwice(scan, fi.Size())
			v := pick.value()
			if v == "" {
				v = codexFallbackTitle(blob)
			}
			if v != "" {
				title = &v
			}
			titleScanned = true
			titleChanged = !recordChanged
		}
		cache.remember(f, entry(), titleChanged)
		s := Session{Agent: "codex", ID: sid, Cwd: cwd, Updated: pyMtime(fi), Path: f, Size: fi.Size()}
		if title != nil {
			s.Title = *title
		}
		add(s)
	})
	cache.flush()
}

var codexFallbackNeedles = [][]byte{[]byte(`"session_meta"`), []byte(`"thread_goal_updated"`)}

// codexFallbackTitle names rollouts that have no user prompt: subagent
// threads by their agent path and nickname, and goal-driven threads by the
// goal objective. Subagents come first because forked ones inherit the
// parent's goal record.
func codexFallbackTitle(blob []byte) string {
	var subagent, goal string
	iterLinesWith(blob, codexFallbackNeedles, func(e obj) bool {
		p := sub(e, "payload")
		switch {
		case e["type"] == "session_meta" && p["thread_source"] == "subagent":
			path, _ := str(p, "agent_path")
			label := strings.TrimPrefix(path, "/root/")
			if nick, _ := str(p, "agent_nickname"); nick != "" {
				label = strings.TrimSpace(label + " (" + nick + ")")
			}
			if label != "" {
				subagent = "subagent: " + label
			}
			return false
		case e["type"] == "event_msg" && p["type"] == "thread_goal_updated" && goal == "":
			objective, _ := str(sub(p, "goal"), "objective")
			goal, _ = CleanTitle(objective)
		}
		return true
	})
	if subagent != "" {
		return subagent
	}
	return goal
}

// -------------------------------------------------------------------- droid

func aDroid(env *Env, keep, dirOK func(string) bool, add func(Session)) {
	for _, pdir := range iterDirs(pyJoin(env.Home, ".factory/sessions")) {
		pname := pyBase(pdir)
		if !dirOK(pname) {
			continue
		}
		for _, f := range globSuffix(pdir, ".jsonl") {
			scan := func(limit int64) (string, string) {
				var cwd, name string
				var pick titlePick
				jsonlHead(f, limit, func(e obj) bool {
					switch e["type"] {
					case "session_start":
						cwd, _ = str(e, "cwd")
						name = cleanStr(e["title"])
						if l := strings.ToLower(name); l == "new session" || l == "start new chat" {
							name = ""
						}
					case "message":
						if pick.best == "" {
							msg := sub(e, "message")
							if msg["role"] == "user" && !truthy(msg["hookEventName"]) {
								pick.offer(blocksToText(msg["content"]))
							}
						}
					}
					return !(cwd != "" && (name != "" || pick.best != ""))
				})
				if name != "" {
					return cwd, name
				}
				return cwd, pick.value()
			}
			size := sizeOf(f)
			cwd, name := scan(head)
			if name == "" && size > head {
				var cwd2 string
				cwd2, name = scan(min(size, rescan))
				if cwd == "" {
					cwd = cwd2
				}
			}
			if cwd != "" && !keep(cwd) {
				continue
			}
			if cwd == "" {
				cwd = pname
			}
			add(Session{Agent: "droid", ID: pyStem(pyBase(f)), Cwd: cwd, Title: name, Updated: mtime(f), Path: f, Size: size})
		}
	}
}

// ----------------------------------------------------------------- opencode

func aOpencode(env *Env, keep, _ func(string) bool, add func(Session)) {
	rglob(pyJoin(env.Home, ".local/share/opencode/storage/session"), "", ".json", func(f string) {
		d, ok := loadJSON(f).(obj)
		if !ok {
			return
		}
		cwd, _ := str(d, "directory")
		if cwd == "" || !keep(cwd) {
			return
		}
		t := sub(d, "time")
		ts := parseTS(or(t["updated"], t["created"]))
		if ts == 0 {
			ts = mtime(f)
		}
		add(Session{Agent: "opencode", ID: strOr(d, "id", pyStem(filepath.Base(f))), Cwd: cwd,
			Title: cleanStr(d["title"]), Updated: ts, Path: f})
	})
}

// ------------------------------------------------------------------- cursor

// cursorMeta reads a chat's metadata row; cursor stores the JSON hex-encoded.
func cursorMeta(db string) obj {
	con, err := sql.Open("sqlite", "file:"+db+"?mode=ro&immutable=1")
	if err != nil {
		return nil
	}
	defer con.Close()
	var raw any
	if con.QueryRow("select value from meta order by key limit 1").Scan(&raw) != nil {
		return nil
	}
	var b []byte
	switch x := raw.(type) {
	case []byte:
		b = x
	case string:
		b = []byte(x)
	default:
		return nil
	}
	if len(b) == 0 {
		return nil
	}
	if len(b)%2 == 0 && isHex(b) {
		d := make([]byte, len(b)/2)
		if _, err := hex.Decode(d, b); err != nil {
			return nil
		}
		b = d
	}
	var m obj
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func isHex(b []byte) bool {
	for _, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func aCursor(env *Env, keep func(string) bool, cands []string, add func(Session)) {
	base := pyJoin(env.Home, ".cursor/chats")
	if !isDir(base) {
		return
	}
	for _, cwd := range cands {
		// candidates are a superset (root, its children, other agents' cwds);
		// re-check keep() since folder filters aren't applied to that set
		if !keep(cwd) {
			continue
		}
		sum := md5.Sum([]byte(cwd))
		wdir := pyJoin(base, hex.EncodeToString(sum[:]))
		if !isDir(wdir) {
			continue
		}
		for _, sdir := range iterDirs(wdir) {
			db := pyJoin(sdir, "store.db")
			if !exists(db) {
				continue
			}
			meta := cursorMeta(db)
			dbm := mtime(db)
			ts := parseTS(meta["createdAt"])
			if ts == 0 {
				ts = dbm
			}
			add(Session{Agent: "cursor", ID: strOr(meta, "agentId", pyBase(sdir)), Cwd: cwd,
				Title: cleanStr(meta["name"]), Updated: max(ts, dbm), Path: db, Size: sizeOf(db),
				Note: strOr(meta, "lastUsedModel", "")})
		}
	}
}

// ------------------------------------------------------------------ copilot

// pySplitlines is str.splitlines().
func pySplitlines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, sz := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, s[start:i])
			if r == '\r' && i+1 < len(s) && s[i+1] == '\n' {
				sz = 2
			}
			start = i + sz
		}
		i += sz
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func aCopilot(env *Env, keep, _ func(string) bool, add func(Session)) {
	for _, sdir := range iterDirs(pyJoin(env.Home, ".copilot/session-state")) {
		wf := pyJoin(sdir, "workspace.yaml")
		if !exists(wf) {
			continue
		}
		info := map[string]string{}
		for _, line := range pySplitlines(decodeReplace(readHead(wf, 8192))) {
			if strings.Contains(line, ":") && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "-") {
				k, v, _ := strings.Cut(line, ":")
				info[pyStrip(k)] = strings.Trim(pyStrip(v), `'"`)
			}
		}
		cwd := info["cwd"]
		if cwd == "" || !keep(cwd) {
			continue
		}
		upd := info["updated_at"]
		if upd == "" {
			upd = info["created_at"]
		}
		ts := parseTS(upd)
		if ts == 0 {
			ts = mtime(wf)
		}
		id, ok := info["id"]
		if !ok {
			id = pyBase(sdir)
		}
		events := pyJoin(sdir, "events.jsonl")
		add(Session{Agent: "copilot", ID: id, Cwd: cwd, Title: cleanStr(info["summary"]),
			Updated: max(ts, mtime(events)), Path: sdir, Size: sizeOf(events)})
	}
}

// --------------------------------------------------------------------- grok

// pyUnquote is urllib.parse.unquote: valid %XX escapes become bytes, the
// result is decoded as UTF-8 with replacement.
func pyUnquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex([]byte{s[i+1], s[i+2]}) {
			v, _ := hex.DecodeString(s[i+1 : i+3])
			b = append(b, v[0])
			i += 2
			continue
		}
		b = append(b, s[i])
	}
	return decodeReplace(b)
}

func aGrok(env *Env, keep, _ func(string) bool, add func(Session)) {
	for _, wdir := range iterDirs(pyJoin(env.Home, ".grok/sessions")) {
		cwd := pyUnquote(pyBase(wdir))
		if !keep(cwd) {
			continue
		}
		prompts := map[string]any{}
		if history := pyJoin(wdir, "prompt_history.jsonl"); exists(history) {
			jsonlHead(history, 1<<20, func(e obj) bool {
				sid, _ := str(e, "session_id")
				if _, seen := prompts[sid]; sid != "" && truthy(e["prompt"]) && !seen && !truthy(e["is_bash"]) {
					prompts[sid] = e["prompt"]
				}
				return true
			})
		}
		for _, sdir := range iterDirs(wdir) {
			summary := loadObj(pyJoin(sdir, "summary.json"))
			sid := strOr(sub(summary, "info"), "id", pyBase(sdir))
			title := cleanStr(summary["session_summary"])
			if title == "" {
				title = cleanStr(prompts[sid])
			}
			ts := parseTS(or(summary["last_active_at"], summary["updated_at"]))
			if ts == 0 {
				ts = mtime(sdir)
			}
			chat := pyJoin(sdir, "chat_history.jsonl")
			note := ""
			if n := summary["num_messages"]; truthy(n) {
				note = pyFormat(n) + " msgs"
			}
			add(Session{Agent: "grok", ID: sid, Cwd: cwd, Title: title, Updated: max(ts, mtime(chat)),
				Path: sdir, Size: sizeOf(chat), Note: note})
		}
	}
}

// --------------------------------------------------------------------- kimi

func aKimi(env *Env, keep, _ func(string) bool, add func(Session)) {
	jsonlHead(pyJoin(env.Home, ".kimi-code/session_index.jsonl"), 1<<20, func(e obj) bool {
		cwd, _ := str(e, "workDir")
		sd, _ := str(e, "sessionDir")
		if cwd == "" || sd == "" || !keep(cwd) {
			return true
		}
		sdir := pyPath(sd)
		st := loadObj(pyJoin(sdir, "state.json"))
		title := cleanStr(st["title"])
		if title == "" {
			title = cleanStr(st["lastPrompt"])
		}
		ts := parseTS(or(st["updatedAt"], st["createdAt"]))
		if ts == 0 {
			ts = mtime(sdir)
		}
		add(Session{Agent: "kimi", ID: strOr(e, "sessionId", pyBase(sdir)), Cwd: cwd, Title: title, Updated: ts, Path: sdir})
		return true
	})
}

// ----------------------------------------------------------------------- pi

// sessionHeader returns the session header: the first parsable line, or the second
// when the first is omp's fixed-width title slot (slot is then non-nil).
func sessionHeader(f string, slotted bool) (header, slot obj) {
	n := 0
	jsonlHead(f, cwdHead, func(e obj) bool {
		n++
		if n == 1 {
			if slotted && e["type"] == "title" {
				slot = e
				return true
			}
			header = e
			return false
		}
		header = e
		return false
	})
	return header, slot
}

func headerIDs(header obj) (sid, cwd string, ok bool) {
	if header == nil || header["type"] != "session" {
		return "", "", false
	}
	sid, ok1 := str(header, "id")
	cwd, ok2 := str(header, "cwd")
	return sid, cwd, ok1 && ok2 && cwd != ""
}

var (
	piNeedles    = [][]byte{[]byte(`"session_info"`), []byte(`"user"`)}
	userNeedles  = [][]byte{[]byte(`"user"`)}
	codexNeedles = [][]byte{[]byte(`"user"`), []byte(`"user_message"`), []byte(`"UserMessage"`)}
)

func aPi(env *Env, keep, _ func(string) bool, add func(Session)) {
	for _, pdir := range iterDirs(pyJoin(env.PiAgentDir, "sessions")) {
		// Pi encodes cwd in the directory name, but the header is authoritative
		// and supports both current and legacy encodings without guessing.
		for _, f := range globSuffix(pdir, ".jsonl") {
			header, _ := sessionHeader(f, false)
			sid, cwd, ok := headerIDs(header)
			if !ok || !keep(cwd) {
				continue
			}
			scan := func(limit int64) (name string, named bool, pick titlePick) {
				iterLinesWith(readHead(f, limit), piNeedles, func(e obj) bool {
					switch e["type"] {
					case "session_info":
						name, named = cleanStr(e["name"]), true
					case "message":
						if msg := sub(e, "message"); msg["role"] == "user" {
							pick.offer(blocksToText(msg["content"]))
						}
					}
					return true
				})
				return
			}
			size := sizeOf(f)
			name, _, pick := scan(head)
			tailNamed := false
			if size > head {
				// /name can be run long after the opening prompt. A small tail
				// scan recovers the latest session_info without reading the
				// whole transcript merely to discover that display name.
				iterLinesWith(tailBytes(f, tailSizes[0]), piNeedles[:1], func(e obj) bool {
					if e["type"] == "session_info" {
						name, tailNamed = cleanStr(e["name"]), true
					}
					return true
				})
			}
			if name == "" && pick.value() == "" && size > head {
				headName, headNamed, p := scan(min(size, rescan))
				pick = p
				if headNamed && !tailNamed {
					name = headName
				}
			}
			if name == "" {
				name = pick.value()
			}
			add(Session{Agent: "pi", ID: sid, Cwd: cwd, Title: name, Updated: mtime(f), Path: f, Size: size})
		}
	}
}

// ---------------------------------------------------------------------- omp

// ompSessionsDir: omp moves its data under $XDG_DATA_HOME/omp once that
// directory exists.
func ompSessionsDir(env *Env) string {
	if env.DataHome != "" && isDir(pyJoin(env.DataHome, "omp")) {
		return pyJoin(env.DataHome, "omp/sessions")
	}
	return pyJoin(env.Home, ".omp/agent/sessions")
}

func aOmp(env *Env, keep, _ func(string) bool, add func(Session)) {
	for _, pdir := range iterDirs(ompSessionsDir(env)) {
		// Subagent transcripts live in a <session>/ directory beside their
		// parent; only top-level files are sessions a user started.
		for _, f := range globSuffix(pdir, ".jsonl") {
			// Current files open with a fixed-width title slot that omp rewrites
			// in place; legacy files start directly with the session header.
			header, slot := sessionHeader(f, true)
			sid, cwd, ok := headerIDs(header)
			if !ok || !keep(cwd) {
				continue
			}
			size := sizeOf(f)
			title := ""
			if slot != nil {
				title = cleanStr(slot["title"])
			}
			if title == "" {
				title = cleanStr(header["title"])
			}
			if title == "" {
				scan := func(limit int64) (string, titlePick) {
					var pick titlePick
					iterLinesWith(readHead(f, limit), userNeedles, func(e obj) bool {
						if e["type"] != "message" {
							return true
						}
						msg := sub(e, "message")
						return !(msg["role"] == "user" && pick.offer(blocksToText(msg["content"])))
					})
					return cwd, pick
				}
				_, pick := scanTwice(scan, size)
				title = pick.value()
			}
			add(Session{Agent: "omp", ID: sid, Cwd: cwd, Title: title, Updated: mtime(f), Path: f, Size: size})
		}
	}
}

// ------------------------------------------------------------------- gemini

// geminiText flattens Gemini content: a plain string in older sessions, or a
// list of {"text": ...} blocks (no "type" tag, unlike Anthropic/OpenAI blocks).
func geminiText(content any) (string, bool) {
	switch c := content.(type) {
	case string:
		return c, true
	case []any:
		var parts []string
		for _, b := range c {
			if m, ok := b.(obj); ok {
				if s, ok := m["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return strings.Join(parts, "\n"), true
	}
	return "", false
}

var geminiRole = map[string]string{"user": "user", "gemini": "assistant"}

// geminiRaw returns the raw message list of one chat file. The .jsonl variant
// is a log of {"$set": {...}} patches; a patch that sets "messages" carries
// the full array, so the last one seen is the current state. blob is the
// file's first 2 MiB (jsonl) and d its decoded document (json).
func geminiRaw(d obj, blob []byte, jsonl bool) []any {
	if !jsonl {
		raw, _ := d["messages"].([]any)
		return raw
	}
	var raw []any
	iterLines(blob, func(e obj) bool {
		if m, ok := sub(e, "$set")["messages"].([]any); ok {
			raw = m
		}
		return true
	})
	return raw
}

// geminiTurns converts raw messages to turns, normalising text with norm and
// dropping empty ones; firstUser stops at the first user turn.
func geminiTurns(raw []any, norm func(string) string, firstUser bool) []Turn {
	var out []Turn
	for _, v := range raw {
		m, ok := v.(obj)
		if !ok {
			continue
		}
		kind, _ := m["type"].(string)
		role := geminiRole[kind]
		if role == "" || firstUser && role != "user" {
			continue
		}
		text, ok := geminiText(m["content"])
		if !ok {
			continue
		}
		if text = norm(text); text != "" {
			out = append(out, Turn{role, text})
			if firstUser {
				break
			}
		}
	}
	return out
}

func geminiFile(f string) (d obj, blob []byte, jsonl bool) {
	if pySuffix(pyBase(f)) == ".json" {
		return loadObj(f), nil, false
	}
	return nil, readHead(f, 1<<21), true
}

func aGemini(env *Env, keep func(string) bool, cands []string, add func(Session)) {
	byHash := make(map[string]string, len(cands))
	for _, c := range cands {
		sum := sha256.Sum256([]byte(c))
		byHash[hex.EncodeToString(sum[:])] = c
	}
	for _, pdir := range iterDirs(pyJoin(env.Home, ".gemini/tmp")) {
		cwd := ""
		if rootFile := pyJoin(pdir, ".project_root"); exists(rootFile) {
			cwd = pyStrip(decodeReplace(readHead(rootFile, 4096)))
		}
		if cwd == "" {
			cwd = byHash[pyBase(pdir)]
		}
		if cwd == "" || !keep(cwd) {
			continue
		}
		chats := pyJoin(pdir, "chats")
		if !isDir(chats) {
			continue
		}
		names := listNames(chats)
		sort.Strings(names)
		for _, name := range names {
			if suf := pySuffix(name); suf != ".json" && suf != ".jsonl" {
				continue
			}
			f := pyJoin(chats, name)
			sid, ts := pyStem(name), mtime(f)
			d, blob, jsonl := geminiFile(f)
			if !jsonl {
				sid = strOr(d, "sessionId", sid)
				if t := parseTS(or(d["lastUpdated"], d["startTime"])); t != 0 {
					ts = t
				}
			} else {
				h := blob[:min(len(blob), 1<<16)]
				if s, _ := jsonStr(h, "sessionId"); s != "" {
					sid = s
				}
				upd, _ := jsonStr(h, "lastUpdated")
				if upd == "" {
					upd, _ = jsonStr(h, "startTime")
				}
				if t := parseTS(upd); t != 0 {
					ts = t
				}
			}
			title := ""
			if t := geminiTurns(geminiRaw(d, blob, jsonl), titleStrip, true); len(t) > 0 {
				title = t[0].Text
			}
			add(Session{Agent: "gemini", ID: sid, Cwd: cwd, Title: title, Updated: ts, Path: f, Size: sizeOf(f)})
		}
	}
}
