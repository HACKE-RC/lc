// Package tui is the interactive session browser: a filterable session list
// beside a Markdown-rendered preview of the selected transcript.
package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/HACKE-RC/lc/internal/sessions"
	"github.com/HACKE-RC/lc/internal/theme"
)

// Action is what the user chose when leaving the browser.
type Action int

const (
	ActionQuit Action = iota
	ActionResume
	ActionPath
)

type Options struct {
	Env      *sessions.Env
	Collect  sessions.Options
	IDs      bool
	ShowSize bool
}

type Result struct {
	Action   Action
	Session  sessions.Session
	Hidden   []string // directories hidden with `h`, in hiding order
	Scanned  bool     // false when the user quit before discovery finished
	Found    int      // sessions discovered before any browser-side filtering
	Warnings []string // per-store failures, reported after the screen is restored
}

const (
	minSplitCols   = 90 // below this the preview is too cramped to help
	minListCols    = 32
	minPreviewCols = 36
	itemRows       = 3 // title, meta, spacer
	previewDelay   = 70 * time.Millisecond
	previewHeader  = 3 // title, meta, divider
)

type (
	sessionsMsg struct {
		list     []sessions.Session
		warnings []string
	}
	previewTickMsg struct{ key string }
	previewMsg     struct{ key, content string }
)

type model struct {
	opt  Options
	st   styles
	md   *markdown
	keys keyMap
	help help.Model
	spin spinner.Model
	in   textinput.Model

	loading  bool
	ticking  bool // a spinner tick chain is in flight
	all      []sessions.Session
	hay      []string // lowercased "agent dir title" per session
	view     []int    // indices into all
	warnings []string

	cur, top int
	typing   bool
	hidden   []string
	ids      bool
	pendingG bool

	width, height int
	listPref      int // user-chosen list pane width; 0 = proportional default
	dragging      bool

	vp         viewport.Model
	previewKey string
	cache      map[string]string
	content    string // rendered preview behind the viewport, unhighlighted

	// reader: the preview opened full-screen with `o`
	rkeys      readerKeyMap
	reading    bool
	finding    bool // typing a search query
	find       textinput.Model
	findOrigin int // viewport offset when the search started
	matches    []match
	matchIdx   int

	result Result
}

// Run discovers sessions behind a spinner, then browses them.
func Run(opt Options) (Result, error) {
	m := newModel(opt, theme.For(lipgloss.DefaultRenderer()))
	final, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithInputTTY()).Run()
	if err != nil {
		return Result{}, err
	}
	fm := final.(model)
	fm.result.Hidden = fm.hidden
	fm.result.Scanned = !fm.loading
	fm.result.Found = len(fm.all)
	fm.result.Warnings = fm.warnings
	return fm.result, nil
}

func newModel(opt Options, p theme.Palette) model {
	st := newStyles(p)
	h := help.New()
	h.Styles.ShortKey = lipgloss.NewStyle().Foreground(lipgloss.Color(p.Lavender))
	h.Styles.ShortDesc = st.dim
	h.Styles.ShortSeparator = st.sep
	h.Styles.FullKey = h.Styles.ShortKey
	h.Styles.FullDesc = st.dim
	h.Styles.FullSeparator = st.sep
	h.Styles.Ellipsis = st.dim

	in := textinput.New()
	in.Prompt = "/ "
	in.PromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.Mauve)).Bold(true)
	in.TextStyle = st.text
	in.Placeholder = "agent, directory, or title"
	in.PlaceholderStyle = st.dim
	in.Cursor.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(p.Mauve))

	find := textinput.New()
	find.Prompt = in.Prompt
	find.PromptStyle = in.PromptStyle
	find.TextStyle = in.TextStyle
	find.Placeholder = "search this transcript"
	find.PlaceholderStyle = st.dim
	find.Cursor.Style = in.Cursor.Style

	return model{
		opt:     opt,
		st:      st,
		md:      newMarkdown(p),
		keys:    newKeyMap(),
		help:    h,
		spin:    spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(lipgloss.NewStyle().Foreground(lipgloss.Color(p.Mauve)))),
		in:      in,
		find:    find,
		rkeys:   newReaderKeyMap(),
		loading: true,
		ticking: true,
		ids:     opt.IDs,
		vp:      viewport.New(0, 0),
		cache:   map[string]string{},
	}
}

