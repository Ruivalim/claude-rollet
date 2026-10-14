package adapters

// ChatGPT models through the OpenAI API (API key, not the ChatGPT
// subscription). Reasoning models refuse max_tokens and sampling knobs.
func init() {
	register(Adapter{
		Type:           "chatgpt",
		Label:          "ChatGPT (OpenAI API key)",
		BaseURL:        "https://api.openai.com/v1",
		Models:         []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini"},
		MaxTokensParam: "max_completion_tokens",
		Patch: func(body map[string]any) {
			delete(body, "temperature")
			delete(body, "top_p")
		},
	})
}
