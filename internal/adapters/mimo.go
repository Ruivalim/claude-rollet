package adapters

// Xiaomi MiMo. Streams its reasoning as reasoning_content, which the shared
// translation already carries across turns as thinking blocks.
func init() {
	register(Adapter{
		Type:    "mimo",
		Label:   "Xiaomi MiMo",
		BaseURL: "https://api.xiaomimimo.com/v1",
		Models:  []string{"mimo-v2.5-pro", "mimo-v2.5", "mimo-v2.5-pro-ultraspeed"},
	})
}
