// Package cli is lc's command line: argument handling, the flat listing,
// JSON output, and handing off to the browser or a resumed agent.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
	flag "github.com/spf13/pflag"

	"github.com/HACKE-RC/lc/internal/sessions"
	"github.com/HACKE-RC/lc/internal/tui"
)

const Version = "0.3.0"

type args struct {
	path                     string
	all, ids, noSize, browse bool
	json, version            bool
	limit                    int
	agent, exceptAgent       []string
	exceptFolder, only       []string
	positional               []string
}

func parse(argv []string, stdout, stderr io.Writer) (*args, int, bool) {
	a := &args{}
	fs := flag.NewFlagSet("lc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.SortFlags = false
	var exceptAgents, exceptFolders, onlyAlias []string
	fs.StringVarP(&a.path, "path", "p", "", "use this `PATH` instead of the cwd")
	fs.BoolVarP(&a.all, "all", "a", false, "every repository, not just this one")
	fs.IntVarP(&a.limit, "limit", "n", 40, "show at most `N` rows (0 = all)")
	fs.StringArrayVar(&a.agent, "agent", nil, "only this `AGENT` (repeatable, comma-separated, or positional: lc codex claude)")
	fs.StringArrayVarP(&a.exceptAgent, "except-agent", "X", nil, "skip this `AGENT` (repeatable, comma-separated)")
	fs.StringArrayVar(&exceptAgents, "except-agents", nil, "")
	fs.StringArrayVarP(&a.exceptFolder, "except-folder", "x", nil, "skip sessions from `DIR`: subdir, absolute path, glob, or bare directory name (repeatable, comma-separated)")
	fs.StringArrayVar(&exceptFolders, "except-folders", nil, "")
	fs.StringArrayVarP(&a.only, "only-folder", "o", nil, "show only sessions from `DIR` (same pattern rules as --except-folder)")
	fs.StringArrayVar(&onlyAlias, "only", nil, "")
	fs.BoolVarP(&a.ids, "ids", "i", false, "show session ids")
	fs.BoolVar(&a.noSize, "no-size", false, "hide the transcript size column")
	fs.BoolVarP(&a.browse, "interactive", "I", false, "browse sessions in the interactive TUI")
	fs.BoolVar(&a.json, "json", false, "machine-readable output")
	fs.BoolVar(&a.version, "version", false, "print the version and exit")
	for _, alias := range []string{"except-agents", "except-folders", "only"} {
		_ = fs.MarkHidden(alias)
	}
	fs.Usage = func() {
		fmt.Fprintf(stdout, "usage: lc [options] [AGENT|PATH ...]\n\n"+
			"List coding-agent sessions for the current repository or a supplied directory.\n"+
			"Positional arguments are agent filters and/or one existing directory, e.g. `lc ~/ codex`.\n\n"+
			"options:\n%s", fs.FlagUsages())
	}
	argv, err := expandAbbreviations(fs, argv)
	if err != nil {
		fmt.Fprintf(stderr, "lc: %v (see lc --help)\n", err)
		return nil, 2, true
	}
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, 0, true
		}
		fmt.Fprintf(stderr, "lc: %v (see lc --help)\n", err)
		return nil, 2, true
	}
	a.exceptAgent = append(a.exceptAgent, exceptAgents...)
	a.exceptFolder = append(a.exceptFolder, exceptFolders...)
	a.only = append(a.only, onlyAlias...)
	a.positional = fs.Args()
	return a, 0, false
}

// expandAbbreviations rewrites unique prefixes of long options to the full
// name (`--inter` → `--interactive`), as Python's argparse accepted. A prefix
// matching several options is an error; flag values are never rewritten.
func expandAbbreviations(fs *flag.FlagSet, argv []string) ([]string, error) {
	names := []string{"help"}
	fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
	takesValue := func(f *flag.Flag) bool { return f != nil && f.NoOptDefVal == "" }
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			return append(out, argv[i:]...), nil
		case strings.HasPrefix(arg, "--"):
			name, value, inline := strings.Cut(arg[2:], "=")
			if name != "help" && fs.Lookup(name) == nil {
				var hits []string
				for _, n := range names {
					if strings.HasPrefix(n, name) {
						hits = append(hits, n)
					}
				}
				if len(hits) > 1 {
					return nil, fmt.Errorf("ambiguous option: --%s could match --%s", name, strings.Join(hits, ", --"))
				}
				if len(hits) == 1 {
					name = hits[0]
				}
			}
			if inline {
				out = append(out, "--"+name+"="+value)
				continue
			}
			out = append(out, "--"+name)
			if takesValue(fs.Lookup(name)) && i+1 < len(argv) {
				i++
				out = append(out, argv[i])
			}
		case len(arg) > 1 && arg[0] == '-':
			// A shorthand cluster like -ain1 or -an 1: the value of the
			// first value-taking flag is the rest of the cluster or the
			// next argument.
			out = append(out, arg)
			for j := 1; j < len(arg); j++ {
				if takesValue(fs.ShorthandLookup(arg[j : j+1])) {
					if j == len(arg)-1 && i+1 < len(argv) {
						i++
						out = append(out, argv[i])
					}
					break
				}
			}
		default:
			out = append(out, arg)
		}
	}
	return out, nil
}

