package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/HACKE-RC/lc/internal/sessions"
	"github.com/HACKE-RC/lc/internal/theme"
)

// Lines wider than the pane get wrapped again by the pane itself, which
// strands words on their own lines; every rendered line must fit.
func TestConversationFitsThePane(t *testing.T) {
	turn := strings.Join([]string{
		"In `lc -I`, pressing `o` now opens the selected session's preview full-screen. It's at `~/.local/bin/lc`; `go vet ./...` and `go test ./...` pass.",
		"- a bullet item that is long enough to wrap onto the next line, with `inline code` near the end",
		"  - a nested bullet with a https://example.com/a/really/long/url/without/any/spaces/in/it/at/all",
		"> a quoted line that is long enough to wrap onto another line inside the quote",
		"```go\nfunc main() {\n\tfmt.Println(\"a very long line of code that goes past the pane width\")\n}\n```",
		"tab\tseparated\tcolumns\tfrom\ta\ttool\tresult",
		"If you set `PI_CODING_AGENT_DIR`, it looks in `$XDG_DATA_HOME/omp` (profiles in `~/.omp/profiles/<name>/`) aren't scanned; `lc`, `src/lc/cli.py` and `go.mod` too.",
	}, "\n\n")
	s := sessions.Session{Agent: "claude"}
	md := newMarkdown(theme.Mocha)
	for width := 24; width <= 90; width++ {
		out := renderConversation(newStyles(theme.Mocha), md, s, []sessions.Turn{{Role: "assistant", Text: turn}}, true, nil, width)
		for _, line := range strings.Split(out, "\n") {
			if w := ansi.StringWidth(line); w > width {
				t.Errorf("width %d: line is %d wide: %q", width, w, ansi.Strip(line))
			}
			// A wrap between a code span and its punctuation strands the
			// punctuation at the start of the next line.
			if text := strings.TrimSpace(ansi.Strip(line)); strings.HasPrefix(text, ",") || strings.HasPrefix(text, ";") {
				t.Errorf("width %d: stranded punctuation: %q", width, text)
			}
		}
	}
}

func TestContinuationLinesHangUnderTheirBlock(t *testing.T) {
	out := ansi.Strip(newMarkdown(theme.Mocha).render("- one two three four five six seven\n\n> alpha beta gamma delta epsilon zeta", 20))
	want := []string{
		"• one two three four",
		"  five six seven",
		"",
		"│ alpha beta gamma",
		"│ delta epsilon zeta",
	}
	if got := strings.Split(out, "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Captured tool output can carry escape sequences; they must not reach the
// terminal from a preview or a title.
func TestTranscriptControlCharactersAreDropped(t *testing.T) {
	if got := cleanTurn("red \x1b[31mtext\x1b[0m\r\x07"); strings.ContainsAny(got, "\x1b\r\x07") {
		t.Fatalf("cleanTurn kept control characters: %q", got)
	}
	if got := (sessions.Session{Title: "fix \x1b[2Jthe\u009b screen"}).DisplayTitle(); got != "fix [2Jthe screen" {
		t.Fatalf("DisplayTitle = %q", got)
	}
}