func (m model) Init() tea.Cmd {
	env, opt := m.opt.Env, m.opt.Collect
	collect := func() tea.Msg {
		var warnings []string
		list := sessions.Collect(env, opt, func(agent string, err error) {
			warnings = append(warnings, fmt.Sprintf("lc: %s: %v", agent, err))
		})
		return sessionsMsg{list, warnings}
	}
	return tea.Batch(m.spin.Tick, collect)
}

// ------------------------------------------------------------------ layout

type layout struct {
	split                 bool
	bodyH                 int
	listW, prevW          int // outer pane widths
	listInnerW, prevInner int
	innerH                int
	itemsY                int // screen row of the first list item
	perPage               int
}

func (m model) showFilter() bool { return m.typing || m.in.Value() != "" }

func (m model) layout() layout {
	var l layout
	l.bodyH = max(3, m.height-1-lipgloss.Height(m.footer(false)))
	l.innerH = max(1, l.bodyH-2)
	l.split = m.width >= minSplitCols && !m.reading
	switch {
	case m.reading:
		l.prevW = m.width
	case l.split:
		l.listW = clampList(m.listPref, m.width)
		l.prevW = m.width - l.listW - 1
	default:
		l.listW = m.width
	}
	l.listInnerW = max(1, l.listW-4)
	l.prevInner = max(1, l.prevW-4)
	listRows := l.innerH - 1
	l.itemsY = 3 // header, top border, pane heading
	if m.showFilter() {
		listRows--
		l.itemsY++
	}
	l.perPage = max(1, (listRows+1)/itemRows)
	return l
}

func clampList(pref, width int) int {
	if pref == 0 {
		pref = width * 2 / 5
	}
	return max(minListCols, min(pref, width-1-minPreviewCols))
}

// ------------------------------------------------------------------ state

// refresh recomputes the visible sessions: not buried by a hidden directory,
// and containing every whitespace-separated filter term.
func (m *model) refresh() {
	terms := strings.Fields(strings.ToLower(m.in.Value()))
	m.view = m.view[:0]
outer:
	for i, s := range m.all {
		if m.buried(s.Cwd) {
			continue
		}
		for _, t := range terms {
			if !strings.Contains(m.hay[i], t) {
				continue outer
			}
		}
		m.view = append(m.view, i)
	}
}

// buried reports whether a hidden directory covers cwd. Hiding hides the
// subtree, except at the repository root, where that would hide everything.
func (m *model) buried(cwd string) bool {
	root := m.opt.Collect.Root
	for _, d := range m.hidden {
		if cwd == d || (d != root && strings.HasPrefix(cwd, strings.TrimRight(d, "/")+"/")) {
			return true
		}
	}
	return false
}

// clamp bounds the cursor, then scrolls it into view. The cursor must be in
// range before layout(): measuring the footer reads the selected session.
func (m *model) clamp() {
	m.cur = min(max(m.cur, 0), max(0, len(m.view)-1))
	per := m.layout().perPage
	m.top = min(max(m.top, m.cur-per+1), m.cur)
	m.top = max(0, min(m.top, max(0, len(m.view)-per)))
}

func (m model) selected() (sessions.Session, bool) {
	if len(m.view) == 0 {
		return sessions.Session{}, false
	}
	return m.all[m.view[m.cur]], true
}

func (m model) subdir(cwd string) string {
	return sessions.SubdirOf(cwd, m.opt.Collect.Root, m.opt.Env.Home)
}

func (m model) previewPending() bool {
	_, ready := m.cache[m.previewKey]
	return m.previewKey != "" && !ready
}

// previewRows is the viewport height: the pane minus its header and, in the
// reader, the search bar.
func (m model) previewRows(l layout) int {
	rows := l.innerH - previewHeader
	if m.showFind() {
		rows--
	}
	return max(1, rows)
}

// sizeViewport fits the viewport to the layout, staying pinned to the
// bottom if it was there, so toggling help doesn't hide the latest turn.
func (m *model) sizeViewport(l layout) {
	bottom := m.vp.AtBottom()
	m.vp.Width, m.vp.Height = l.prevInner, m.previewRows(l)
	if bottom {
		m.vp.GotoBottom()
	} else {
		m.vp.SetYOffset(m.vp.YOffset)
	}
}

// fitInputs sizes the filter and search inputs to their panes, which move
// with resizes, [ / ], and splitter drags.
func (m *model) fitInputs(l layout) {
	m.in.Width = max(1, l.listInnerW-len(gutter)-lipgloss.Width(m.in.Prompt)-1)
	m.find.Width = max(1, l.prevInner-lipgloss.Width(m.find.Prompt)-12)
}

