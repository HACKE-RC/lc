package tui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The reader is the preview opened full-screen with `o`: scrollable like a
// pager, with `/` search, highlighted matches, and n/N to step through them.

type readerKeyMap struct {
	Down, Up, HalfDown, HalfUp, PageDown, PageUp, Top, Bottom key.Binding
	Find, Next, Prev, Resume, Path, Help, Close, Quit         key.Binding
}

func newReaderKeyMap() readerKeyMap {
	b := func(help, desc string, keys ...string) key.Binding {
		return key.NewBinding(key.WithKeys(keys...), key.WithHelp(help, desc))
	}
	return readerKeyMap{
		Down:     b("j/↓", "scroll down", "j", "down"),
		Up:       b("k/↑", "scroll up", "k", "up"),
		HalfDown: b("^d", "half page down", "ctrl+d"),
		HalfUp:   b("^u", "half page up", "ctrl+u"),
		PageDown: b("pgdn/^f", "page down", "pgdown", "ctrl+f", " "),
		PageUp:   b("pgup/^b", "page up", "pgup", "ctrl+b"),
		Top:      b("gg/home", "top", "home"),
		Bottom:   b("G/end", "bottom", "G", "end"),
		Find:     b("/", "search", "/"),
		Next:     b("n", "next match", "n"),
		Prev:     b("N", "previous match", "N"),
		Resume:   b("enter", "resume", "enter"),
		Path:     b("p", "print path", "p"),
		Help:     b("?", "help", "?"),
		Close:    b("o/q/esc", "back to list", "o", "q", "esc"),
		Quit:     b("^c", "quit", "ctrl+c"),
	}
}

func (k readerKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Down, k.Up, k.Find, k.Next, k.Prev, k.Close, k.Help}
}

func (k readerKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Down, k.Up, k.Top, k.Bottom},
		{k.HalfDown, k.HalfUp, k.PageDown, k.PageUp},
		{k.Find, k.Next, k.Prev},
		{k.Resume, k.Path, k.Help, k.Close, k.Quit},
	}
}

// match is one search hit, in rendered-line and display-cell coordinates.
type match struct{ line, start, end int }

func (m model) showFind() bool { return m.reading && (m.finding || m.find.Value() != "") }

// setPreview installs rendered preview content. An active search is re-run
// and stays on its current match (the content arrives late or re-renders
// on resize); otherwise the view starts at the latest turn.
func (m *model) setPreview(content string) {
	prev := m.matchIdx
	m.content = content
	m.findMatches()
	if len(m.matches) > 0 {
		m.matchIdx = min(prev, len(m.matches)-1)
		m.showMatch()
		return
	}
	m.highlight()
	m.vp.GotoBottom()
}

// findMatches locates every case-insensitive occurrence of the query in the
// visible text of the rendered preview. Matches never span wrapped lines.
func (m *model) findMatches() {
	m.matches, m.matchIdx = m.matches[:0], 0
	q := m.find.Value()
	if q == "" || m.content == "" {
		return
	}
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(q))
	for i, line := range strings.Split(m.content, "\n") {
		plain := ansi.Strip(line)
		for _, loc := range re.FindAllStringIndex(plain, -1) {
			start := ansi.StringWidth(plain[:loc[0]])
			m.matches = append(m.matches, match{i, start, start + ansi.StringWidth(plain[loc[0]:loc[1]])})
		}
	}
}

// highlight paints the matches into the viewport, the current one loudest.
func (m *model) highlight() {
	if len(m.matches) == 0 {
		m.vp.SetContent(m.content)
		return
	}
	lines := strings.Split(m.content, "\n")
	for i := 0; i < len(m.matches); {
		n := m.matches[i].line
		line, plain := lines[n], ansi.Strip(lines[n])
		var b strings.Builder
		pos := 0
		for ; i < len(m.matches) && m.matches[i].line == n; i++ {
			mt := m.matches[i]
			b.WriteString(ansi.Cut(line, pos, mt.start))
			b.WriteString(m.st.paintMatch(ansi.Cut(plain, mt.start, mt.end), i == m.matchIdx))
			pos = mt.end
		}
		b.WriteString(ansi.TruncateLeft(line, pos, ""))
		lines[n] = b.String()
	}
	m.vp.SetContent(strings.Join(lines, "\n"))
}

// jumpFrom selects the first match at or below `line`, wrapping to the top.
func (m *model) jumpFrom(line int) {
	if len(m.matches) == 0 {
		return
	}
	m.matchIdx = 0
	for i, mt := range m.matches {
		if mt.line >= line {
			m.matchIdx = i
			break
		}
	}
	m.showMatch()
}

func (m *model) step(delta int) {
	if len(m.matches) == 0 {
		return
	}
	m.matchIdx = (m.matchIdx + delta + len(m.matches)) % len(m.matches)
	m.showMatch()
}

