package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Delay holds artificial latency ranges, written as text so they're easy to
// edit by hand: "30s-70s", "500ms-2s", "5s" (fixed) or "0" (none).
//
// The point is to blur the speed gap between providers, which otherwise
// gives the game away: slow thinking models next to a fast Claude.
type Delay struct {
	First   string `json:"first,omitempty"`   // first turn of a session
	Between string `json:"between,omitempty"` // every later call
}

type Range struct{ Min, Max time.Duration }

func (r Range) Zero() bool { return r.Max <= 0 }

func (r Range) String() string {
	if r.Zero() {
		return "none"
	}
	if r.Min == r.Max {
		return r.Min.String()
	}
	return r.Min.String() + "-" + r.Max.String()
}

// ParseRange reads "A-B", "A" or "0"/"off"/"". A bare number takes the unit
// of the other side ("30-70s"), or seconds when neither side has one.
func ParseRange(s string) (Range, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "0" || s == "off" || s == "none" {
		return Range{}, nil
	}
	lo, hi, isRange := strings.Cut(s, "-")
	if !isRange {
		hi = lo
	}
	lo, hi = strings.TrimSpace(lo), strings.TrimSpace(hi)
	unit := "s"
	if u := unitOf(hi); u != "" {
		unit = u
	} else if u := unitOf(lo); u != "" {
		unit = u
	}
	from, err := parseDur(lo, unit)
	if err != nil {
		return Range{}, fmt.Errorf("delay %q: %w", s, err)
	}
	to, err := parseDur(hi, unit)
	if err != nil {
		return Range{}, fmt.Errorf("delay %q: %w", s, err)
	}
	if from < 0 || to < from {
		return Range{}, fmt.Errorf("delay %q: needs 0 <= min <= max", s)
	}
	if to > 10*time.Minute {
		return Range{}, fmt.Errorf("delay %q: over 10m, Claude Code would time out first", s)
	}
	return Range{from, to}, nil
}

func unitOf(s string) string {
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i < 0 {
		return ""
	}
	return s[i:]
}

func parseDur(s, unit string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty bound")
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		s += unit
	}
	return time.ParseDuration(s)
}

// DelayFor picks the range for a request. Provider fields override the
// global ones one by one, so "first" can be set per provider while "between"
// still comes from the top level; "0" on a provider turns a global one off.
func (c *Config) DelayFor(p Provider, firstTurn bool) Range {
	pick := func(d *Delay) string {
		if d == nil {
			return ""
		}
		if firstTurn {
			return d.First
		}
		return d.Between
	}
	v := pick(p.Delay)
	if v == "" {
		v = pick(c.Delay)
	}
	r, _ := ParseRange(v) // validated at load; a bad value means no delay
	return r
}

func (d *Delay) validate(where string) error {
	if d == nil {
		return nil
	}
	if _, err := ParseRange(d.First); err != nil {
		return fmt.Errorf("%s: first: %w", where, err)
	}
	if _, err := ParseRange(d.Between); err != nil {
		return fmt.Errorf("%s: between: %w", where, err)
	}
	return nil
}