// syncPreview points the viewport at the selected session, rendering it after
// a short debounce so holding j does not parse every transcript it passes.
func (m *model) syncPreview() tea.Cmd {
	l := m.layout()
	m.sizeViewport(l)
	m.fitInputs(l)
	s, ok := m.selected()
	if (!l.split && !m.reading) || !ok {
		m.previewKey = ""
		return nil
	}
	k := fmt.Sprintf("%s\x00%s\x00%s\x00%d", s.Agent, s.ID, s.Path, l.prevInner)
	if k == m.previewKey {
		return nil
	}
	m.previewKey = k
	if content, hit := m.cache[k]; hit {
		m.setPreview(content)
		return nil
	}
	m.setPreview("")
	debounce := tea.Tick(previewDelay, func(time.Time) tea.Msg { return previewTickMsg{k} })
	if m.ticking {
		return debounce
	}
	m.ticking = true
	return tea.Batch(debounce, m.spin.Tick)
}

func (m model) loadPreview(k string) tea.Cmd {
	s, _ := m.selected()
	env, st, md, width := m.opt.Env, m.st, m.md, m.layout().prevInner
	return func() tea.Msg {
		turns, supported, err := sessions.Preview(env, s)
		return previewMsg{k, renderConversation(st, md, s, turns, supported, err, width)}
	}
}

// ------------------------------------------------------------------ update

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.Width = msg.Width
		m.clamp()
		cmd := m.syncPreview()
		return m, cmd

	case spinner.TickMsg:
		if !m.loading && !m.previewPending() {
			m.ticking = false
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case sessionsMsg:
		m.loading = false
		m.all, m.warnings = msg.list, msg.warnings
		m.hay = make([]string, len(m.all))
		for i, s := range m.all {
			m.hay[i] = strings.ToLower(s.Agent + " " + m.subdir(s.Cwd) + " " + s.Title)
		}
		if len(m.all) == 0 {
			return m, tea.Quit
		}
		m.refresh()
		m.clamp()
		cmd := m.syncPreview()
		return m, cmd

	case previewTickMsg:
		if msg.key != m.previewKey {
			return m, nil
		}
		return m, m.loadPreview(msg.key)

	case previewMsg:
		m.cache[msg.key] = msg.content
		if msg.key == m.previewKey {
			m.setPreview(msg.content)
		}
		return m, nil

	case tea.MouseMsg:
		return m.mouse(msg)

	case tea.KeyMsg:
		if m.loading {
			if key.Matches(msg, m.keys.Quit) || (msg.Type == tea.KeyRunes && slices.Contains(msg.Runes, 'q')) {
				return m, tea.Quit
			}
			return m, nil
		}
		if m.typing {
			return m.filterKey(msg)
		}
		if m.finding {
			return m.findKey(msg)
		}
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
			return m.keyBurst(msg.Runes)
		}
		return m.key(msg)
	}
	return m, nil
}

func (m model) filterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		m.typing = false
		m.in.Blur()
		m.clamp()
		cmd := m.syncPreview()
		return m, cmd
	case "esc":
		m.typing = false
		m.in.Blur()
		m.in.SetValue("")
		m.refresh()
		m.cur, m.top = 0, 0
		m.clamp()
		cmd := m.syncPreview()
		return m, cmd
	}
	before := m.in.Value()
	var cmd tea.Cmd
	m.in, cmd = m.in.Update(msg)
	if m.in.Value() != before {
		m.refresh()
		m.cur, m.top = 0, 0
		m.clamp()
		sync := m.syncPreview()
		return m, tea.Batch(cmd, sync)
	}
	return m, cmd
}

// keyBurst replays runes the terminal delivered in one read (fast typing,
// key repeat under load) as individual keystrokes. It stops at an exit key;
// once `/` opens the filter or search, the remaining runes become the query.
func (m model) keyBurst(runes []rune) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	for i, r := range runes {
		single := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
		next, cmd := m.key(single)
		m = next.(model)
		cmds = append(cmds, cmd)
		if key.Matches(single, m.keys.Quit, m.keys.Resume, m.keys.Path) {
			break
		}
		if m.typing || m.finding {
			if rest := runes[i+1:]; len(rest) > 0 {
				rest := tea.KeyMsg{Type: tea.KeyRunes, Runes: rest}
				if m.typing {
					next, cmd = m.filterKey(rest)
				} else {
					next, cmd = m.findKey(rest)
				}
				m = next.(model)
				cmds = append(cmds, cmd)
			}
			break
		}
	}
	return m, tea.Batch(cmds...)
}