// split expands repeatable, comma-separated flag values.
func split(specs []string) []string {
	var out []string
	for _, spec := range specs {
		for _, x := range strings.Split(spec, ",") {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, x)
			}
		}
	}
	return out
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Main runs lc and returns its exit status.
func Main(argv []string) int {
	stdout, stderr := os.Stdout, os.Stderr
	a, code, done := parse(argv, stdout, stderr)
	if done {
		return code
	}
	if a.version {
		fmt.Fprintf(stdout, "lc %s\n", Version)
		return 0
	}
	env := sessions.DefaultEnv()

	var agentsPos, paths []string
	for _, t := range a.positional {
		// A known agent always remains an agent filter; any other existing
		// directory is a positional scope, so `lc -I ~/` and `lc codex claude`
		// both read naturally.
		switch {
		case sessions.IsAgent(t):
			agentsPos = append(agentsPos, t)
		case isDir(expandHome(t, env.Home)):
			paths = append(paths, t)
		default:
			agentsPos = append(agentsPos, split([]string{t})...)
		}
	}
	if len(paths) > 1 {
		fmt.Fprintf(stderr, "lc: expected at most one positional path, got: %s\n", strings.Join(paths, ", "))
		return 2
	}
	if a.path != "" && len(paths) > 0 {
		fmt.Fprintln(stderr, "lc: use either -p/--path or a positional path, not both")
		return 2
	}
	start := "."
	if a.path != "" {
		start = a.path
	} else if len(paths) == 1 {
		start = paths[0]
	}
	start, err := filepath.Abs(expandHome(start, env.Home))
	if err != nil {
		fmt.Fprintf(stderr, "lc: %v\n", err)
		return 2
	}
	if resolved, err := filepath.EvalSymlinks(start); err == nil {
		start = resolved
	}
	root := sessions.RepoRoot(start)

	wanted, skipped := append(split(a.agent), agentsPos...), split(a.exceptAgent)
	var unknown []string
	for _, name := range append(append([]string{}, wanted...), skipped...) {
		if !sessions.IsAgent(name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		fmt.Fprintf(stderr, "lc: unknown agent(s): %s\n    known: %s\n", strings.Join(unknown, ", "), strings.Join(sessions.Agents, ", "))
		return 2
	}
	if len(wanted) == 0 {
		wanted = sessions.Agents
	}
	var agents []string
	for _, name := range wanted {
		if !slices.Contains(skipped, name) && !slices.Contains(agents, name) {
			agents = append(agents, name)
		}
	}
	if len(agents) == 0 {
		fmt.Fprintln(stderr, "lc: every agent was excluded")
		return 2
	}

	opt := sessions.Options{
		Root:    root,
		All:     a.all,
		Agents:  agents,
		SkipDir: sessions.FolderMatcher(root, split(a.exceptFolder), false),
		OnlyDir: sessions.FolderMatcher(root, split(a.only), true),
	}
	tty := isatty.IsTerminal(stdout.Fd())
	nowhere := func() int {
		where := "anywhere"
		if !a.all {
			where = sessions.Tildify(root, env.Home)
		}
		fmt.Fprintf(stdout, "no coding sessions found for %s\n", where)
		return 0
	}

	if a.browse && tty && !a.json {
		res, err := tui.Run(tui.Options{Env: env, Collect: opt, IDs: a.ids, ShowSize: !a.noSize})
		if err != nil {
			fmt.Fprintf(stderr, "lc: %v\n", err)
			return 1
		}
		for _, w := range res.Warnings {
			fmt.Fprintln(stderr, w)
		}
		if !res.Scanned {
			return 0
		}
		if res.Found == 0 {
			return nowhere()
		}
		dim := lipgloss.NewRenderer(stderr).NewStyle().Faint(true)
		if len(res.Hidden) > 0 && res.Action != tui.ActionResume {
			dirs := make([]string, len(res.Hidden))
			for i, d := range res.Hidden {
				dirs[i] = sessions.SubdirOf(d, root, env.Home)
			}
			fmt.Fprintln(stderr, dim.Render("to keep those hidden: lc -x "+strings.Join(dirs, ",")))
		}
		switch res.Action {
		case tui.ActionResume:
			return resume(res.Session, stdout, stderr)
		case tui.ActionPath:
			fmt.Fprintln(stdout, res.Session.Path)
		}
		return 0
	}

	list := sessions.Collect(env, opt, func(agent string, err error) {
		fmt.Fprintf(stderr, "lc: %s: %v\n", agent, err)
	})
	shown := list
	if a.limit > 0 && len(shown) > a.limit {
		shown = shown[:a.limit]
	}
	if a.json {
		return writeJSON(stdout, shown)
	}
	if len(list) == 0 {
		return nowhere()
	}
	printTable(stdout, shown, root, env.Home, a.ids, !a.noSize, tty)
	if tty {
		tail := fmt.Sprintf("%d sessions", len(list))
		if len(list) == 1 {
			tail = "1 session"
		}
		if !a.all {
			tail += " in " + sessions.Tildify(root, env.Home)
		}
		if more := len(list) - len(shown); more > 0 {
			tail += fmt.Sprintf(" (%d more, use -n 0)", more)
		}
		fmt.Fprintln(stdout, lipgloss.NewRenderer(stdout).NewStyle().Faint(true).Render(tail))
	}
	return 0
}

type jsonSession struct {
	Agent   string  `json:"agent"`
	ID      string  `json:"id"`
	Cwd     string  `json:"cwd"`
	Name    string  `json:"name"`
	Title   string  `json:"title"`
	Updated pyFloat `json:"updated"`
	Path    string  `json:"path"`
	Bytes   int64   `json:"bytes"`
	Note    string  `json:"note"`
}

// pyFloat marshals as Python's json does: always with a fraction, so a
// whole-second timestamp stays a float (1790416804.0) for strict consumers.
type pyFloat float64

func (f pyFloat) MarshalJSON() ([]byte, error) {
	b := strconv.AppendFloat(nil, float64(f), 'f', -1, 64)
	if !bytes.ContainsRune(b, '.') {
		b = append(b, ".0"...)
	}
	return b, nil
}

// writeJSON prints sessions byte-for-byte like Python's json.dump(indent=2),
// whose default ensure_ascii escapes every non-ASCII character.
func writeJSON(w io.Writer, list []sessions.Session) int {
	out := make([]jsonSession, len(list))
	for i, s := range list {
		out[i] = jsonSession{s.Agent, s.ID, s.Cwd, s.Title, s.Title, pyFloat(s.Updated), s.Path, s.Size, s.Note}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return 1
	}
	if _, err := w.Write(asciiJSON(buf.Bytes())); err != nil {
		return 1
	}
	return 0
}

// asciiJSON escapes non-ASCII runes as \uXXXX (UTF-16 surrogate pairs above
// the BMP). Non-ASCII can only occur inside JSON strings, so this is safe.
func asciiJSON(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, r := range string(b) {
		switch {
		case r < utf8.RuneSelf:
			out = append(out, byte(r))
		case r > 0xFFFF:
			r1, r2 := utf16.EncodeRune(r)
			out = fmt.Appendf(out, `\u%04x\u%04x`, r1, r2)
		default:
			out = fmt.Appendf(out, `\u%04x`, r)
		}
	}
	return out
}

// resume replaces lc with the agent that owns the session, started in the
// session's working directory because agents scope sessions by cwd.
func resume(s sessions.Session, stdout, stderr io.Writer) int {
	dim := lipgloss.NewRenderer(stdout).NewStyle().Faint(true)
	argv := sessions.ResumeCommand(s)
	var bin string
	why := "no resume-by-id support"
	if argv != nil {
		var err error
		if bin, err = exec.LookPath(argv[0]); err != nil {
			why = argv[0] + " not on PATH"
		}
	}
	if bin == "" {
		fmt.Fprintf(stdout, "%s\n%s\n", s.Path, dim.Render(fmt.Sprintf("(%s: %s)", s.Agent, why)))
		return 0
	}
	if err := os.Chdir(s.Cwd); err != nil {
		wd, _ := os.Getwd()
		fmt.Fprintf(stderr, "lc: %s is gone; resuming from %s\n", s.Cwd, wd)
	}
	fmt.Fprintln(stdout, dim.Render("$ "+strings.Join(argv, " ")))
	err := syscall.Exec(bin, argv, os.Environ())
	fmt.Fprintf(stderr, "lc: %s: %v\n", argv[0], err)
	return 1
}
