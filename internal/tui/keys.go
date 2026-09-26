package tui

import "github.com/charmbracelet/bubbles/key"

type keyMap struct {
	Up, Down, Top, Bottom, HalfDown, HalfUp, PageDown, PageUp key.Binding
	PreviewDown, PreviewUp                                    key.Binding
	Open, Filter, Narrow, Widen                               key.Binding
	Hide, Undo, Unhide, IDs                                   key.Binding
	Resume, Path, Help, Quit                                  key.Binding
}

func newKeyMap() keyMap {
	b := func(help string, desc string, keys ...string) key.Binding {
		return key.NewBinding(key.WithKeys(keys...), key.WithHelp(help, desc))
	}
	return keyMap{
		Up:          b("k/↑", "up", "k", "up"),
		Down:        b("j/↓", "down", "j", "down"),
		Top:         b("gg/home", "first", "home"),
		Bottom:      b("G/end", "last", "G", "end"),
		HalfDown:    b("^d", "half page down", "ctrl+d"),
		HalfUp:      b("^u", "half page up", "ctrl+u"),
		PageDown:    b("pgdn/^f", "page down", "pgdown", "ctrl+f", " "),
		PageUp:      b("pgup/^b", "page up", "pgup", "ctrl+b"),
		PreviewDown: b("J", "scroll preview down", "J"),
		PreviewUp:   b("K", "scroll preview up", "K"),
		Open:        b("o", "open preview", "o"),
		Filter:      b("/", "filter", "/"),
		Narrow:      b("[", "narrower list", "[", "left"),
		Widen:       b("]", "wider list", "]", "right"),
		Hide:        b("h", "hide dir", "h"),
		Undo:        b("u", "undo hide", "u"),
		Unhide:      b("H", "unhide all", "H"),
		IDs:         b("i", "ids", "i"),
		Resume:      b("enter", "resume", "enter"),
		Path:        b("p", "print path", "p"),
		Help:        b("?", "help", "?"),
		Quit:        b("q", "quit", "q", "esc", "ctrl+c"),
	}
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Down, k.Up, k.Open, k.Filter, k.Hide, k.Resume, k.Path, k.Help, k.Quit}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Down, k.Up, k.Top, k.Bottom},
		{k.HalfDown, k.HalfUp, k.PageDown, k.PageUp},
		{k.Open, k.PreviewDown, k.PreviewUp, k.Narrow, k.Widen},
		{k.Filter, k.Hide, k.Undo, k.Unhide},
		{k.IDs, k.Resume, k.Path, k.Help, k.Quit},
	}
}
