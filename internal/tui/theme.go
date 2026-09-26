package tui

import (
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/HACKE-RC/lc/internal/theme"
)

type styles struct {
	p theme.Palette

	badge, headerPath, headerDim, chip lipgloss.Style

	pane, paneActive, paneTitle lipgloss.Style

	itemTitle, itemTitleSel, itemMeta, itemMetaSel, itemBar lipgloss.Style
	dim, muted, text, sep                                   lipgloss.Style

	previewTitle, roleUser, divider lipgloss.Style
	match, matchCur                 lipgloss.Style
	plain                           bool // no colors (NO_COLOR, dumb terminal)

	facts, position lipgloss.Style
}

func newStyles(p theme.Palette) styles {
	c := func(hex string) lipgloss.Color { return lipgloss.Color(hex) }
	border := lipgloss.RoundedBorder()
	return styles{
		p:          p,
		badge:      lipgloss.NewStyle().Bold(true).Foreground(c(p.Crust)).Background(c(p.Mauve)).Padding(0, 1),
		headerPath: lipgloss.NewStyle().Foreground(c(p.Text)).Bold(true),
		headerDim:  lipgloss.NewStyle().Foreground(c(p.Overlay)),
		chip:       lipgloss.NewStyle().Foreground(c(p.Text)).Background(c(p.Surface0)).Padding(0, 1),

		pane:       lipgloss.NewStyle().Border(border).BorderForeground(c(p.Surface1)).Padding(0, 1),
		paneActive: lipgloss.NewStyle().Border(border).BorderForeground(c(p.Lavender)).Padding(0, 1),
		paneTitle:  lipgloss.NewStyle().Bold(true).Foreground(c(p.Lavender)),

		itemTitle:    lipgloss.NewStyle().Foreground(c(p.Text)),
		itemTitleSel: lipgloss.NewStyle().Foreground(c(p.Text)).Background(c(p.Surface0)).Bold(true),
		itemMeta:     lipgloss.NewStyle().Foreground(c(p.Overlay)),
		itemMetaSel:  lipgloss.NewStyle().Foreground(c(p.Subtext)).Background(c(p.Surface0)),
		itemBar:      lipgloss.NewStyle().Background(c(p.Surface0)),
		dim:          lipgloss.NewStyle().Foreground(c(p.Overlay)),
		muted:        lipgloss.NewStyle().Foreground(c(p.Subtext)),
		text:         lipgloss.NewStyle().Foreground(c(p.Text)),
		sep:          lipgloss.NewStyle().Foreground(c(p.Surface1)),

		previewTitle: lipgloss.NewStyle().Bold(true).Foreground(c(p.Text)),
		roleUser:     lipgloss.NewStyle().Bold(true).Foreground(c(p.Sky)),
		divider:      lipgloss.NewStyle().Foreground(c(p.Surface1)),
		match:        lipgloss.NewStyle().Foreground(c(p.Text)).Background(c(p.Surface1)),
		matchCur:     lipgloss.NewStyle().Foreground(c(p.Crust)).Background(c(p.Peach)).Bold(true),
		plain:        lipgloss.ColorProfile() == termenv.Ascii,

		facts:    lipgloss.NewStyle().Bold(true).Foreground(c(p.Text)).Background(c(p.Surface0)).Padding(0, 1),
		position: lipgloss.NewStyle().Foreground(c(p.Crust)).Background(c(p.Lavender)).Bold(true).Padding(0, 1),
	}
}

// paintMatch highlights a search hit. Without colors it falls back to
// reverse video for the current match and underline for the rest, which
// NO_COLOR permits (it forbids color, not text attributes).
func (st styles) paintMatch(text string, current bool) string {
	switch {
	case st.plain && current:
		return "\x1b[7m" + text + "\x1b[27m"
	case st.plain:
		return "\x1b[4m" + text + "\x1b[24m"
	case current:
		return st.matchCur.Render(text)
	}
	return st.match.Render(text)
}

