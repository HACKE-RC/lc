package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/HACKE-RC/lc/internal/sessions"
	"github.com/HACKE-RC/lc/internal/theme"
)

func browser(t *testing.T, list ...sessions.Session) model {
	t.Helper()
	m := newModel(Options{Env: &sessions.Env{Home: "/home/u"}, Collect: sessions.Options{Root: "/repo"}}, theme.Mocha)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	next, _ = next.(model).Update(sessionsMsg{list: list})
	return next.(model)
}

func press(t *testing.T, m model, keys ...string) model {
	t.Helper()
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "ctrl+d":
			msg = tea.KeyMsg{Type: tea.KeyCtrlD}
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		next, _ := m.Update(msg)
		m = next.(model)
		_ = m.View()
	}
	return m
}

func TestCursorStaysInsideTheList(t *testing.T) {
	list := []sessions.Session{
		{Agent: "omp", ID: "a", Cwd: "/repo", Title: "first"},
		{Agent: "omp", ID: "b", Cwd: "/repo/api", Title: "second"},
		{Agent: "omp", ID: "c", Cwd: "/repo/web", Title: "third"},
	}
	cases := []struct {
		name string
		keys []string
		want string // selected ID
	}{
		{"up at the top", []string{"k", "k"}, "a"},
		{"down past the end", []string{"j", "j", "j", "j"}, "c"},
		{"half page past the end", []string{"ctrl+d", "ctrl+d"}, "c"},
		{"hiding the last row", []string{"G", "h"}, "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := press(t, browser(t, list...), tc.keys...)
			s, ok := m.selected()
			if !ok || s.ID != tc.want {
				t.Fatalf("selected %q (ok=%v), want %q", s.ID, ok, tc.want)
			}
		})
	}
}

func TestHidingEverySessionLeavesNoSelection(t *testing.T) {
	m := press(t, browser(t, sessions.Session{Agent: "omp", ID: "a", Cwd: "/repo/api"}), "h", "k", "j")
	if _, ok := m.selected(); ok {
		t.Fatal("expected no selection once every session is hidden")
	}
}

// reader opens the preview full-screen and delivers `content` as its render.
func reader(t *testing.T, content string) model {
	t.Helper()
	m := press(t, browser(t, sessions.Session{Agent: "omp", ID: "a", Cwd: "/repo", Title: "first"}), "o")
	if !m.reading || m.previewKey == "" {
		t.Fatalf("o did not open the reader (reading=%v key=%q)", m.reading, m.previewKey)
	}
	next, _ := m.Update(previewMsg{key: m.previewKey, content: content})
	return next.(model)
}

func TestReaderSearchStepsThroughMatches(t *testing.T) {
	m := reader(t, "alpha\nbeta Foo\ngamma\nfoo delta foo")
	// "o" inside the query is text, not the close key.
	m = press(t, m, "/", "f", "o", "o", "enter")
	if !m.reading || m.finding {
		t.Fatalf("reading=%v finding=%v after enter", m.reading, m.finding)
	}
	want := []match{{1, 5, 8}, {3, 0, 3}, {3, 10, 13}}
	if len(m.matches) != len(want) {
		t.Fatalf("matches %v, want %v", m.matches, want)
	}
	for i := range want {
		if m.matches[i] != want[i] {
			t.Fatalf("matches %v, want %v", m.matches, want)
		}
	}
	for _, step := range []struct {
		key  string
		want int
	}{{"n", 1}, {"n", 2}, {"n", 0}, {"N", 2}} {
		m = press(t, m, step.key)
		if m.matchIdx != step.want {
			t.Fatalf("after %s: match %d, want %d", step.key, m.matchIdx, step.want)
		}
	}
}

func TestReaderEscClearsSearchBeforeClosing(t *testing.T) {
	m := press(t, reader(t, "one foo\ntwo"), "/", "f", "o", "o", "enter", "esc")
	if !m.reading || m.find.Value() != "" || len(m.matches) != 0 {
		t.Fatalf("first esc: reading=%v query=%q matches=%v", m.reading, m.find.Value(), m.matches)
	}
	m = press(t, m, "esc")
	if m.reading {
		t.Fatal("second esc did not close the reader")
	}
}

