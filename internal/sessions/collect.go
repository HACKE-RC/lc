package sessions

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Session is one discovered agent session.
type Session struct {
	Agent   string
	ID      string
	Cwd     string
	Title   string  // "" when unnamed
	Updated float64 // epoch seconds
	Path    string
	Size    int64
	Note    string
}

// DisplayTitle is the title as shown on a terminal: a placeholder when the
// session is unnamed, and without control characters, so a pasted prompt
// carrying escape sequences can't restyle or move the cursor.
func (s Session) DisplayTitle() string {
	if s.Title == "" {
		return "(unnamed session)"
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s.Title)
}

// Turn is one previewed conversation turn.
type Turn struct{ Role, Text string } // Role is "user" or "assistant"

// Env holds every filesystem root; tests construct it directly.
type Env struct {
	Home       string // $HOME
	CacheDir   string // $XDG_CACHE_HOME/lc or ~/.cache/lc ; Codex cache = CacheDir/codex-sessions.json
	DataHome   string // $XDG_DATA_HOME (may be "")
	PiAgentDir string // $PI_CODING_AGENT_DIR expanded, else Home/.pi/agent
}

// DefaultEnv reads the roots from the process environment.
func DefaultEnv() *Env {
	home := pyPath(homeDir())
	e := &Env{Home: home, DataHome: os.Getenv("XDG_DATA_HOME")}
	// Empty variables count as unset (the XDG rule): an empty
	// XDG_CACHE_HOME would otherwise put the cache under the cwd.
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		e.CacheDir = pyJoin(v, "lc")
	} else {
		e.CacheDir = pyJoin(home, ".cache/lc")
	}
	if v := os.Getenv("PI_CODING_AGENT_DIR"); v != "" {
		e.PiAgentDir = expandUser(pyPath(v))
	} else {
		e.PiAgentDir = pyJoin(home, ".pi/agent")
	}
	return e
}

func homeDir() string {
	if h, ok := os.LookupEnv("HOME"); ok {
		return h
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return ""
}

// expandUser is os.path.expanduser.
func expandUser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	i := strings.IndexByte(p, '/')
	if i < 0 {
		i = len(p)
	}
	var h string
	if i == 1 {
		h = homeDir()
	} else {
		u, err := user.Lookup(p[1:i])
		if err != nil {
			return p
		}
		h = u.HomeDir
	}
	out := strings.TrimRight(h, "/") + p[i:]
	if out == "" {
		return "/"
	}
	return out
}

// Agents lists every supported agent in discovery order.
var Agents = []string{"claude", "codex", "droid", "opencode", "cursor", "copilot", "grok", "kimi", "pi", "omp", "gemini"}

// IsAgent reports whether name is a supported agent.
func IsAgent(name string) bool { return slices.Contains(Agents, name) }

// Options selects which sessions Collect returns.
type Options struct {
	Root    string
	All     bool
	Agents  []string              // already validated/filtered
	SkipDir func(cwd string) bool // may be nil
	OnlyDir func(cwd string) bool // may be nil
}

type adapter func(env *Env, keep, dirOK func(string) bool, add func(Session))

var adapters = map[string]adapter{
	"claude":   aClaude,
	"codex":    aCodex,
	"droid":    aDroid,
	"opencode": aOpencode,
	"copilot":  aCopilot,
	"grok":     aGrok,
	"kimi":     aKimi,
	"pi":       aPi,
	"omp":      aOmp,
}

type candAdapter func(env *Env, keep func(string) bool, cands []string, add func(Session))

// runAdapter isolates one store: a broken store must not kill the listing.
// Sessions yielded before a failure are kept, as Python's list.extend does.
func runAdapter(name string, warn func(string, error), fn func()) {
	defer func() {
		if r := recover(); r != nil && warn != nil {
			if err, ok := r.(error); ok {
				warn(name, err)
			} else {
				warn(name, fmt.Errorf("%v", r))
			}
		}
	}()
	fn()
}

