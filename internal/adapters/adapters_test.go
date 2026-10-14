package adapters

import (
	"net/url"
	"testing"
)

func TestRegistry(t *testing.T) {
	all := All()
	if all[len(all)-1].Type != Generic {
		t.Error("generic adapter must be listed last")
	}
	seen := map[string]bool{}
	for _, a := range all {
		if seen[a.Type] {
			t.Errorf("duplicate type %s", a.Type)
		}
		seen[a.Type] = true
		if a.Label == "" {
			t.Errorf("%s has no label", a.Type)
		}
		if a.Type == Generic {
			continue
		}
		if u, err := url.Parse(a.BaseURL); err != nil || u.Scheme != "https" {
			t.Errorf("%s: base url %q", a.Type, a.BaseURL)
		}
		if len(a.Models) == 0 {
			t.Errorf("%s: no suggested models", a.Type)
		}
		switch a.MaxTokensParam {
		case "", "max_tokens", "max_completion_tokens":
		default:
			t.Errorf("%s: max tokens param %q", a.Type, a.MaxTokensParam)
		}
	}
	for _, want := range []string{"mimo", "deepseek", "chatgpt", "openai"} {
		if !seen[want] {
			t.Errorf("missing adapter %s", want)
		}
	}
}
