package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"

	"github.com/HACKE-RC/lc/internal/sessions"
	"github.com/HACKE-RC/lc/internal/theme"
)

// printTable writes the flat, grep-friendly listing. Colors follow the
// terminal (none when piped or with NO_COLOR); columns only appear when they
// carry information.
func printTable(w *os.File, list []sessions.Session, root, home string, ids, sizes, tty bool) {
	r := lipgloss.NewRenderer(w)
	p := theme.Mocha
	if tty {
		p = theme.For(r)
	}
	now := float64(time.Now().UnixNano()) / 1e9
	showDir := false
	for _, s := range list {
		if s.Cwd != root {
			showDir = true
			break
		}
	}
	heads := []string{"AGENT", "AGE", "SIZE", "ID", "DIR", "TITLE"}
	keep := []bool{true, true, sizes, ids, showDir, true}
	rows := make([][]string, len(list))
	for i, s := range list {
		dir := ""
		if showDir {
			dir = sessions.SubdirOf(s.Cwd, root, home)
		}
		title := s.DisplayTitle()
		rows[i] = []string{s.Agent, sessions.RelAge(s.Updated, now), sessions.HumanSize(s.Size), s.ID, dir, title}
	}
	widths := make([]int, len(heads))
	fixed := 0
	for c, h := range heads {
		if !keep[c] {
			continue
		}
		widths[c] = ansi.StringWidth(h)
		for _, row := range rows {
			widths[c] = max(widths[c], ansi.StringWidth(row[c]))
		}
		if c < len(heads)-1 {
			fixed += widths[c] + 2
		}
	}
	total := 100
	if cols, _, err := term.GetSize(int(w.Fd())); err == nil && cols > 0 {
		total = cols
	}
	widths[len(heads)-1] = max(20, total-fixed-1)

	style := func(hex string) lipgloss.Style { return r.NewStyle().Foreground(lipgloss.Color(hex)) }
	header := r.NewStyle().Bold(true).Foreground(lipgloss.Color(p.Lavender))
	columns := []lipgloss.Style{{}, style(p.Subtext), style(p.Overlay), style(p.Overlay), style(p.Teal), style(p.Text)}

	line := func(cells []string, paint func(c int, text string) string) string {
		var parts []string
		for c, cell := range cells {
			if !keep[c] {
				continue
			}
			cell = ansi.Truncate(cell, widths[c], "…")
			if c < len(cells)-1 {
				cell += strings.Repeat(" ", widths[c]-ansi.StringWidth(cell))
			}
			parts = append(parts, paint(c, cell))
		}
		return strings.TrimRight(strings.Join(parts, "  "), " ")
	}
	fmt.Fprintln(w, header.Render(line(heads, func(_ int, t string) string { return t })))
	for i, s := range list {
		agent := r.NewStyle().Foreground(p.Agent(s.Agent))
		fmt.Fprintln(w, line(rows[i], func(c int, t string) string {
			if strings.TrimSpace(t) == "" {
				return t
			}
			if c == 0 {
				return agent.Render(t)
			}
			return columns[c].Render(t)
		}))
	}
}
