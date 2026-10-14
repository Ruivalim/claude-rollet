package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
)

var (
	accent  = lipgloss.AdaptiveColor{Light: "#B4235A", Dark: "#FF5F87"}
	muted   = lipgloss.AdaptiveColor{Light: "#6B6B6B", Dark: "#9A9A9A"}
	good    = lipgloss.AdaptiveColor{Light: "#1F7A3A", Dark: "#5FD787"}
	bad     = lipgloss.AdaptiveColor{Light: "#B3261E", Dark: "#FF6B6B"}
	sTitle  = lipgloss.NewStyle().Bold(true).Foreground(accent)
	sMuted  = lipgloss.NewStyle().Foreground(muted)
	sGood   = lipgloss.NewStyle().Foreground(good)
	sBad    = lipgloss.NewStyle().Foreground(bad)
	sBold   = lipgloss.NewStyle().Bold(true)
	sHidden = lipgloss.NewStyle().Foreground(muted).Italic(true)
	sHeader = lipgloss.NewStyle().Bold(true).Foreground(muted)
)

const hiddenProvider = "???"

func theme() *huh.Theme { return huh.ThemeCharm() }

func interactive() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

func ok(format string, a ...any) {
	fmt.Println(sGood.Render("✓ ") + fmt.Sprintf(format, a...))
}

func warn(format string, a ...any) {
	fmt.Fprintln(os.Stderr, sBad.Render("! ")+fmt.Sprintf(format, a...))
}

func hint(format string, a ...any) {
	fmt.Println(sMuted.Render(fmt.Sprintf(format, a...)))
}

// table renders rows with padded columns. lipgloss.Width keeps styled cells
// aligned (ANSI codes have no width).
func table(header []string, rows [][]string) string {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = lipgloss.Width(h)
	}
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], lipgloss.Width(c))
		}
	}
	line := func(cells []string, style *lipgloss.Style) string {
		var b strings.Builder
		for i, c := range cells {
			if style != nil {
				c = style.Render(c)
			}
			b.WriteString(c)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-lipgloss.Width(c)+2))
			}
		}
		return strings.TrimRight(b.String(), " ")
	}
	var out []string
	out = append(out, line(header, &sHeader))
	for _, r := range rows {
		out = append(out, line(r, nil))
	}
	return strings.Join(out, "\n")
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Local().Format("2006-01-02")
	}
}

func pct(v float64) string { return fmt.Sprintf("%.0f%%", v*100) }

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
