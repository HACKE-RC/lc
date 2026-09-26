// Package sessions discovers, titles and previews coding-agent sessions from
// the local stores of every supported agent.
package sessions

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Byte budgets shared by the adapters and previewers.
const (
	cwdHead = 1 << 16 // cwd appears in the opening session metadata
	head    = 1 << 18 // bytes of a matching transcript scanned for a title fallback
	rescan  = 2 << 20 // retry budget when that window held no usable title

	titleSourceLimit = 1 << 16
	// TitleDisplayLimit bounds a title in runes.
	TitleDisplayLimit = 240

	maxTurns = 8
)

var tailSizes = [...]int64{1 << 18, 1 << 20, 1 << 22} // 256K, 1M, 4M

type obj = map[string]any

// ---------------------------------------------------------- Python semantics

// isPySpace reports Python's str.isspace (also what `\s` matches in str regexes).
func isPySpace(r rune) bool {
	switch {
	case r == ' ', r >= '\t' && r <= '\r', r >= 0x1c && r <= 0x1f, r == 0x85, r == 0xa0,
		r == 0x1680, r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029,
		r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

// pySpaceClass is isPySpace as a regexp character class body.
const pySpaceClass = `\t\n\v\f\r \x1c-\x1f\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

// isWord reports Python's `\w` for str patterns.
func isWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

func pyStrip(s string) string { return strings.TrimFunc(s, isPySpace) }

// collapseSpace is Python's re.sub(r"\s+", " ", s).strip().
func collapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pending := false
	for _, r := range s {
		if isPySpace(r) {
			pending = b.Len() > 0
			continue
		}
		if pending {
			b.WriteByte(' ')
			pending = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// truncRunes is Python's s[:n] on a str.
func truncRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// truthy is Python truthiness for decoded JSON values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case obj:
		return len(x) > 0
	}
	return true
}

// or is Python's `a or b`.
func or(a, b any) any {
	if truthy(a) {
		return a
	}
	return b
}

// sub is Python's `(m.get(k) or {})` for a dict-valued field.
func sub(m obj, k string) obj {
	v, _ := m[k].(obj)
	return v
}

func str(m obj, k string) (string, bool) {
	s, ok := m[k].(string)
	return s, ok
}

// strOr is Python's m.get(k, def) used as a string: an absent key yields def,
// JSON null or other falsy values yield "" (Session coerces them with `or ""`).
func strOr(m obj, k, def string) string {
	v, ok := m[k]
	if !ok {
		return def
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == 0 {
			return ""
		}
		return pyNum(x)
	case bool:
		if x {
			return "True"
		}
		return ""
	}
	return ""
}

// pyNum formats a JSON number the way Python would print the int it most
// likely was (JSON integers decode to float64 here).
func pyNum(x float64) string {
	if x == float64(int64(x)) && x < 1e16 && x > -1e16 {
		return strconv.FormatInt(int64(x), 10)
	}
	return strconv.FormatFloat(x, 'g', -1, 64)
}

// pyStem is pathlib's PurePath.stem (Python 3.14: a trailing dot is a suffix).
func pyStem(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

// pySuffix is pathlib's PurePath.suffix.
func pySuffix(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[i:]
	}
	return ""
}

// pyPath normalises a path the way pathlib.PurePosixPath does: collapse
// repeated slashes, drop "." components and trailing slashes, keep "..".
func pyPath(p string) string {
	if p == "" {
		return "."
	}
	lead := ""
	if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
		lead = "//"
	} else if strings.HasPrefix(p, "/") {
		lead = "/"
	}
	if !strings.Contains(p, "//") && !strings.Contains(p, "/./") && !strings.HasSuffix(p, "/") &&
		!strings.HasSuffix(p, "/.") && !strings.HasPrefix(p, "./") && p != "." {
		return p
	}
	parts := strings.Split(p, "/")
	kept := parts[:0]
	for _, part := range parts {
		if part != "" && part != "." {
			kept = append(kept, part)
		}
	}
	out := lead + strings.Join(kept, "/")
	if out == "" {
		return "."
	}
	return out
}

// pyJoin is pathlib's `a / b`: an absolute b replaces a.
func pyJoin(a, b string) string {
	if strings.HasPrefix(b, "/") {
		return pyPath(b)
	}
	if b == "" {
		return pyPath(a)
	}
	if a == "." || a == "" {
		return pyPath(b)
	}
	return pyPath(a + "/" + b)
}

// pyBase is pathlib's PurePath.name.
func pyBase(p string) string {
	p = pyPath(p)
	if p == "." || strings.TrimLeft(p, "/") == "" {
		return ""
	}
	return p[strings.LastIndexByte(p, '/')+1:]
}

// --------------------------------------------------------------- file reads

func readHead(path string, limit int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	n := limit
	if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() && fi.Size() < n {
		n = fi.Size()
	}
	buf := make([]byte, n)
	got, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil
	}
	return buf[:got]
}

// tailBytes returns at most the last `limit` bytes, minus the partial line the
// cut landed inside.
func tailBytes(path string, limit int64) []byte {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	size := fi.Size()
	if size <= limit {
		b, err := io.ReadAll(f)
		if err != nil {
			return nil
		}
		return b
	}
	if _, err := f.Seek(-limit, io.SeekEnd); err != nil {
		return nil
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	i := bytes.IndexByte(b, '\n')
	if i < 0 {
		return nil
	}
	return b[i+1:]
}

func loadJSON(path string) any {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(b) {
		return nil
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return v
}

// loadObj is Python's `load_json(path) or {}` used as a dict.
func loadObj(path string) obj {
	m, _ := loadJSON(path).(obj)
	return m
}

func stat(path string) (os.FileInfo, bool) {
	fi, err := os.Stat(path)
	return fi, err == nil
}

// pyMtime is os.stat().st_mtime: seconds + nanoseconds*1e-9, as CPython builds it.
func pyMtime(fi os.FileInfo) float64 {
	t := fi.ModTime()
	return float64(t.Unix()) + float64(t.Nanosecond())*1e-9
}

func mtime(path string) float64 {
	if fi, ok := stat(path); ok {
		return pyMtime(fi)
	}
	return 0
}

func sizeOf(path string) int64 {
	if fi, ok := stat(path); ok {
		return fi.Size()
	}
	return 0
}

// isDir is Path.is_dir (follows symlinks).
func isDir(path string) bool {
	fi, ok := stat(path)
	return ok && fi.IsDir()
}

// listNames lists a directory in on-disk order, like os.scandir.
func listNames(dir string) []string {
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer f.Close()
	names, _ := f.Readdirnames(-1)
	return names
}

// iterDirs lists the sub-directories of base (symlinks followed), unsorted.
func iterDirs(base string) []string {
	var out []string
	for _, n := range listNames(base) {
		p := pyJoin(base, n)
		if isDir(p) {
			out = append(out, p)
		}
	}
	return out
}

// globSuffix is Path.glob("*"+suffix): every entry kind, dotfiles included.
func globSuffix(dir, suffix string) []string {
	var out []string
	for _, n := range listNames(dir) {
		if strings.HasSuffix(n, suffix) {
			out = append(out, pyJoin(dir, n))
		}
	}
	return out
}

// ------------------------------------------------------------------- parsing

var asciiSpace = " \t\n\r\v\f"

// iterLines calls fn with each parsable JSON object line of a jsonl blob until
// fn returns false.
func iterLines(blob []byte, fn func(obj) bool) {
	for len(blob) > 0 {
		var line []byte
		if i := bytes.IndexByte(blob, '\n'); i >= 0 {
			line, blob = blob[:i], blob[i+1:]
		} else {
			line, blob = blob, nil
		}
		line = bytes.Trim(line, asciiSpace)
		if len(line) == 0 || line[0] != '{' || !utf8.Valid(line) {
			continue
		}
		var m obj
		if json.Unmarshal(line, &m) != nil || m == nil {
			continue
		}
		if !fn(m) {
			return
		}
	}
}

// iterLinesWith is iterLines restricted to lines containing one of needles.
// Callers pass the literal JSON string tokens every relevant line must hold;
// lines with \u escapes are always parsed since they may spell a needle.
func iterLinesWith(blob []byte, needles [][]byte, fn func(obj) bool) {
	for len(blob) > 0 {
		var line []byte
		if i := bytes.IndexByte(blob, '\n'); i >= 0 {
			line, blob = blob[:i], blob[i+1:]
		} else {
			line, blob = blob, nil
		}
		if !hasAny(line, needles) {
			continue
		}
		cont := true
		iterLines(line, func(m obj) bool { cont = fn(m); return cont })
		if !cont {
			return
		}
	}
}

var uEscape = []byte(`\u`)

func hasAny(line []byte, needles [][]byte) bool {
	for _, n := range needles {
		if bytes.Contains(line, n) {
			return true
		}
	}
	return bytes.Contains(line, uEscape)
}

func jsonlHead(path string, limit int64, fn func(obj) bool) {
	iterLines(readHead(path, limit), fn)
}

var jsonStrRes = func() map[string]*regexp.Regexp {
	m := map[string]*regexp.Regexp{}
	for _, k := range []string{"cwd", "sessionId", "lastUpdated", "startTime"} {
		m[k] = regexp.MustCompile(`"` + regexp.QuoteMeta(k) + `"[\t\n\v\f\r ]*:[\t\n\v\f\r ]*"((?:[^"\\]|\\.)*)"`)
	}
	return m
}()

