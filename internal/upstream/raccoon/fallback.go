package raccoon

// fallbackModels is the static catalogue snapshot, verified against the live
// /model_catalog on 2026-11-20 (ten chat models, default raccoon-090dd3).
// It is used before the first successful fetch (and whenever the remote list is
// empty); the remote catalogue always wins once it has been fetched.
//
// The three raccoon-* entries are the internal "Raccoon-Work" models: the
// upstream marks them hidden, so they are catalogued for explicit mapping but
// not advertised.
func fallbackModels() []Model {
	return []Model{
		{ID: "raccoon-090dd3", Name: "Raccoon-Work", ContextWindow: 1_000_000, MaxTokens: 100_000,
			SupportsImage: true, SupportsThinking: true, Visible: false, Multiplier: 1.0, Kind: "chat"},
		{ID: "raccoon-19b265", Name: "Raccoon-Work-260817-A", ContextWindow: 1_000_000, MaxTokens: 100_000,
			SupportsThinking: true, Visible: false, Multiplier: 1.0, Kind: "chat"},
		{ID: "raccoon-405a1c", Name: "Raccoon-Work-260817-B", ContextWindow: 1_000_000, MaxTokens: 100_000,
			SupportsThinking: true, Visible: false, Multiplier: 1.0, Kind: "chat"},
		{ID: "sn-sensenova-6-8-flash", Name: "SenseNova-6.8-Flash", ContextWindow: 256_000, MaxTokens: 63_999,
			SupportsImage: true, SupportsThinking: true, Visible: true, Kind: "chat"},
		{ID: "sn-sensenova-6-8-flash-lite", Name: "SenseNova-6.8-Flash-Lite", ContextWindow: 256_000, MaxTokens: 63_999,
			SupportsImage: true, SupportsThinking: true, Visible: true, Kind: "chat"},
		{ID: "sn-qwen4-max", Name: "Qwen4-Max", ContextWindow: 1_000_000, MaxTokens: 100_000,
			SupportsImage: true, SupportsThinking: true, Visible: true, Multiplier: 0.5, Kind: "chat"},
		{ID: "sn-glm-5-3", Name: "GLM-5-3", ContextWindow: 1_000_000, MaxTokens: 100_000,
			SupportsThinking: true, Visible: true, Multiplier: 0.75, Kind: "chat"},
		{ID: "sn-kimi-k3-1-pro", Name: "Kimi-K3.1-Pro", ContextWindow: 1_000_000, MaxTokens: 100_000,
			SupportsImage: true, SupportsThinking: true, Visible: true, Multiplier: 1.0, Kind: "chat"},
		{ID: "sn-glm-5-3-flash", Name: "GLM-5-3-Flash", ContextWindow: 1_000_000, MaxTokens: 100_000,
			SupportsImage: true, SupportsThinking: true, Visible: true, Multiplier: 0.2, Kind: "chat"},
		{ID: "sn-deepseek-v4-1-flash-1017", Name: "DeepSeek-V4.1-Flash-1017", ContextWindow: 1_000_000, MaxTokens: 100_000,
			SupportsImage: true, SupportsThinking: true, Visible: true, Multiplier: 0.25, Kind: "chat"},
		{ID: ImageModelID, Name: "Raccoon Image", SupportsImage: true, Visible: true, Kind: "image"},
	}
}

// FallbackModels returns a copy of the static catalogue.
func FallbackModels() []Model {
	out := make([]Model, len(fallbackModels()))
	copy(out, fallbackModels())
	return out
}
