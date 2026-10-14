// Package roll picks a provider for a new session.
package roll

import (
	"crypto/rand"
	"errors"
	"math/big"

	"github.com/ruivalim/claude-rollet/internal/config"
)

// Intn returns a uniform integer in [0, n). Swappable in tests.
type Intn func(n int64) int64

func CryptoIntn(n int64) int64 {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		panic(err) // crypto/rand failing means the OS is broken; no sane fallback
	}
	return v.Int64()
}

// oddsScale turns a probability into an integer draw so the whole roll stays
// on crypto/rand without float tricks.
const oddsScale = 1_000_000

// Pick chooses a provider. With claude_odds set, it first decides Claude vs
// not-Claude with that probability, then picks by weight inside the group.
// If one group is empty the odds can't apply and everyone competes by weight.
func Pick(cfg *config.Config, intn Intn) (config.Provider, error) {
	var claude, others []config.Provider
	for _, p := range cfg.Providers {
		if p.W() <= 0 {
			continue
		}
		if p.IsClaude() {
			claude = append(claude, p)
		} else {
			others = append(others, p)
		}
	}
	pool := append(append([]config.Provider{}, claude...), others...)
	if len(pool) == 0 {
		return config.Provider{}, errors.New("no provider with weight > 0")
	}
	if cfg.ClaudeOdds != nil && len(claude) > 0 && len(others) > 0 {
		threshold := int64(*cfg.ClaudeOdds * oddsScale)
		if intn(oddsScale) < threshold {
			pool = claude
		} else {
			pool = others
		}
	}
	return weighted(pool, intn), nil
}

func weighted(pool []config.Provider, intn Intn) config.Provider {
	total := 0
	for _, p := range pool {
		total += p.W()
	}
	n := int(intn(int64(total)))
	for _, p := range pool {
		n -= p.W()
		if n < 0 {
			return p
		}
	}
	return pool[len(pool)-1]
}