// jsonStr pulls a plain JSON string value out of a raw byte blob.
func jsonStr(blob []byte, key string) (string, bool) {
	m := jsonStrRes[key].FindSubmatchIndex(blob)
	if m == nil {
		return "", false
	}
	raw := blob[m[2]-1 : m[3]+1] // include the surrounding quotes
	if !utf8.Valid(raw) {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// ------------------------------------------------------------------- titles

var noisePrefixes = [...]string{
	"caveat: the messages below",
	"## memory",
	"# agents.md",
	"# claude.md",
	"this session is being continued",
	"please continue the conversation from where",
}

var (
	commandRe = regexp.MustCompile(`<command-name>[` + pySpaceClass + `]*(.*?)[` + pySpaceClass + `]*</command-name>`)
	instrRe   = regexp.MustCompile(`^#+ .{0,40}instructions for /`)
)

// wrapperNames are the injected blocks that surround (or replace) the human's
// text; `local-command-\w+` is handled separately.
var wrapperNames = [...]string{
	"system-reminder", "recommended_plugins", "environment_context", "user_instructions",
	"ide_context", "ide_selection", "attachment", "system_context", "session_context",
}

// wrapperEnd matches `<(names)\b.*?</\1>` (re.S) at s[start] == '<' and returns
// the end of the match, or -1.
func wrapperEnd(s string, start int) int {
	p := start + 1
	rest := s[p:]
	nameEnd := -1
	for _, n := range wrapperNames {
		if strings.HasPrefix(rest, n) {
			nameEnd = p + len(n)
			if nameEnd < len(s) {
				if r, _ := utf8.DecodeRuneInString(s[nameEnd:]); isWord(r) {
					return -1 // \b fails
				}
			}
			break
		}
	}
	if nameEnd < 0 {
		const lc = "local-command-"
		if !strings.HasPrefix(rest, lc) {
			return -1
		}
		q := p + len(lc)
		k := q
		for k < len(s) {
			r, sz := utf8.DecodeRuneInString(s[k:])
			if !isWord(r) {
				break
			}
			k += sz
		}
		if k == q {
			return -1
		}
		// \w+ ends at a word boundary only when maximal, so no backtracking.
		nameEnd = k
	}
	closeTag := "</" + s[p:nameEnd] + ">"
	i := strings.Index(s[nameEnd:], closeTag)
	if i < 0 {
		return -1
	}
	return nameEnd + i + len(closeTag)
}

// removeWrappers is WRAPPER_RE.sub(" ", s).
func removeWrappers(s string) string {
	var b strings.Builder
	matched := false
	last, i := 0, 0
	for i < len(s) {
		j := strings.IndexByte(s[i:], '<')
		if j < 0 {
			break
		}
		start := i + j
		end := wrapperEnd(s, start)
		if end < 0 {
			i = start + 1
			continue
		}
		if !matched {
			b.Grow(len(s))
			matched = true
		}
		b.WriteString(s[last:start])
		b.WriteByte(' ')
		last, i = end, end
	}
	if !matched {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// lowerHasPrefix reports whether Python's s.lower().startswith(p) for ASCII p.
func lowerHasPrefix(s, p string) bool {
	i := 0
	for _, r := range s {
		if i == len(p) {
			return true
		}
		if r == 0x130 { // Python lowers İ to "i\u0307"
			return false
		}
		if unicode.ToLower(r) != rune(p[i]) {
			return false
		}
		i++
	}
	return i == len(p)
}

// noiseTag is NOISE_RE: `^<[a-zA-Z_][\w:-]*[\s>]`.
func noiseTag(t string) bool {
	if len(t) < 3 || t[0] != '<' {
		return false
	}
	if c := t[1]; !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
		return false
	}
	for _, r := range t[2:] {
		if isWord(r) || r == ':' || r == '-' {
			continue
		}
		return r == '>' || isPySpace(r)
	}
	return false
}

// CleanTitle normalises a candidate title; ok is false for machine noise.
func CleanTitle(text string) (string, bool) {
	// Transcript turns may contain megabytes of injected context. A title is a
	// label, not a transcript export: bound regex work and cached output.
	text = truncRunes(text, titleSourceLimit)
	if m := commandRe.FindStringSubmatch(text); m != nil {
		c := pyStrip(m[1])
		return c, c != ""
	}
	text = collapseSpace(removeWrappers(text))
	if text == "" {
		return "", false
	}
	for _, p := range noisePrefixes {
		if lowerHasPrefix(text, p) {
			return "", false
		}
	}
	if noiseTag(text) || instrRe.MatchString(text) {
		return "", false
	}
	return strings.TrimRightFunc(truncRunes(text, TitleDisplayLimit), isPySpace), true
}

// cleanAny is clean_title on an arbitrary JSON value.
func cleanAny(v any) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	return CleanTitle(s)
}

// cleanStr is clean_title(v) or "".
func cleanStr(v any) string {
	s, _ := cleanAny(v)
	return s
}

// titleStrip is Python's strip_wrappers: wrappers removed, whitespace collapsed.
func titleStrip(s string) string { return collapseSpace(removeWrappers(s)) }

// previewStrip removes wrapper blocks like titleStrip but keeps line structure
// so Markdown survives: CRLF becomes LF, trailing whitespace goes, runs of
// blank lines shrink to one, and leading/trailing blank lines are trimmed.
func previewStrip(s string) string {
	s = strings.ReplaceAll(removeWrappers(s), "\r\n", "\n")
	var b strings.Builder
	b.Grow(len(s))
	blank := false
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimRightFunc(line, isPySpace)
		if line == "" {
			blank = b.Len() > 0
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
			if blank {
				b.WriteByte('\n')
			}
		}
		blank = false
		b.WriteString(line)
	}
	return b.String()
}

// titlePick picks the most descriptive early user turn as a title fallback.
type titlePick struct{ best, fallback string }

func (p *titlePick) offer(text string, ok bool) bool {
	if !ok {
		return false
	}
	t, ok := CleanTitle(text)
	if !ok {
		return false
	}
	if strings.HasPrefix(t, "/") && utf8.RuneCountInString(t) < 24 && !strings.Contains(t, "\n") {
		if p.fallback == "" {
			p.fallback = t
		}
		return false
	}
	p.best = t
	return true
}

func (p *titlePick) offerAny(v any) bool {
	s, ok := v.(string)
	return p.offer(s, ok)
}

func (p *titlePick) value() string {
	if p.best != "" {
		return p.best
	}
	return p.fallback
}

// blocksToText flattens an Anthropic/OpenAI style content field.
func blocksToText(content any) (string, bool) {
	switch c := content.(type) {
	case string:
		return c, true
	case []any:
		var parts []string
		for _, b := range c {
			m, ok := b.(obj)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t != "text" && t != "input_text" {
				continue
			}
			if s, ok := m["text"].(string); ok {
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return strings.Join(parts, "\n"), true
	}
	return "", false
}
