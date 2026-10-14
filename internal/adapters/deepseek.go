package adapters

// DeepSeek. In thinking mode it rejects assistant turns (tool calls above all)
// that come back without their reasoning_content; the shared translation keeps
// that reasoning as thinking blocks and returns it.
func init() {
	register(Adapter{
		Type:    "deepseek",
		Label:   "DeepSeek",
		BaseURL: "https://api.deepseek.com",
		Models:  []string{"deepseek-v4-pro", "deepseek-v4-flash"},
	})
}