// markdownStyle maps the palette onto glamour with no document margin: the
// preview pane already supplies its own padding and indentation.
func markdownStyle(p theme.Palette) ansi.StyleConfig {
	s := func(v string) *string { return &v }
	b := func(v bool) *bool { return &v }
	u := func(v uint) *uint { return &v }
	fg := func(hex string) ansi.StylePrimitive { return ansi.StylePrimitive{Color: s(hex)} }
	heading := func(prefix string) ansi.StyleBlock {
		return ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: prefix}}
	}
	return ansi.StyleConfig{
		Document: ansi.StyleBlock{StylePrimitive: fg(p.Text), Margin: u(0)},
		BlockQuote: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: s(p.Green), Italic: b(true)},
			Indent:         u(1),
			IndentToken:    s("│ "),
		},
		List: ansi.StyleList{LevelIndent: 2},
		Heading: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{
			BlockSuffix: "\n", Color: s(p.Lavender), Bold: b(true),
		}},
		H1:             heading("▍ "),
		H2:             heading("▍ "),
		H3:             heading("▍ "),
		H4:             heading("▍ "),
		H5:             heading("▍ "),
		H6:             heading("▍ "),
		Strikethrough:  ansi.StylePrimitive{CrossedOut: b(true)},
		Emph:           ansi.StylePrimitive{Italic: b(true)},
		Strong:         ansi.StylePrimitive{Bold: b(true), Color: s(p.Peach)},
		HorizontalRule: ansi.StylePrimitive{Color: s(p.Surface1), Format: "\n────────\n"},
		Item:           ansi.StylePrimitive{BlockPrefix: "• ", Color: s(p.Text)},
		Enumeration:    ansi.StylePrimitive{BlockPrefix: ". ", Color: s(p.Mauve)},
		Task:           ansi.StyleTask{Ticked: "✓ ", Unticked: "○ "},
		Link:           ansi.StylePrimitive{Color: s(p.Overlay), Underline: b(true)},
		LinkText:       ansi.StylePrimitive{Color: s(p.Blue), Bold: b(true)},
		Image:          ansi.StylePrimitive{Color: s(p.Overlay), Underline: b(true)},
		ImageText:      ansi.StylePrimitive{Color: s(p.Blue), Format: "image: {{.text}}"},
		Code: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{
			// Non-breaking pads: a plain space would let lines wrap
			// between a code span and the punctuation after it.
			Prefix: "\u00a0", Suffix: "\u00a0", Color: s(p.Yellow), BackgroundColor: s(p.Surface0),
		}},
		CodeBlock: ansi.StyleCodeBlock{
			StyleBlock: ansi.StyleBlock{StylePrimitive: fg(p.Text), Margin: u(1)},
			Chroma: &ansi.Chroma{
				Text:                fg(p.Text),
				Error:               fg(p.Red),
				Comment:             ansi.StylePrimitive{Color: s(p.Overlay), Italic: b(true)},
				CommentPreproc:      fg(p.Pink),
				Keyword:             fg(p.Mauve),
				KeywordReserved:     fg(p.Mauve),
				KeywordNamespace:    fg(p.Mauve),
				KeywordType:         fg(p.Yellow),
				Operator:            fg(p.Sky),
				Punctuation:         fg(p.Subtext),
				Name:                fg(p.Text),
				NameBuiltin:         fg(p.Peach),
				NameTag:             fg(p.Blue),
				NameAttribute:       fg(p.Yellow),
				NameClass:           ansi.StylePrimitive{Color: s(p.Yellow), Bold: b(true)},
				NameDecorator:       fg(p.Peach),
				NameFunction:        fg(p.Blue),
				LiteralNumber:       fg(p.Peach),
				LiteralString:       fg(p.Green),
				LiteralStringEscape: fg(p.Pink),
				GenericDeleted:      fg(p.Red),
				GenericEmph:         ansi.StylePrimitive{Italic: b(true)},
				GenericInserted:     fg(p.Green),
				GenericStrong:       ansi.StylePrimitive{Bold: b(true)},
				GenericSubheading:   fg(p.Overlay),
				Background:          ansi.StylePrimitive{BackgroundColor: s(p.Mantle)},
			},
		},
		DefinitionDescription: ansi.StylePrimitive{BlockPrefix: "\n→ "},
	}
}
