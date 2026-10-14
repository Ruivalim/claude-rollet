// Package adapters holds one entry per provider that rollet translates to.
//
// Every adapter goes through the same Anthropic -> Chat Completions
// translation; an adapter only adds what is specific to that provider:
// default URL, suggested models and a Patch for its quirks. Supporting a new
// provider is one new file that calls register() in its init.
package adapters

import "sort"

type Adapter struct {
	Type    string // value of "type" in config.json
	Label   string // shown in `rollet provider add`
	BaseURL string // default, editable in the form
	Models  []string
	// MaxTokensParam is the field the provider wants for the output cap.
	MaxTokensParam string
	// Patch adjusts the translated body right before it is sent.
	Patch func(body map[string]any)
}

var registry = map[string]Adapter{}

func register(a Adapter) { registry[a.Type] = a }

func Get(typ string) (Adapter, bool) {
	a, ok := registry[typ]
	return a, ok
}

// All returns the adapters with the generic "openai" last, since a named
// provider is the better pick whenever one exists.
func All() []Adapter {
	out := make([]Adapter, 0, len(registry))
	for _, a := range registry {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Type == Generic) != (out[j].Type == Generic) {
			return out[j].Type == Generic
		}
		return out[i].Label < out[j].Label
	})
	return out
}

const Generic = "openai"

func init() {
	register(Adapter{
		Type:  Generic,
		Label: "Any OpenAI-compatible endpoint (OpenRouter, Groq, Ollama...)",
	})
}