// Collect lists the sessions of opt.Agents, deduplicated, newest first.
func Collect(env *Env, opt Options, warn func(agent string, err error)) []Session {
	root := opt.Root
	base := strings.TrimRight(root, "/")
	keep := func(cwd string) bool {
		if !opt.All && cwd != root && !strings.HasPrefix(cwd, base+"/") {
			return false
		}
		if opt.SkipDir != nil && opt.SkipDir(cwd) {
			return false
		}
		return opt.OnlyDir == nil || opt.OnlyDir(cwd)
	}
	dirOK := func(string) bool { return true }
	if !opt.All {
		dirOK = dirMatcher(root)
	}

	var sessions []Session
	add := func(s Session) { sessions = append(sessions, s) }
	for _, name := range opt.Agents {
		if fn, ok := adapters[name]; ok {
			runAdapter(name, warn, func() { fn(env, keep, dirOK, add) })
		}
	}

	// cursor and gemini key their stores by a hash of the cwd, so they can only
	// be looked up from candidate paths: the repo root, its immediate children,
	// and every cwd the other agents reported.
	set := map[string]struct{}{root: {}}
	for _, s := range sessions {
		if strings.HasPrefix(s.Cwd, "/") {
			set[s.Cwd] = struct{}{}
		}
	}
	for _, d := range iterDirs(pyPath(root)) {
		set[d] = struct{}{}
	}
	if opt.All {
		for _, d := range iterDirs(env.Home) {
			set[d] = struct{}{}
		}
		set[env.Home] = struct{}{}
	}
	cands := make([]string, 0, len(set))
	for c := range set {
		cands = append(cands, c)
	}
	sort.Strings(cands)
	for _, name := range [...]string{"cursor", "gemini"} {
		if !slices.Contains(opt.Agents, name) {
			continue
		}
		fn := candAdapter(aCursor)
		if name == "gemini" {
			fn = aGemini
		}
		runAdapter(name, warn, func() { fn(env, keep, cands, add) })
	}

	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].Updated > sessions[j].Updated })
	type key struct{ agent, id string }
	seen := make(map[key]struct{}, len(sessions))
	out := sessions[:0]
	for _, s := range sessions {
		// Claude/droid transcripts that never recorded a cwd carry their
		// store directory name instead; adapters can't judge those with
		// keep(), but --only-folder/--except-folder still have to apply.
		if !strings.HasPrefix(s.Cwd, "/") &&
			((opt.OnlyDir != nil && !opt.OnlyDir(s.Cwd)) || (opt.SkipDir != nil && opt.SkipDir(s.Cwd))) {
			continue
		}
		k := key{s.Agent, s.ID}
		if k.id == "" {
			k.id = s.Path
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	return out
}

// RepoRoot returns the git toplevel containing start, or start itself.
func RepoRoot(start string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", start, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return start
	}
	if s := pyStrip(string(out)); s != "" {
		return pyPath(s)
	}
	return start
}

// dirMatcher matches session-store directory names that mangle a path into
// one token: Claude Code and droid replace every non-alphanumeric character
// of the cwd with '-', so compare positionally.
func dirMatcher(root string) func(string) bool {
	pat := []rune(root)
	return func(name string) bool {
		i := 0
		for _, want := range pat {
			if i >= len(name) {
				return false
			}
			r, sz := utf8.DecodeRuneInString(name[i:])
			if isAlnum(want) {
				if r != want {
					return false
				}
			} else if isASCIIAlnum(r) {
				return false
			}
			i += sz
		}
		if i == len(name) {
			return true
		}
		r, _ := utf8.DecodeRuneInString(name[i:])
		return !isASCIIAlnum(r)
	}
}

func isAlnum(r rune) bool { return r != '_' && isWord(r) }

