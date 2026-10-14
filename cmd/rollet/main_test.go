package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/ruivalim/claude-rollet/internal/config"
	"github.com/ruivalim/claude-rollet/internal/store"
)

func TestParseOdds(t *testing.T) {
	for in, want := range map[string]float64{"0.2": 0.2, "20%": 0.2, " 1 ": 1, "0": 0, "100%": 1, "0%": 0} {
		v, err := parseOdds(in)
		if err != nil || v == nil || *v != want {
			t.Errorf("%q -> %v %v", in, v, err)
		}
	}
	for _, in := range []string{"off", "OFF", "none"} {
		if v, err := parseOdds(in); err != nil || v != nil {
			t.Errorf("%q should turn odds off", in)
		}
	}
	for _, in := range []string{"1.5", "-0.1", "150%", "half", ""} {
		if _, err := parseOdds(in); err == nil {
			t.Errorf("%q should fail", in)
		}
	}
}

func TestEnvLine(t *testing.T) {
	cases := map[string]string{
		"zsh":  "export ANTHROPIC_BASE_URL=http://127.0.0.1:7777",
		"bash": "export ANTHROPIC_BASE_URL=http://127.0.0.1:7777",
		"fish": "set -gx ANTHROPIC_BASE_URL http://127.0.0.1:7777",
		"nu":   `$env.ANTHROPIC_BASE_URL = "http://127.0.0.1:7777"`,
	}
	for sh, want := range cases {
		got, err := envLine(sh, "http://127.0.0.1:7777")
		if err != nil || !strings.HasPrefix(got, want) {
			t.Errorf("%s: %q %v", sh, got, err)
		}
	}
	if _, err := envLine("tcsh", "x"); err == nil {
		t.Error("unknown shell should fail instead of printing something wrong")
	}
}

func TestProxyURL(t *testing.T) {
	if got := proxyURL(nil); got != "http://127.0.0.1:7777" {
		t.Errorf("nil config -> %s", got)
	}
	if got := proxyURL(&config.Config{Listen: ":9000"}); got != "http://127.0.0.1:9000" {
		t.Errorf("bare port -> %s", got)
	}
}

func TestDispatchUnknown(t *testing.T) {
	if err := dispatch([]string{"nope"}); err == nil {
		t.Error("unknown command must fail")
	}
	if err := dispatch([]string{"provider", "nope"}); err == nil {
		t.Error("unknown provider subcommand must fail")
	}
}

func TestCommandsAreReachable(t *testing.T) {
	// Every advertised command must resolve to itself, not to a shorter
	// prefix that happens to match first.
	for _, sec := range sections() {
		for _, c := range sec.cmds {
			words := strings.Fields(c.name)
			hits := 0
			for _, sec2 := range sections() {
				for _, c2 := range sec2.cmds {
					w2 := strings.Fields(c2.name)
					if len(w2) <= len(words) && strings.Join(words[:len(w2)], " ") == c2.name {
						hits++
					}
				}
			}
			if hits != 1 {
				t.Errorf("%q is shadowed (%d matches)", c.name, hits)
			}
		}
	}
}

func TestTableAlignsStyledCells(t *testing.T) {
	out := table([]string{"A", "B"}, [][]string{{sBold.Render("x"), "1"}, {"longer", "2"}})
	lines := strings.Split(out, "\n")
	col := -1
	for _, l := range lines {
		plain := stripANSI(l)
		idx := strings.LastIndex(plain, " ") + 1
		if col == -1 {
			col = idx
		} else if idx != col {
			t.Errorf("misaligned:\n%s", out)
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			in = true
		case in && r == 'm':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestBlindCells(t *testing.T) {
	s := store.Session{Provider: "deepseek", Guess: "claude"}
	if providerCell(s, false) != hiddenProvider {
		t.Error("provider must be hidden by default")
	}
	if guessCell(s) != "claude" {
		t.Error("an unrevealed guess must not show a verdict, it would leak the answer")
	}
	s.Revealed = true
	if providerCell(s, false) != "deepseek" || guessCell(s) != "claude ✗" {
		t.Errorf("revealed: %s %s", providerCell(s, false), guessCell(s))
	}
	if lipgloss.Width(clip("ããããããããããã", 5)) != 5 {
		t.Error("clip must count runes")
	}
}
