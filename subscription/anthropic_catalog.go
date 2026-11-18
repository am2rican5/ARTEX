// Catalog values are translated from oh-my-pi commit
// 1bdbf6d242d99ca62bdf2feeff4519ff992f647d under the MIT License.
package subscription

func claudeCatalogEntry(id, name string, contextWindowK, maxOutputTokens int, supportsThinkingDisable bool, efforts ...string) Model {
	return Model{
		ID:                      id,
		Name:                    name,
		Provider:                ProviderClaude,
		ContextWindowK:          contextWindowK,
		MaxOutputTokens:         maxOutputTokens,
		SupportsTools:           true,
		SupportsThinkingDisable: supportsThinkingDisable,
		SupportsOutputCap:       true,
		ReasoningEfforts:        efforts,
	}
}

var claudeCatalog = []Model{
	claudeCatalogEntry("claude-fable-5", "Claude Fable 5", 1000, 128000, false, "low", "medium", "high", "xhigh", "max"),
	claudeCatalogEntry("claude-fable-5-1", "Claude Fable 5.1", 1000, 128000, false, "low", "medium", "high", "xhigh", "max"),
	claudeCatalogEntry("claude-fable-5-2", "Claude Fable 5.2", 1000, 128000, false, "low", "medium", "high", "xhigh", "max"),
	claudeCatalogEntry("claude-haiku-4-5", "Claude Haiku 4.5", 200, 64000, true, "minimal", "low", "medium", "high", "xhigh"),
	claudeCatalogEntry("claude-haiku-4-5-20251001", "Claude Haiku 4.5", 200, 64000, true, "minimal", "low", "medium", "high", "xhigh"),
	claudeCatalogEntry("claude-haiku-5-5", "Claude Haiku 5.5", 1000, 128000, true, "low", "medium", "high", "xhigh", "max"),
	claudeCatalogEntry("claude-mythos-5", "Claude Mythos 5", 1000, 128000, false, "low", "medium", "high", "xhigh", "max"),
	claudeCatalogEntry("claude-mythos-5-1", "Claude Mythos 5.1", 1000, 128000, false, "low", "medium", "high", "xhigh", "max"),
	claudeCatalogEntry("claude-mythos-5-2", "Claude Mythos 5.2", 1000, 128000, false, "low", "medium", "high", "xhigh", "max"),
	claudeCatalogEntry("claude-opus-4-5", "Claude Opus 4.5", 200, 64000, true, "minimal", "low", "medium", "high", "xhigh"),
	claudeCatalogEntry("claude-opus-4-5-20251101", "Claude Opus 4.5", 200, 64000, true, "minimal", "low", "medium", "high", "xhigh"),
	claudeCatalogEntry("claude-opus-4-6", "Claude Opus 4.6", 1000, 128000, false, "low", "medium", "high", "max"),
	claudeCatalogEntry("claude-opus-4-7", "Claude Opus 4.7", 1000, 128000, false, "low", "medium", "high", "xhigh", "max"),
	claudeCatalogEntry("claude-opus-4-8", "Claude Opus 4.8", 1000, 128000, false, "low", "medium", "high", "xhigh", "max"),
	claudeCatalogEntry("claude-opus-5", "Claude Opus 5", 1000, 128000, false, "low", "medium", "high", "xhigh", "max"),
	claudeCatalogEntry("claude-opus-5-5", "Claude Opus 5.5", 1000, 128000, false, "low", "medium", "high", "xhigh", "max"),
	claudeCatalogEntry("claude-sonnet-4-5", "Claude Sonnet 4.5", 200, 64000, true, "minimal", "low", "medium", "high", "xhigh"),
	claudeCatalogEntry("claude-sonnet-4-5-20250929", "Claude Sonnet 4.5", 200, 64000, true, "minimal", "low", "medium", "high", "xhigh"),
	claudeCatalogEntry("claude-sonnet-4-6", "Claude Sonnet 4.6", 1000, 128000, false, "low", "medium", "high"),
	claudeCatalogEntry("claude-sonnet-5", "Claude Sonnet 5", 1000, 128000, false, "low", "medium", "high", "xhigh", "max"),
	claudeCatalogEntry("claude-sonnet-5-5", "Claude Sonnet 5.5", 1000, 128000, false, "low", "medium", "high", "xhigh", "max"),
}

// ClaudeModels returns pinned static candidates. It does not assert that the
// connected account is entitled to any listed model.
func ClaudeModels() []Model {
	models := make([]Model, len(claudeCatalog))
	for index, model := range claudeCatalog {
		models[index] = model
		models[index].ReasoningEfforts = append([]string(nil), model.ReasoningEfforts...)
	}
	return models
}

func claudeCatalogModel(id string) (Model, bool) {
	for _, model := range claudeCatalog {
		if model.ID == id {
			return model, true
		}
	}
	return Model{}, false
}