func TestReaderSearchWithoutMatches(t *testing.T) {
	m := press(t, reader(t, "nothing here"), "/", "z", "q")
	if len(m.matches) != 0 || !m.finding || m.find.Value() != "zq" {
		t.Fatalf("finding=%v query=%q matches=%v", m.finding, m.find.Value(), m.matches)
	}
	if bar := m.findBar(40); !strings.Contains(bar, "no matches") {
		t.Fatalf("find bar %q lacks the no-match notice", bar)
	}
}

func sized(t *testing.T, m model, w, h int) model {
	t.Helper()
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(model)
}

func threeSessions() []sessions.Session {
	return []sessions.Session{
		{Agent: "omp", ID: "a", Cwd: "/repo", Title: "first", Size: 2048},
		{Agent: "codex", ID: "b", Cwd: "/repo/api", Title: "second"},
		{Agent: "claude", ID: "c", Cwd: "/repo/web", Title: "third"},
	}
}

// The footer must fit on one row and always keep the selection's position,
// whatever the width; bubbles' help alone overflows at some widths.
func TestFooterFitsAndKeepsPosition(t *testing.T) {
	for _, reading := range []bool{false, true} {
		for w := 30; w <= 140; w++ {
			m := sized(t, browser(t, threeSessions()...), w, 30)
			if reading {
				m = press(t, m, "o")
			}
			f := m.footer(true)
			if lipgloss.Height(f) != 1 || lipgloss.Width(f) > w || !strings.Contains(ansi.Strip(f), "1/3") {
				t.Fatalf("reading=%v width %d: footer %q", reading, w, ansi.Strip(f))
			}
		}
	}
}

// The whole screen must fit the terminal, help expanded or not, down to
// small heights; otherwise the header scrolls off.
func TestViewFitsTheTerminal(t *testing.T) {
	for _, keys := range [][]string{nil, {"?"}, {"o"}, {"o", "?"}} {
		for _, w := range []int{60, 140} {
			for h := 7; h <= 16; h++ {
				m := press(t, sized(t, browser(t, threeSessions()...), w, h), keys...)
				if got := lipgloss.Height(m.View()); got > h {
					t.Fatalf("keys %v at %dx%d: view is %d rows", keys, w, h, got)
				}
			}
		}
	}
}

func TestEscClearsAKeptFilterBeforeQuitting(t *testing.T) {
	m := press(t, browser(t, threeSessions()...), "/", "c", "o", "d", "enter")
	if len(m.view) != 1 {
		t.Fatalf("filter kept %d sessions, want 1", len(m.view))
	}
	m = press(t, m, "esc")
	if m.in.Value() != "" || len(m.view) != 3 {
		t.Fatalf("esc left query %q and %d sessions", m.in.Value(), len(m.view))
	}
	// Reopening the filter edits the kept query instead of discarding it.
	m = press(t, m, "/", "c", "enter", "/", "l")
	if m.in.Value() != "cl" {
		t.Fatalf("query %q, want %q", m.in.Value(), "cl")
	}
}

// A search typed before the preview has rendered must land on its match
// once the content arrives.
func TestSearchBeforeRenderLandsOnTheMatch(t *testing.T) {
	m := press(t, browser(t, threeSessions()...), "o", "/", "z", "z", "enter")
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = "line"
	}
	lines[20] = "the zz line"
	next, _ := m.Update(previewMsg{key: m.previewKey, content: strings.Join(lines, "\n")})
	m = next.(model)
	if len(m.matches) != 1 {
		t.Fatalf("matches %v", m.matches)
	}
	if off := m.vp.YOffset; 20 < off || 20 >= off+m.vp.Height {
		t.Fatalf("match on line 20 is off screen (offset %d, height %d)", off, m.vp.Height)
	}
}
