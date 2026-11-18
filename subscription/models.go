package subscription

// Model is a pinned catalog candidate, not proof of an account's entitlement
// or remaining subscription allowance. Capabilities describe upstream controls.
type Model struct {
	ID                      string   `json:"id"`
	Name                    string   `json:"name"`
	Provider                string   `json:"provider"`
	ContextWindowK          int      `json:"context_window_k"`
	MaxOutputTokens         int      `json:"max_output_tokens"`
	SupportsTools           bool     `json:"supports_tools"`
	SupportsThinkingDisable bool     `json:"supports_thinking_disable"`
	SupportsOutputCap       bool     `json:"supports_output_cap"`
	ReasoningEfforts        []string `json:"reasoning_efforts,omitempty"`
}

// Models lists pinned candidates for one selected credential provider.
// It never discovers another account or asserts live model availability.
func Models(provider string) []Model {
	switch provider {
	case ProviderClaude:
		return ClaudeModels()
	case ProviderCodex:
		return CodexModels()
	default:
		return nil
	}
}
