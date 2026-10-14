// Package config loads the provider list and the knobs that drive the roll.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ruivalim/claude-rollet/internal/adapters"
)

const (
	TypeAnthropic = "anthropic" // passthrough, no translation (Claude itself, or any Anthropic-compatible endpoint)
	TypeOpenAI    = "openai"    // generic translated provider; named ones live in internal/adapters

	DefaultListen = "127.0.0.1:7777"
)

type Provider struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`

	// Literal key or the name of an env var holding it. For anthropic providers
	// both are optional: without them the client's own auth is forwarded.
	APIKey    string `json:"api_key,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`

	// Model sent upstream. SmallModel replaces it when Claude Code asks for a
	// haiku model (titles, summaries). Ignored by anthropic providers.
	Model      string `json:"model,omitempty"`
	SmallModel string `json:"small_model,omitempty"`

	// Caps max_tokens: Claude Code asks for far more than most providers allow.
	MaxTokens      int    `json:"max_tokens,omitempty"`
	MaxTokensParam string `json:"max_tokens_param,omitempty"` // "max_tokens" (default) or "max_completion_tokens"

	// Merged into every upstream request body (reasoning knobs and the like).
	ExtraBody map[string]any `json:"extra_body,omitempty"`

	// Relative weight inside its group. nil means 1, 0 takes it out of the roll.
	Weight *int `json:"weight,omitempty"`

	// Overrides the global delay for sessions on this provider.
	Delay *Delay `json:"delay,omitempty"`
}

type Config struct {
	Listen string `json:"listen,omitempty"`

	// Probability (0..1) that a new session lands on an anthropic provider.
	// nil means "no special treatment": every provider competes by weight alone.
	ClaudeOdds *float64 `json:"claude_odds,omitempty"`

	// Artificial latency for every provider; see Delay.
	Delay *Delay `json:"delay,omitempty"`

	Providers []Provider `json:"providers"`
}

func (p Provider) W() int {
	if p.Weight == nil {
		return 1
	}
	return *p.Weight
}

func (p Provider) IsClaude() bool { return p.Type == TypeAnthropic }

// Translated reports whether requests go through the Chat Completions
// translation (every registered adapter) rather than passthrough.
func (p Provider) Translated() bool {
	_, ok := adapters.Get(p.Type)
	return ok
}

// Key resolves the API key. Empty is valid only for anthropic providers.
func (p Provider) Key() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	if p.APIKeyEnv != "" {
		return os.Getenv(p.APIKeyEnv)
	}
	return ""
}

func (p Provider) ModelFor(requested string) string {
	if p.SmallModel != "" && strings.Contains(strings.ToLower(requested), "haiku") {
		return p.SmallModel
	}
	return p.Model
}

// Validate fails loudly instead of silently dropping a provider: a provider
// that quietly leaves the pool would skew the experiment without anyone noticing.
func (c *Config) Validate() error {
	if len(c.Providers) == 0 {
		return errors.New("no providers configured")
	}
	if c.ClaudeOdds != nil && (*c.ClaudeOdds < 0 || *c.ClaudeOdds > 1) {
		return fmt.Errorf("claude_odds must be between 0 and 1, got %v", *c.ClaudeOdds)
	}
	if err := c.Delay.validate("delay"); err != nil {
		return err
	}
	seen := map[string]bool{}
	active := 0
	for i, p := range c.Providers {
		where := fmt.Sprintf("provider #%d (%q)", i+1, p.Name)
		if p.Name == "" {
			return fmt.Errorf("provider #%d has no name", i+1)
		}
		if seen[p.Name] {
			return fmt.Errorf("%s: duplicate name", where)
		}
		seen[p.Name] = true
		if p.W() < 0 {
			return fmt.Errorf("%s: weight cannot be negative", where)
		}
		if err := p.Delay.validate(where + " delay"); err != nil {
			return err
		}
		if p.W() > 0 {
			active++
		}
		switch p.Type {
		case TypeAnthropic:
			if p.BaseURL == "" {
				return fmt.Errorf("%s: base_url is required", where)
			}
			if p.APIKeyEnv != "" && p.Key() == "" {
				return fmt.Errorf("%s: env var %s is empty", where, p.APIKeyEnv)
			}
		default:
			if !p.Translated() {
				return fmt.Errorf("%s: unknown type %q (known: %s)", where, p.Type, strings.Join(KnownTypes(), ", "))
			}
			if p.BaseURL == "" {
				return fmt.Errorf("%s: base_url is required", where)
			}
			if p.Model == "" {
				return fmt.Errorf("%s: model is required", where)
			}
			if a, _ := adapters.Get(p.Type); !a.OAuth && p.Key() == "" {
				if p.APIKeyEnv != "" {
					return fmt.Errorf("%s: env var %s is empty", where, p.APIKeyEnv)
				}
				return fmt.Errorf("%s: api_key or api_key_env is required", where)
			}
			switch p.MaxTokensParam {
			case "", "max_tokens", "max_completion_tokens":
			default:
				return fmt.Errorf("%s: max_tokens_param must be max_tokens or max_completion_tokens", where)
			}
		}
	}
	if active == 0 {
		return errors.New("every provider has weight 0, nothing to roll")
	}
	return nil
}