func (m model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.reading {
		return m.readerKey(msg)
	}
	l := m.layout()
	if m.pendingG {
		m.pendingG = false
		if msg.String() == "g" {
			m.cur = 0
			m.clamp()
			cmd := m.syncPreview()
			return m, cmd
		}
	}
	k := m.keys
	switch {
	case msg.String() == "g":
		m.pendingG = true
		return m, nil
	case msg.String() == "esc" && m.in.Value() != "":
		m.in.SetValue("")
		m.refresh()
		m.cur, m.top = 0, 0
	case key.Matches(msg, k.Quit):
		return m, tea.Quit
	case key.Matches(msg, k.Open):
		return m.openReader()
	case key.Matches(msg, k.Down):
		m.cur++
	case key.Matches(msg, k.Up):
		m.cur--
	case key.Matches(msg, k.Top):
		m.cur = 0
	case key.Matches(msg, k.Bottom):
		m.cur = len(m.view) - 1
	case key.Matches(msg, k.HalfDown):
		m.cur += max(1, l.perPage/2)
	case key.Matches(msg, k.HalfUp):
		m.cur -= max(1, l.perPage/2)
	case key.Matches(msg, k.PageDown):
		m.cur += l.perPage
	case key.Matches(msg, k.PageUp):
		m.cur -= l.perPage
	case key.Matches(msg, k.PreviewDown):
		m.vp.ScrollDown(3)
		return m, nil
	case key.Matches(msg, k.PreviewUp):
		m.vp.ScrollUp(3)
		return m, nil
	case key.Matches(msg, k.Narrow, k.Widen):
		if !l.split {
			return m, nil
		}
		step := 4
		if key.Matches(msg, k.Narrow) {
			step = -4
		}
		m.listPref = clampList(l.listW+step, m.width)
	case key.Matches(msg, k.Filter):
		// Reopening the filter edits the kept query.
		m.typing = true
		m.in.CursorEnd()
		m.clamp()
		focus := m.in.Focus()
		sync := m.syncPreview()
		return m, tea.Batch(focus, sync)
	case key.Matches(msg, k.Hide):
		if s, ok := m.selected(); ok && !slices.Contains(m.hidden, s.Cwd) {
			m.hidden = append(m.hidden, s.Cwd)
			m.refresh()
		}
	case key.Matches(msg, k.Undo):
		if len(m.hidden) > 0 {
			m.hidden = m.hidden[:len(m.hidden)-1]
			m.refresh()
		}
	case key.Matches(msg, k.Unhide):
		if len(m.hidden) > 0 {
			m.hidden = m.hidden[:0]
			m.refresh()
		}
	case key.Matches(msg, k.IDs):
		m.ids = !m.ids
	case key.Matches(msg, k.Help):
		m.help.ShowAll = !m.help.ShowAll
	case key.Matches(msg, k.Resume, k.Path):
		if s, ok := m.selected(); ok {
			m.result.Session = s
			m.result.Action = ActionResume
			if key.Matches(msg, k.Path) {
				m.result.Action = ActionPath
			}
			return m, tea.Quit
		}
		return m, nil
	default:
		return m, nil
	}
	m.clamp()
	cmd := m.syncPreview()
	return m, cmd
}

func (m model) mouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.reading {
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelDown {
			m.vp.ScrollDown(3)
		} else if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelUp {
			m.vp.ScrollUp(3)
		}
		return m, nil
	}
	l := m.layout()
	onSplitter := l.split && msg.X >= l.listW-1 && msg.X <= l.listW+1 && msg.Y >= 1 && msg.Y <= l.bodyH
	switch {
	case msg.Action == tea.MouseActionRelease:
		m.dragging = false
		return m, nil
	case msg.Action == tea.MouseActionMotion && m.dragging:
		m.listPref = clampList(msg.X+1, m.width)
		m.clamp()
		cmd := m.syncPreview()
		return m, cmd
	case msg.Action != tea.MouseActionPress:
		return m, nil
	}
	inList := msg.X < l.listW
	switch msg.Button {
	case tea.MouseButtonWheelDown, tea.MouseButtonWheelUp:
		delta := 1
		if msg.Button == tea.MouseButtonWheelUp {
			delta = -1
		}
		if inList || m.loading {
			m.cur += delta
			m.clamp()
			cmd := m.syncPreview()
			return m, cmd
		}
		if delta > 0 {
			m.vp.ScrollDown(3)
		} else {
			m.vp.ScrollUp(3)
		}
		return m, nil
	case tea.MouseButtonLeft:
		if onSplitter {
			m.dragging = true
			return m, nil
		}
		row := msg.Y - l.itemsY
		if inList && row >= 0 && row%itemRows != itemRows-1 {
			if i := m.top + row/itemRows; i < len(m.view) && i < m.top+l.perPage {
				m.cur = i
				m.clamp()
				cmd := m.syncPreview()
				return m, cmd
			}
		}
	}
	return m, nil
}

