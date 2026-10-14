package roll

import (
	"math"
	"testing"

	"github.com/ruivalim/claude-rollet/internal/config"
)

func w(n int) *int         { return &n }
func f(v float64) *float64 { return &v }

func pool(odds *float64) *config.Config {
	return &config.Config{
		ClaudeOdds: odds,
		Providers: []config.Provider{
			{Name: "claude", Type: config.TypeAnthropic},
			{Name: "a", Type: config.TypeOpenAI},
			{Name: "b", Type: config.TypeOpenAI, Weight: w(3)},
			{Name: "off", Type: config.TypeOpenAI, Weight: w(0)},
		},
	}
}

// draws uses the real crypto source: the point is to check the roll that
// ships, not a mocked one. 40k draws keeps 3 points of tolerance > 10 sigma.
func draws(t *testing.T, cfg *config.Config, n int) map[string]float64 {
	t.Helper()
	counts := map[string]float64{}
	for range n {
		p, err := Pick(cfg, CryptoIntn)
		if err != nil {
			t.Fatal(err)
		}
		counts[p.Name]++
	}
	for k := range counts {
		counts[k] /= float64(n)
	}
	return counts
}

func TestPickMatchesOdds(t *testing.T) {
	for _, tc := range []struct {
		name string
		odds *float64
	}{
		{"weights only", nil},
		{"claude 20%", f(0.2)},
		{"claude 70%", f(0.7)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := pool(tc.odds)
			got := draws(t, cfg, 40000)
			for i, want := range cfg.Odds() {
				name := cfg.Providers[i].Name
				if math.Abs(got[name]-want) > 0.03 {
					t.Errorf("%s: got %.3f, want %.3f", name, got[name], want)
				}
			}
			if got["off"] != 0 {
				t.Error("weight 0 provider was picked")
			}
		})
	}
}

func TestOddsSumToOne(t *testing.T) {
	for _, o := range []*float64{nil, f(0), f(0.33), f(1)} {
		sum := 0.0
		for _, v := range pool(o).Odds() {
			sum += v
		}
		if math.Abs(sum-1) > 1e-9 {
			t.Errorf("odds %v sum to %v", o, sum)
		}
	}
}

func TestPickExtremes(t *testing.T) {
	for range 2000 {
		if p, _ := Pick(pool(f(0)), CryptoIntn); p.IsClaude() {
			t.Fatal("claude_odds=0 picked claude")
		}
		if p, _ := Pick(pool(f(1)), CryptoIntn); !p.IsClaude() {
			t.Fatal("claude_odds=1 picked someone else")
		}
	}
}

func TestPickOddsIgnoredWithOneGroup(t *testing.T) {
	// Only non-Claude providers: claude_odds=0.9 must not leave a 90% hole.
	cfg := &config.Config{ClaudeOdds: f(0.9), Providers: []config.Provider{
		{Name: "a", Type: config.TypeOpenAI}, {Name: "b", Type: config.TypeOpenAI},
	}}
	got := draws(t, cfg, 20000)
	if math.Abs(got["a"]-0.5) > 0.03 {
		t.Errorf("a = %.3f", got["a"])
	}
}

func TestPickNothingActive(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{{Name: "x", Type: config.TypeOpenAI, Weight: w(0)}}}
	if _, err := Pick(cfg, CryptoIntn); err == nil {
		t.Fatal("expected error")
	}
}

func TestWeightedBoundaries(t *testing.T) {
	cfg := pool(nil)
	// total weight = 1+1+3 = 5; draw 0 -> claude, 1 -> a, 2..4 -> b
	for draw, want := range map[int64]string{0: "claude", 1: "a", 2: "b", 4: "b"} {
		p, _ := Pick(cfg, func(int64) int64 { return draw })
		if p.Name != want {
			t.Errorf("draw %d -> %s, want %s", draw, p.Name, want)
		}
	}
}
