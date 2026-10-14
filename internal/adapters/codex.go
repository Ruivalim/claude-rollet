package adapters

// ChatGPT through your subscription, signing in like the Codex CLI does
// (`rollet codex login`). Talks the Responses API to the Codex backend, not
// Chat Completions, so it has its own translation in internal/translate.
func init() {
	register(Adapter{
		Type:     "codex",
		Label:    "ChatGPT subscription (Codex login, no API key)",
		BaseURL:  "https://chatgpt.com/backend-api",
		Models:   []string{"gpt-6.1-sol", "gpt-6-sol", "gpt-6-luna", "gpt-6-astra", "gpt-5.6-sol", "gpt-5.5"},
		Protocol: ProtocolResponses,
		OAuth:    true,
	})
}