func isASCIIAlnum(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

// fnmatchRe translates a shell pattern exactly as Python's fnmatch.translate
// does: `*` crosses '/', `[!...]` negates, unclosed '[' is literal.
func fnmatchRe(pat string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`\A(?s:`)
	rs := []rune(pat)
	i, n := 0, len(rs)
	for i < n {
		c := rs[i]
		i++
		switch c {
		case '*':
			for i < n && rs[i] == '*' {
				i++
			}
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			j := i
			if j < n && rs[j] == '!' {
				j++
			}
			if j < n && rs[j] == ']' {
				j++
			}
			for j < n && rs[j] != ']' {
				j++
			}
			if j >= n {
				b.WriteString(`\[`)
				continue
			}
			stuff := string(rs[i:j])
			if !strings.Contains(stuff, "-") {
				stuff = strings.ReplaceAll(stuff, `\`, `\\`)
			} else {
				var chunks [][]rune
				k := i + 1
				if rs[i] == '!' {
					k = i + 2
				}
				for {
					f := -1
					for q := k; q < j; q++ {
						if rs[q] == '-' {
							f = q
							break
						}
					}
					if f < 0 {
						break
					}
					chunks = append(chunks, rs[i:f:f])
					i = f + 1
					k = f + 3
				}
				if chunk := rs[i:j:j]; len(chunk) > 0 {
					chunks = append(chunks, chunk)
				} else {
					chunks[len(chunks)-1] = append(chunks[len(chunks)-1], '-')
				}
				// Remove empty ranges -- invalid in RE.
				for k := len(chunks) - 1; k > 0; k-- {
					prev, cur := chunks[k-1], chunks[k]
					if len(prev) > 0 && len(cur) > 0 && prev[len(prev)-1] > cur[0] {
						merged := append(append([]rune{}, prev[:len(prev)-1]...), cur[1:]...)
						chunks[k-1] = merged
						chunks = append(chunks[:k], chunks[k+1:]...)
					}
				}
				parts := make([]string, len(chunks))
				for k, ch := range chunks {
					s := strings.ReplaceAll(string(ch), `\`, `\\`)
					parts[k] = strings.ReplaceAll(s, "-", `\-`)
				}
				stuff = strings.Join(parts, "-")
			}
			i = j + 1
			switch {
			case stuff == "":
				b.WriteString(`[^\x00-\x{10FFFF}]`) // empty range: never match
			case stuff == "!":
				b.WriteString(".") // negated empty range: any character
			default:
				var e strings.Builder
				for _, r := range stuff {
					if r == '&' || r == '~' || r == '|' {
						e.WriteByte('\\')
					}
					e.WriteRune(r)
				}
				stuff = e.String()
				if stuff[0] == '!' {
					stuff = "^" + stuff[1:]
				} else if stuff[0] == '^' || stuff[0] == '[' {
					stuff = `\` + stuff
				}
				b.WriteString("[" + stuff + "]")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`)\z`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return regexp.MustCompile(`[^\x00-\x{10FFFF}]`)
	}
	return re
}

// FolderMatcher builds a `cwd -> bool` predicate shared by --except-folder and
// --only-folder. A pattern may be a subdirectory (`ws/r5`, `./ws/r5`), an
// absolute path, a glob (`ws/*`, `*-old`), a bare name matching any path
// component, or `.` for sessions started at the repo root itself. `empty` is
// the result when no patterns were given.
func FolderMatcher(root string, patterns []string, empty bool) func(cwd string) bool {
	base := strings.TrimRight(root, "/")
	type pattern struct {
		p       string
		abs     string         // expanded absolute path
		re, sub *regexp.Regexp // p and p+"/*"
	}
	var norm []pattern
	for _, raw := range patterns {
		p := strings.TrimRight(pyStrip(raw), "/")
		p = strings.TrimPrefix(p, "./")
		if p == "" {
			continue
		}
		pt := pattern{p: p}
		switch {
		case p == ".":
		case strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~"):
			pt.abs = strings.TrimRight(expandUser(p), "/")
		default:
			pt.re, pt.sub = fnmatchRe(p), fnmatchRe(p+"/*")
		}
		norm = append(norm, pt)
	}
	if len(norm) == 0 {
		return func(string) bool { return empty }
	}
	return func(cwd string) bool {
		var rel string
		switch {
		case strings.HasPrefix(cwd, base+"/"):
			rel = cwd[len(base)+1:]
		case cwd == base:
			rel = ""
		default:
			rel = cwd
		}
		var parts []string
		for _, s := range strings.Split(rel, "/") {
			if s != "" {
				parts = append(parts, s)
			}
		}
		for _, pt := range norm {
			switch {
			case pt.p == ".":
				if len(parts) == 0 {
					return true
				}
			case pt.re == nil:
				if cwd == pt.abs || strings.HasPrefix(cwd, pt.abs+"/") {
					return true
				}
			default:
				if rel == pt.p || strings.HasPrefix(rel, pt.p+"/") || pt.re.MatchString(rel) || pt.sub.MatchString(rel) {
					return true
				}
				for _, part := range parts {
					if pt.re.MatchString(part) {
						return true
					}
				}
			}
		}
		return false
	}
}

// SubdirOf renders cwd relative to root: ".", "./sub", or ~-abbreviated.
func SubdirOf(cwd, root, home string) string {
	base := strings.TrimRight(root, "/")
	if cwd == root {
		return "."
	}
	if strings.HasPrefix(cwd, base+"/") {
		return "./" + cwd[len(base)+1:]
	}
	if cwd == "" {
		return "?"
	}
	return Tildify(cwd, home)
}

// Tildify abbreviates home to ~ when path is home or lies inside it.
func Tildify(path, home string) string {
	if home != "" && home != "/" && (path == home || strings.HasPrefix(path, home+"/")) {
		return "~" + path[len(home):]
	}
	return path
}

// RelAge renders the age of ts at now, e.g. "5m", "3d", "2mo".
func RelAge(ts, now float64) string {
	if ts <= 0 {
		return "?"
	}
	d := now - ts
	if !(d > 0) {
		d = 0
	}
	cuts := [...]struct {
		cut, div float64
		unit     string
	}{{90, 1, "s"}, {5400, 60, "m"}, {172800, 3600, "h"}, {1209600, 86400, "d"}, {7776000, 604800, "w"}}
	for _, c := range cuts {
		if d < c.cut {
			return fmt.Sprintf("%d%s", int64(d/c.div), c.unit)
		}
	}
	v := d / 2592000
	if math.IsInf(v, 0) || v >= 1<<63 {
		return "?"
	}
	return fmt.Sprintf("%dmo", int64(v))
}

// HumanSize renders a byte count, e.g. "4.2K", "17M".
func HumanSize(n int64) string {
	if n <= 0 {
		return "-"
	}
	for _, u := range [...]struct {
		unit string
		div  int64
	}{{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if n >= u.div {
			v := float64(n) / float64(u.div)
			if v < 10 {
				return fmt.Sprintf("%.1f%s", v, u.unit)
			}
			return fmt.Sprintf("%.0f%s", v, u.unit)
		}
	}
	return fmt.Sprintf("%dB", n)
}

// How each agent reopens a session; {id} is the session id. Gemini resumes by
// index rather than id, so it has no entry.
var resume = map[string][]string{
	"claude":   {"claude", "--resume", "{id}"},
	"codex":    {"codex", "resume", "{id}"},
	"droid":    {"droid", "--resume", "{id}"},
	"opencode": {"opencode", "--session", "{id}"},
	"cursor":   {"cursor-agent", "--resume", "{id}"},
	"copilot":  {"copilot", "--resume", "{id}"},
	"grok":     {"grok", "--resume", "{id}"},
	"kimi":     {"kimi", "--session", "{id}"},
	"pi":       {"pi", "--session", "{id}"},
	"omp":      {"omp", "--resume", "{id}"},
}

// ResumeCommand returns the argv that reopens s, or nil if unsupported.
func ResumeCommand(s Session) []string {
	tpl := resume[s.Agent]
	if tpl == nil {
		return nil
	}
	argv := make([]string, len(tpl))
	for i, part := range tpl {
		argv[i] = strings.ReplaceAll(part, "{id}", s.ID)
	}
	return argv
}