func KnownTypes() []string {
	out := []string{TypeAnthropic}
	for _, a := range adapters.All() {
		out = append(out, a.Type)
	}
	return out
}

// Home is where config.json and sessions.json live. ROLLET_HOME wins, then
// XDG_CONFIG_HOME, then ~/.config.
func Home() (string, error) {
	if h := os.Getenv("ROLLET_HOME"); h != "" {
		return h, nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "claude-rollet"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "claude-rollet"), nil
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if a, ok := adapters.Get(p.Type); ok && p.BaseURL == "" {
			p.BaseURL = a.BaseURL
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
	}
	return &c, nil
}

// Example is what `rollet init` writes. Claude stays in the pool on purpose:
// without it there is nothing to compare against.
const Example = `{
  "listen": "127.0.0.1:7777",
  "claude_odds": 0.5,
  "providers": [
    {
      "name": "claude",
      "type": "anthropic",
      "base_url": "https://api.anthropic.com"
    },
    {
      "name": "mimo",
      "type": "mimo",
      "api_key": "sk-...",
      "model": "mimo-v2.5-pro"
    },
    {
      "name": "deepseek",
      "type": "deepseek",
      "api_key_env": "DEEPSEEK_API_KEY",
      "model": "deepseek-v4-pro",
      "weight": 0
    },
    {
      "name": "openrouter-kimi",
      "type": "openai",
      "base_url": "https://openrouter.ai/api/v1",
      "api_key_env": "OPENROUTER_API_KEY",
      "model": "moonshotai/kimi-k2",
      "weight": 0
    }
  ]
}
`

// Save writes atomically with 0600: the file may hold API keys.
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (c *Config) Provider(name string) (int, *Provider) {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return i, &c.Providers[i]
		}
	}
	return -1, nil
}

// Odds returns each provider's effective chance of winning a new session,
// in config order, using the same rules as roll.Pick.
func (c *Config) Odds() []float64 {
	out := make([]float64, len(c.Providers))
	var wClaude, wOthers int
	for _, p := range c.Providers {
		if p.W() <= 0 {
			continue
		}
		if p.IsClaude() {
			wClaude += p.W()
		} else {
			wOthers += p.W()
		}
	}
	split := c.ClaudeOdds != nil && wClaude > 0 && wOthers > 0
	for i, p := range c.Providers {
		if p.W() <= 0 {
			continue
		}
		switch {
		case !split:
			out[i] = float64(p.W()) / float64(wClaude+wOthers)
		case p.IsClaude():
			out[i] = *c.ClaudeOdds * float64(p.W()) / float64(wClaude)
		default:
			out[i] = (1 - *c.ClaudeOdds) * float64(p.W()) / float64(wOthers)
		}
	}
	return out
}
