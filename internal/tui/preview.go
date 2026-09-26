package tui

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/HACKE-RC/lc/internal/sessions"
	"github.com/HACKE-RC/lc/internal/theme"
)

// turnRuneLimit bounds glamour work per turn: a preview is a glimpse, and a
// single pasted log can be megabytes.
const turnRuneLimit = 12_000

// markdown renders turns with glamour for block structure and styling, then
// wraps them itself. glamour's own word wrap mis-measures styled inline
// spans: it overflows the requested width and strands single words on
// their own lines. glamour's renderer keeps per-render state, so rendering
// is serialized.
type markdown struct {
	mu sync.Mutex
	r  *glamour.TermRenderer
}

func newMarkdown(p theme.Palette) *markdown {
	// Word wrap 0 renders every block as one unpadded line.
	r, _ := glamour.NewTermRenderer(glamour.WithStyles(markdownStyle(p)), glamour.WithWordWrap(0), glamour.WithColorProfile(lipgloss.ColorProfile()))
	return &markdown{r: r}
}

func (md *markdown) render(text string, width int) string {
	if md.r == nil {
		return ansi.Wrap(text, width, "")
	}
	md.mu.Lock()
	out, err := md.r.Render(text)
	md.mu.Unlock()
	if err != nil {
		return ansi.Wrap(text, width, "")
	}
	return reflow(out, width)
}

// blockPrefix matches what glamour puts before a block's text: indentation,
// quote bars, list markers, task boxes, and the heading bar.
var blockPrefix = regexp.MustCompile(`^ *(?:(?:│ )+|• |\d+[.)] |[✓○] |▍ )?`)

// reflow wraps rendered Markdown to width. Continuation lines hang under
// the block's text (quote bars repeat), runs of blank lines collapse, and
// the leading/trailing blank lines glamour adds are dropped.
func reflow(rendered string, width int) string {
	var out []string
	blank := false
	for _, line := range strings.Split(rendered, "\n") {
		plain := ansi.Strip(line)
		if strings.TrimSpace(plain) == "" {
			blank = len(out) > 0
			continue
		}
		if blank {
			out = append(out, "")
			blank = false
		}
		if ansi.StringWidth(plain) <= width {
			out = append(out, line)
			continue
		}
		indent := ansi.StringWidth(blockPrefix.FindString(plain))
		if indent > width/2 {
			indent = 0
		}
		prefix, cont := ansi.Cut(line, 0, indent), strings.Repeat(" ", indent)
		if strings.Contains(ansi.Strip(prefix), "│") {
			cont = prefix
		}
		body := carryStyles(strings.Split(ansi.Wrap(ansi.TruncateLeft(line, indent, ""), width-indent, ""), "\n"))
		for i, l := range body {
			// ansi.Wrap can leave a styled span's trailing space one column
			// past the limit; that space is the only thing cut here.
			if ansi.StringWidth(l) > width-indent {
				l = ansi.Truncate(l, width-indent, "")
			}
			if i == 0 {
				out = append(out, prefix+l)
			} else {
				out = append(out, cont+l)
			}
		}
	}
	return strings.Join(out, "\n")
}

var sgr = regexp.MustCompile(`\x1b\[([0-9;:]*)m`)

// carryStyles makes each wrapped line self-contained: styles still open at a
// line break are reset there and re-opened on the next line, so a code span
// split across lines doesn't bleed into the pane border.
func carryStyles(lines []string) []string {
	var open []string
	for i, l := range lines {
		reopen := strings.Join(open, "")
		for _, m := range sgr.FindAllStringSubmatch(l, -1) {
			if strings.Trim(m[1], "0;:") == "" {
				open = open[:0]
			} else {
				open = append(open, m[0])
			}
		}
		l = reopen + strings.TrimRight(l, " ")
		if len(open) > 0 {
			l += "\x1b[0m"
		}
		lines[i] = l
	}
	return lines
}

// cleanTurn bounds a turn's length and makes it safe to put on screen: tabs
// become spaces (a tab's width depends on the column it lands in, so it
// can't be measured), and other control characters, including ESC from
// captured tool output, are dropped so transcripts can't drive the terminal.
func cleanTurn(text string) string {
	if r := []rune(text); len(r) > turnRuneLimit {
		text = string(r[:turnRuneLimit]) + "\n\n*… truncated*"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0:
			return -1
		}
		return r
	}, strings.ReplaceAll(text, "\t", "    "))
}

// renderConversation lays out turns as a chat log for a pane `width` wide.
func renderConversation(st styles, md *markdown, s sessions.Session, turns []sessions.Turn, supported bool, err error, width int) string {
	notice := func(msg string) string {
		return st.dim.Italic(true).Width(width).Render(msg)
	}
	switch {
	case !supported:
		return notice(fmt.Sprintf("%s transcripts can't be previewed — press p to print the path.", s.Agent))
	case err != nil:
		return notice("Preview failed: " + err.Error())
	case len(turns) == 0:
		return notice("No messages to preview.")
	}
	bodyWidth := max(8, width-2)
	agent := lipgloss.NewStyle().Bold(true).Foreground(st.p.Agent(s.Agent))
	blocks := make([]string, 0, len(turns))
	for _, t := range turns {
		var head string
		if t.Role == "user" {
			head = st.roleUser.Render("● you")
		} else {
			head = agent.Render("◆ " + s.Agent)
		}
		body := md.render(cleanTurn(t.Text), bodyWidth)
		body = "  " + strings.ReplaceAll(body, "\n", "\n  ")
		blocks = append(blocks, head+"\n"+body)
	}
	return strings.Join(blocks, "\n\n")
}