// showMatch repaints the highlight and scrolls the current match into view,
// a third of the way down the page.
func (m *model) showMatch() {
	m.highlight()
	line := m.matches[m.matchIdx].line
	if line < m.vp.YOffset || line >= m.vp.YOffset+m.vp.Height {
		m.vp.SetYOffset(line - m.vp.Height/3)
	}
}

// resizeReader refits the viewport after the search bar appears or goes.
func (m *model) resizeReader() {
	m.sizeViewport(m.layout())
}

func (m model) openReader() (tea.Model, tea.Cmd) {
	if _, ok := m.selected(); !ok {
		return m, nil
	}
	m.reading = true
	m.pendingG = false
	m.help.ShowAll = false // each view starts with its short help
	cmd := m.syncPreview()
	return m, cmd
}

func (m model) closeReader() (tea.Model, tea.Cmd) {
	m.reading, m.finding = false, false
	m.help.ShowAll = false
	m.find.Blur()
	m.find.SetValue("")
	m.matches = m.matches[:0]
	cmd := m.syncPreview()
	return m, cmd
}

func (m model) readerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := m.rkeys
	if m.pendingG {
		m.pendingG = false
		if msg.String() == "g" {
			m.vp.GotoTop()
			return m, nil
		}
	}
	switch {
	case msg.String() == "g":
		m.pendingG = true
	case key.Matches(msg, k.Quit):
		return m, tea.Quit
	case msg.String() == "esc" && m.find.Value() != "":
		m.find.SetValue("")
		m.findMatches()
		m.highlight()
		m.resizeReader()
	case key.Matches(msg, k.Close):
		return m.closeReader()
	case key.Matches(msg, k.Down):
		m.vp.ScrollDown(1)
	case key.Matches(msg, k.Up):
		m.vp.ScrollUp(1)
	case key.Matches(msg, k.HalfDown):
		m.vp.HalfPageDown()
	case key.Matches(msg, k.HalfUp):
		m.vp.HalfPageUp()
	case key.Matches(msg, k.PageDown):
		m.vp.PageDown()
	case key.Matches(msg, k.PageUp):
		m.vp.PageUp()
	case key.Matches(msg, k.Top):
		m.vp.GotoTop()
	case key.Matches(msg, k.Bottom):
		m.vp.GotoBottom()
	case key.Matches(msg, k.Find):
		m.finding = true
		m.findOrigin = m.vp.YOffset
		m.find.SetValue("")
		m.findMatches()
		m.highlight()
		m.resizeReader()
		return m, m.find.Focus()
	case key.Matches(msg, k.Next):
		m.step(1)
	case key.Matches(msg, k.Prev):
		m.step(-1)
	case key.Matches(msg, k.Help):
		m.help.ShowAll = !m.help.ShowAll
		m.resizeReader()
	case key.Matches(msg, k.Resume, k.Path):
		s, _ := m.selected()
		m.result.Session = s
		m.result.Action = ActionResume
		if key.Matches(msg, k.Path) {
			m.result.Action = ActionPath
		}
		return m, tea.Quit
	}
	return m, nil
}

// findKey edits the search query, re-searching as you type from where the
// search started; enter keeps it for n/N and esc drops it.
func (m model) findKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		m.finding = false
		m.find.Blur()
		m.resizeReader()
		return m, nil
	case "esc":
		m.finding = false
		m.find.Blur()
		m.find.SetValue("")
		m.findMatches()
		m.highlight()
		m.resizeReader()
		m.vp.SetYOffset(m.findOrigin)
		return m, nil
	}
	before := m.find.Value()
	var cmd tea.Cmd
	m.find, cmd = m.find.Update(msg)
	if m.find.Value() != before {
		m.findMatches()
		if len(m.matches) > 0 {
			m.jumpFrom(m.findOrigin)
		} else {
			m.highlight()
			m.vp.SetYOffset(m.findOrigin)
		}
	}
	return m, cmd
}

// findBar is the search row at the bottom of the reader.
func (m model) findBar(width int) string {
	var status string
	switch q := m.find.Value(); {
	case q == "":
	case len(m.matches) == 0:
		status = m.st.dim.Italic(true).Render("no matches")
	default:
		status = m.st.muted.Render(fmt.Sprintf("%d/%d", m.matchIdx+1, len(m.matches)))
	}
	input := m.find.View()
	if !m.finding {
		input = m.st.dim.Render("/ ") + m.st.text.Render(m.find.Value())
	}
	input = ansi.Truncate(input, max(0, width-lipgloss.Width(status)-1), "")
	gap := max(1, width-lipgloss.Width(input)-lipgloss.Width(status))
	return input + strings.Repeat(" ", gap) + status
}