// ------------------------------------------------------------------ view

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	l := m.layout()
	if m.reading {
		return lipgloss.JoinVertical(lipgloss.Left, m.header(), m.previewPane(l), m.footer(true))
	}
	body := m.listPane(l)
	if l.split {
		body = lipgloss.JoinHorizontal(lipgloss.Top, body, " ", m.previewPane(l))
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.header(), body, m.footer(true))
}

func (m model) header() string {
	root := m.opt.Collect.Root
	where := "all repositories"
	if !m.opt.Collect.All {
		where = sessions.Tildify(root, m.opt.Env.Home)
	}
	left := m.st.badge.Render("lc") + " " + m.st.headerPath.Render(where)
	if !m.loading {
		left += m.st.headerDim.Render(fmt.Sprintf("  %d sessions", len(m.all)))
	}
	var chips []string
	if m.loading {
		chips = append(chips, m.spin.View()+m.st.muted.Render(" scanning session stores"))
	}
	if q := m.in.Value(); q != "" && !m.typing {
		chips = append(chips, m.st.chip.Render("filter "+q))
	}
	if n := len(m.hidden); n > 0 {
		chips = append(chips, m.st.chip.Render(fmt.Sprintf("%d hidden · u undo", n)))
	}
	right := strings.Join(chips, " ")
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return ansi.Truncate(left, m.width, "…")
	}
	return left + strings.Repeat(" ", gap) + right
}

// gutter is the column the selection bar occupies; every list row starts
// after it so headings, the filter, and titles share one left edge.
const gutter = "  "

func (m model) listPane(l layout) string {
	w := l.listInnerW
	heading := gutter + m.st.paneTitle.Render("Sessions")
	if !m.loading {
		count := m.st.dim.Render(fmt.Sprintf("%d/%d", min(m.cur+1, len(m.view)), len(m.view)))
		heading += strings.Repeat(" ", max(1, w-lipgloss.Width(heading)-lipgloss.Width(count))) + count
	}
	lines := []string{heading}
	if m.showFilter() {
		lines = append(lines, gutter+ansi.Truncate(m.in.View(), w-len(gutter), ""))
	}
	switch {
	case m.loading:
		lines = append(lines, "", gutter+m.spin.View()+m.st.muted.Render(" Looking for sessions…"))
	case len(m.view) == 0:
		lines = append(lines, "", gutter+m.st.dim.Italic(true).Render("Nothing matches."))
	default:
		end := min(len(m.view), m.top+l.perPage)
		for i := m.top; i < end; i++ {
			lines = append(lines, m.item(m.all[m.view[i]], i == m.cur, w)...)
			if i < end-1 {
				lines = append(lines, "")
			}
		}
	}
	style := m.st.paneActive
	if m.typing {
		style = m.st.pane
	}
	return style.Width(l.listW - 2).Height(l.innerH).MaxHeight(l.bodyH).Render(clipRows(lines, l.innerH))
}

func (m model) item(s sessions.Session, selected bool, w int) []string {
	titleStyle, metaStyle := m.st.itemTitle, m.st.itemMeta
	bar := gutter
	if selected {
		titleStyle, metaStyle = m.st.itemTitleSel, m.st.itemMetaSel
		bar = lipgloss.NewStyle().Foreground(m.st.p.Agent(s.Agent)).Background(lipgloss.Color(m.st.p.Surface0)).Render("▌ ")
	}
	title := s.DisplayTitle()
	inner := w - len(gutter)
	line1 := titleStyle.Width(inner).Render(ansi.Truncate(title, inner, "…"))

	agent := metaStyle.Foreground(m.st.p.Agent(s.Agent)).Render("● " + s.Agent)
	meta := []string{agent}
	if dir := m.subdir(s.Cwd); dir != "." {
		meta = append(meta, dir)
	}
	meta = append(meta, sessions.RelAge(s.Updated, now())+" ago")
	if m.ids && s.ID != "" {
		meta = append(meta, s.ID)
	}
	rest := metaStyle.Render(" · " + strings.Join(meta[1:], " · "))
	line2 := ansi.Truncate(agent+rest, inner, metaStyle.Render("…"))
	if pad := inner - lipgloss.Width(line2); pad > 0 {
		line2 += metaStyle.Render(strings.Repeat(" ", pad))
	}
	return []string{bar + line1, bar + line2}
}

