// Package theme holds lc's Catppuccin palettes. Hierarchy comes from
// text/overlay/surface; hue is reserved for agent identity and Markdown meaning.
package theme

import "github.com/charmbracelet/lipgloss"

// Palette is one Catppuccin flavor as hex colors.
type Palette struct {
	Text, Subtext, Overlay, Surface1, Surface0, Mantle, Crust string
	Blue, Lavender, Mauve, Green, Teal, Sky, Yellow, Peach    string
	Pink, Flamingo, Red                                       string
}

var Mocha = Palette{
	Text: "#cdd6f4", Subtext: "#a6adc8", Overlay: "#6c7086",
	Surface1: "#45475a", Surface0: "#313244", Mantle: "#181825", Crust: "#11111b",
	Blue: "#89b4fa", Lavender: "#b4befe", Mauve: "#cba6f7", Green: "#a6e3a1",
	Teal: "#94e2d5", Sky: "#89dceb", Yellow: "#f9e2af", Peach: "#fab387",
	Pink: "#f5c2e7", Flamingo: "#f2cdcd", Red: "#f38ba8",
}

var Latte = Palette{
	Text: "#4c4f69", Subtext: "#6c6f85", Overlay: "#9ca0b0",
	Surface1: "#bcc0cc", Surface0: "#ccd0da", Mantle: "#e6e9ef", Crust: "#dce0e8",
	Blue: "#1e66f5", Lavender: "#7287fd", Mauve: "#8839ef", Green: "#40a02b",
	Teal: "#179299", Sky: "#04a5e5", Yellow: "#df8e1d", Peach: "#fe640b",
	Pink: "#ea76cb", Flamingo: "#dd7878", Red: "#d20f39",
}

// For picks the flavor matching the renderer's terminal background.
func For(r *lipgloss.Renderer) Palette {
	if r.HasDarkBackground() {
		return Mocha
	}
	return Latte
}

// claudeOrange is Claude Code's own accent, identical in both flavors.
const claudeOrange = "#d27354"

// Agent returns the identity color of an agent.
func (p Palette) Agent(agent string) lipgloss.Color {
	var c string
	switch agent {
	case "claude":
		c = claudeOrange
	case "codex", "copilot":
		c = p.Blue
	case "droid":
		c = p.Pink
	case "opencode":
		c = p.Green
	case "cursor":
		c = p.Teal
	case "grok":
		c = p.Mauve
	case "kimi":
		c = p.Flamingo
	case "gemini":
		c = p.Yellow
	case "pi":
		c = p.Lavender
	case "omp":
		c = p.Peach
	default:
		c = p.Text
	}
	return lipgloss.Color(c)
}
