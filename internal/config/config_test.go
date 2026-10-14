package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(Example), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Providers) != 4 || c.Listen != DefaultListen {
		t.Fatalf("got %+v", c)
	}
	// named adapters fill in their own base URL
	if _, mimo := c.Provider("mimo"); mimo.BaseURL != "https://api.xiaomimimo.com/v1" {
		t.Errorf("mimo base url = %q", mimo.BaseURL)
	}
}

func TestLoadRejectsTypos(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(path, []byte(`{"providers":[],"claude_odd":0.2}`), 0o600)
	if _, err := Load(path); err == nil {
		t.Fatal("a typo in a knob must not be silently ignored")
	}
}

func TestValidate(t *testing.T) {
	t.Setenv("ROLLET_TEST_KEY", "sk-x")
	t.Setenv("ROLLET_EMPTY_KEY", "")
	ok := func() Provider {
		return Provider{Name: "p", Type: TypeOpenAI, BaseURL: "http://x", Model: "m", APIKeyEnv: "ROLLET_TEST_KEY"}
	}
	zero, neg := 0, -1
	bad, over := -0.1, 1.5

	cases := map[string]struct {
		cfg  Config
		want string
	}{
		"empty":         {Config{}, "no providers"},
		"no name":       {Config{Providers: []Provider{{Type: TypeOpenAI}}}, "no name"},
		"dup":           {Config{Providers: []Provider{ok(), ok()}}, "duplicate"},
		"bad type":      {Config{Providers: []Provider{{Name: "p", Type: "gemini", BaseURL: "x"}}}, "unknown type \"gemini\" (known: anthropic, "},
		"no model":      {Config{Providers: []Provider{{Name: "p", Type: TypeOpenAI, BaseURL: "x", APIKey: "k"}}}, "model is required"},
		"no key":        {Config{Providers: []Provider{{Name: "p", Type: TypeOpenAI, BaseURL: "x", Model: "m"}}}, "api_key"},
		"empty env":     {Config{Providers: []Provider{{Name: "p", Type: TypeOpenAI, BaseURL: "x", Model: "m", APIKeyEnv: "ROLLET_EMPTY_KEY"}}}, "ROLLET_EMPTY_KEY is empty"},
		"neg weight":    {Config{Providers: []Provider{func() Provider { p := ok(); p.Weight = &neg; return p }()}}, "negative"},
		"all zero":      {Config{Providers: []Provider{func() Provider { p := ok(); p.Weight = &zero; return p }()}}, "weight 0"},
		"odds < 0":      {Config{ClaudeOdds: &bad, Providers: []Provider{ok()}}, "claude_odds"},
		"odds > 1":      {Config{ClaudeOdds: &over, Providers: []Provider{ok()}}, "claude_odds"},
		"bad max param": {Config{Providers: []Provider{func() Provider { p := ok(); p.MaxTokensParam = "max"; return p }()}}, "max_tokens_param"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	good := Config{Providers: []Provider{ok(), {Name: "claude", Type: TypeAnthropic, BaseURL: "https://api.anthropic.com"}}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestModelFor(t *testing.T) {
	p := Provider{Model: "big", SmallModel: "small"}
	if p.ModelFor("claude-haiku-4-5") != "small" || p.ModelFor("claude-opus-5-5") != "big" {
		t.Fail()
	}
	if (Provider{Model: "big"}).ModelFor("claude-haiku-4-5") != "big" {
		t.Error("no small model configured should fall back to model")
	}
}

func TestSaveIsPrivateAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	odds := 0.25
	c := &Config{Listen: "127.0.0.1:1", ClaudeOdds: &odds, Providers: []Provider{{Name: "p", Type: TypeOpenAI, BaseURL: "x", Model: "m", APIKey: "secret"}}}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("config holds API keys, mode is %v", st.Mode().Perm())
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if *back.ClaudeOdds != 0.25 || back.Providers[0].APIKey != "secret" {
		t.Errorf("round trip lost data: %+v", back)
	}
}
