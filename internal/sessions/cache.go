package sessions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// Persistent metadata cache for Codex rollout discovery. The format matches
// the Python implementation's (src/lc/cache.py) apart from the version.

// codexCacheVersion 2 invalidates titles scanned before lc read Codex's
// item_completed events, goal objectives, and subagent metadata.
const codexCacheVersion = 2

// codexEntry is the complete cache contract for one rollout file.
type codexEntry struct {
	MtimeNS      int64
	Size         int64
	Cwd          string
	SessionID    string
	Title        *string // nil when no title was found
	TitleScanned bool
}

func (e codexEntry) matches(fi os.FileInfo) bool {
	return e.MtimeNS == fi.ModTime().UnixNano() && e.Size == fi.Size()
}

// codexCache reads unchanged entries and atomically persists meaningful
// batches. A single currently-active rollout is intentionally re-read instead
// of rewriting the entire cache on every agent turn.
type codexCache struct {
	path    string
	stored  map[string]codexEntry
	next    map[string]codexEntry
	order   []string // insertion order of next, as Python dicts keep it
	changes int
}

func newCodexCache(path string) *codexCache {
	return &codexCache{path: path, stored: loadCodexCache(path), next: map[string]codexEntry{}}
}

func loadCodexCache(path string) map[string]codexEntry {
	raw, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(raw) {
		return map[string]codexEntry{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var data any
	if dec.Decode(&data) != nil || dec.More() {
		return map[string]codexEntry{}
	}
	m, ok := data.(obj)
	if !ok || !isCacheVersion(m["version"]) {
		return map[string]codexEntry{}
	}
	sessions, ok := m["sessions"].(obj)
	if !ok {
		return map[string]codexEntry{}
	}
	out := make(map[string]codexEntry, len(sessions))
	for p, v := range sessions {
		if e, ok := codexEntryFromJSON(v); ok {
			out[p] = e
		}
	}
	return out
}

// isCacheVersion compares a decoded JSON number with codexCacheVersion
// numerically, so 2 and 2.0 both match.
func isCacheVersion(v any) bool {
	x, ok := v.(json.Number)
	if !ok {
		return false
	}
	f, err := x.Float64()
	return err == nil && f == codexCacheVersion
}

// pyInt accepts what Python's isinstance(v, int) accepts from json.load:
// integer literals and booleans.
func pyInt(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		if strings.ContainsAny(string(x), ".eE") {
			return 0, false
		}
		n, err := x.Int64()
		return n, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func codexEntryFromJSON(v any) (codexEntry, bool) {
	m, ok := v.(obj)
	if !ok {
		return codexEntry{}, false
	}
	var e codexEntry
	for _, k := range [...]string{"mtime_ns", "size", "cwd", "session_id", "title_scanned"} {
		if _, ok := m[k]; !ok {
			return e, false
		}
	}
	var ok1, ok2, ok3, ok4, ok5 bool
	e.MtimeNS, ok1 = pyInt(m["mtime_ns"])
	e.Size, ok2 = pyInt(m["size"])
	e.Cwd, ok3 = m["cwd"].(string)
	e.SessionID, ok4 = m["session_id"].(string)
	e.TitleScanned, ok5 = m["title_scanned"].(bool)
	if !(ok1 && ok2 && ok3 && ok4 && ok5) {
		return e, false
	}
	switch t := m["title"].(type) {
	case nil:
	case string:
		e.Title = &t
	default:
		return e, false
	}
	return e, true
}

func (c *codexCache) get(path string, fi os.FileInfo) (codexEntry, bool) {
	if e, ok := c.stored[path]; ok && e.matches(fi) {
		return e, true
	}
	c.changes++
	return codexEntry{}, false
}

func (c *codexCache) remember(path string, e codexEntry, changed bool) {
	if _, ok := c.next[path]; !ok {
		c.order = append(c.order, path)
	}
	c.next[path] = e
	if changed {
		c.changes++
	}
}

func (c *codexCache) flush() {
	if d := len(c.next) - len(c.stored); len(c.stored) > 0 && c.changes <= 1 && d >= -1 && d <= 1 {
		return
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"version":%d,"sessions":{`, codexCacheVersion)
	for i, p := range c.order {
		e := c.next[p]
		if i > 0 {
			b.WriteByte(',')
		}
		pyQuote(&b, p)
		fmt.Fprintf(&b, `:{"mtime_ns":%d,"size":%d,"cwd":`, e.MtimeNS, e.Size)
		pyQuote(&b, e.Cwd)
		b.WriteString(`,"session_id":`)
		pyQuote(&b, e.SessionID)
		b.WriteString(`,"title":`)
		if e.Title == nil {
			b.WriteString("null")
		} else {
			pyQuote(&b, *e.Title)
		}
		fmt.Fprintf(&b, `,"title_scanned":%t}`, e.TitleScanned)
	}
	b.WriteString("}}")

	dir, name := "", c.path
	if i := strings.LastIndexByte(c.path, '/'); i >= 0 {
		dir, name = c.path[:i+1], c.path[i+1:]
	}
	if dir != "" && os.MkdirAll(dir, 0o777) != nil {
		return
	}
	tmp := dir + pyStem(name) + ".tmp" // Path.with_suffix(".tmp")
	if os.WriteFile(tmp, b.Bytes(), 0o666) != nil {
		return
	}
	_ = os.Rename(tmp, c.path)
}

// pyQuote writes s as json.dump does with ensure_ascii; undecodable bytes are
// written as the lone surrogates Python's surrogateescape would produce.
func pyQuote(b *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	u := func(r rune) {
		b.WriteString(`\u`)
		b.WriteByte(hex[r>>12&0xf])
		b.WriteByte(hex[r>>8&0xf])
		b.WriteByte(hex[r>>4&0xf])
		b.WriteByte(hex[r&0xf])
	}
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, sz := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && sz == 1 {
			u(0xdc00 | rune(s[i]))
			i++
			continue
		}
		i += sz
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r >= 0x20 && r <= 0x7e:
				b.WriteRune(r)
			case r > 0xffff:
				r -= 0x10000
				u(0xd800 | (r>>10)&0x3ff)
				u(0xdc00 | r&0x3ff)
			default:
				u(r)
			}
		}
	}
	b.WriteByte('"')
}
