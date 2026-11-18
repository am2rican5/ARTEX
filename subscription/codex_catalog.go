package subscription

// codexModels is the internal text-completion catalog pinned from OMP's
// openai-codex catalog at commit 1bdbf6d242d99ca62bdf2feeff4519ff992f647d.
// The separately catalogued gpt-image-2 image model is intentionally absent:
// llm.Provider is a text/tool completion interface and cannot represent its
// image output. Catalog membership is not proof of account entitlement.
//
// OMP source is MIT licensed:
// Copyright (c) 2025 Mario Zechner
// Copyright (c) 2025-2026 Can Bölük
// Copyright (c) 2026 Stencil Labs, Inc.
var codexModels = [...]Model{
	codexModel("gpt-5.6-luna", "GPT-5.6 Luna", 1000),
	codexModel("gpt-5.6-sol", "GPT-5.6 Sol", 1000),
	codexModel("gpt-5.6-terra", "GPT-5.6 Terra", 1000),
	codexModel("gpt-6-astra", "GPT-6 Astra", 272),
	codexModel("gpt-6-luna", "GPT-6 Luna", 272),
	codexModel("gpt-6-sol", "GPT-6 Sol", 272),
	codexModel("gpt-6.1-sol", "GPT-6.1 Sol", 500),
	codexModel("gpt-daybreak-blue-latest", "Daybreak Blue", 272),
}

// CodexModels returns an isolated copy of the pinned Codex catalog.
func CodexModels() []Model {
	models := make([]Model, len(codexModels))
	for index, model := range codexModels {
		model.ReasoningEfforts = append([]string(nil), model.ReasoningEfforts...)
		models[index] = model
	}
	return models
}

func codexModel(id, name string, contextWindowK int) Model {
	return Model{
		ID:                      id,
		Name:                    name,
		Provider:                ProviderCodex,
		ContextWindowK:          contextWindowK,
		MaxOutputTokens:         128000,
		SupportsTools:           true,
		SupportsThinkingDisable: false,
		SupportsOutputCap:       false,
		ReasoningEfforts:        []string{"low", "medium", "high", "xhigh", "max"},
	}
}