func (m model) previewPane(l layout) string {
	w := l.prevInner
	var lines []string
	s, ok := m.selected()
	switch {
	case m.loading:
		lines = []string{m.st.paneTitle.Render("Preview")}
	case !ok:
		lines = []string{m.st.paneTitle.Render("Preview"), "", m.st.dim.Italic(true).Render("Select a session to preview it.")}
	default:
		title := s.DisplayTitle()
		agent := lipgloss.NewStyle().Foreground(m.st.p.Agent(s.Agent)).Bold(true).Render(s.Agent)
		dir := m.subdir(s.Cwd)
		if dir == "." {
			dir = sessions.Tildify(s.Cwd, m.opt.Env.Home)
		}
		facts := []string{dir, "updated " + sessions.RelAge(s.Updated, now()) + " ago"}
		if m.opt.ShowSize && s.Size > 0 {
			facts = append(facts, sessions.HumanSize(s.Size))
		}
		if s.Note != "" {
			facts = append(facts, s.Note)
		}
		if m.ids && s.ID != "" {
			facts = append(facts, s.ID)
		}
		meta := agent + m.st.dim.Render(" · "+strings.Join(facts, " · "))
		lines = []string{
			m.st.previewTitle.Render(ansi.Truncate(title, w, "…")),
			ansi.Truncate(meta, w, "…"),
			m.st.divider.Render(strings.Repeat("─", w)),
		}
		if _, ready := m.cache[m.previewKey]; ready {
			lines = append(lines, m.vp.View())
		} else {
			lines = append(lines, m.spin.View()+m.st.muted.Render(" Rendering preview…"))
		}
		if m.showFind() {
			// Pin the search bar to the last row even while rendering.
			rows := lipgloss.Height(strings.Join(lines, "\n"))
			for ; rows < l.innerH-1; rows++ {
				lines = append(lines, "")
			}
			lines = append(lines, m.findBar(w))
		}
	}
	style := m.st.pane
	if m.reading {
		style = m.st.paneActive
	}
	return style.Width(l.prevW - 2).Height(l.innerH).MaxHeight(l.bodyH).Render(clipRows(lines, l.innerH))
}

// clipRows joins lines and keeps at most n rows, so a pane too short for its
// content loses content rather than its bottom border.
func clipRows(lines []string, n int) string {
	rows := strings.Split(strings.Join(lines, "\n"), "\n")
	return strings.Join(rows[:min(len(rows), max(1, n))], "\n")
}

// fullHelp reports whether the expanded help is shown: it is only when the
// terminal leaves room for the panes beneath it.
func (m model) fullHelp(keys help.KeyMap) bool {
	return m.help.ShowAll && m.height-1-(lipgloss.Height(m.help.View(keys))+1) >= 6
}

// footer renders key help on the left and the selection's facts on the
// right. With full=false it is only used for measuring height.
func (m model) footer(full bool) string {
	if m.typing {
		return m.st.dim.Render(" enter keep filter · esc clear · ctrl+c quit")
	}
	if m.finding {
		return m.st.dim.Render(" enter keep search · esc cancel · ctrl+c quit")
	}
	var keys help.KeyMap = m.keys
	if m.reading {
		keys = m.rkeys
	}
	var right string
	if s, ok := m.selected(); ok && !m.loading {
		facts := sessions.RelAge(s.Updated, now())
		if m.opt.ShowSize && s.Size > 0 {
			facts += " · " + sessions.HumanSize(s.Size)
		}
		right = m.st.facts.Render(facts) + m.st.position.Render(fmt.Sprintf("%d/%d", m.cur+1, len(m.view)))
	}
	h := m.help
	if m.fullHelp(keys) {
		return lipgloss.NewStyle().PaddingLeft(1).Render(h.View(keys)) + "\n" + lipgloss.PlaceHorizontal(m.width, lipgloss.Right, right)
	}
	// bubbles' help only sometimes honors Width, so truncate the line here.
	h.ShowAll, h.Width = false, 0
	left := ansi.Truncate(" "+h.View(keys), max(0, m.width-lipgloss.Width(right)-1), "…")
	if !full {
		return left
	}
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }
