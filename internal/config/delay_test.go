package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseRange(t *testing.T) {
	s, ms := time.Second, time.Millisecond
	ok := map[string]Range{
		"":          {},
		"0":         {},
		"off":       {},
		"5s":        {5 * s, 5 * s},
		"30s-70s":   {30 * s, 70 * s},
		"30-70s":    {30 * s, 70 * s},
		"30-70":     {30 * s, 70 * s},
		"500ms-2s":  {500 * ms, 2 * s},
		"200-800ms": {200 * ms, 800 * ms},
		"1m-2m":     {time.Minute, 2 * time.Minute},
		" 1 - 3s ":  {s, 3 * s},
		"0-10s":     {0, 10 * s},
		"1.5s":      {1500 * ms, 1500 * ms},
	}
	for in, want := range ok {
		got, err := ParseRange(in)
		if err != nil || got != want {
			t.Errorf("%q -> %v %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"70s-30s", "abc", "5s-", "-5s", "1h", "5x", "10s-11m"} {
		if _, err := ParseRange(in); err == nil {
			t.Errorf("%q should fail", in)
		}
	}
}

func TestDelayFor(t *testing.T) {
	c := &Config{Delay: &Delay{First: "10s", Between: "1s-2s"}}
	plain := Provider{Name: "mimo"}
	claude := Provider{Name: "claude", Delay: &Delay{First: "30s-70s"}}
	off := Provider{Name: "x", Delay: &Delay{First: "0", Between: "0"}}

	if r := c.DelayFor(plain, true); r != (Range{10 * time.Second, 10 * time.Second}) {
		t.Errorf("global first = %v", r)
	}
	if r := c.DelayFor(claude, true); r.Min != 30*time.Second {
		t.Errorf("provider first should override: %v", r)
	}
	if r := c.DelayFor(claude, false); r.Max != 2*time.Second {
		t.Errorf("provider without between should inherit the global: %v", r)
	}
	if !c.DelayFor(off, true).Zero() || !c.DelayFor(off, false).Zero() {
		t.Error(`"0" on a provider must switch the global delay off`)
	}
	if !(&Config{}).DelayFor(plain, true).Zero() {
		t.Error("no delay configured means none")
	}
}

func TestValidateDelay(t *testing.T) {
	good := Provider{Name: "p", Type: TypeOpenAI, BaseURL: "x", Model: "m", APIKey: "k"}
	c := Config{Delay: &Delay{First: "9s-1s"}, Providers: []Provider{good}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "delay: first") {
		t.Errorf("err = %v", err)
	}
	good.Delay = &Delay{Between: "nope"}
	c = Config{Providers: []Provider{good}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "between") {
		t.Errorf("err = %v", err)
	}
}
