package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/subscription"
)

func createNativeProfileCredential(t *testing.T, f *nativeHTTPFixture, provider string) *db.LLMCredential {
	t.Helper()
	label := fmt.Sprintf("native-profile-%s-%d-%d", provider, time.Now().UnixNano(), os.Getpid())
	credential, err := f.m.pg.CreateLLMCredential(t.Context(), provider, label, json.RawMessage(`{"fixture":"encrypted"}`))
	if err != nil {
		t.Fatalf("create %s credential: %v", provider, err)
	}
	f.cleanupCredentialIDs = append(f.cleanupCredentialIDs, credential.ID)
	return credential
}

func catalogModel(t *testing.T, provider string, supportsDisable bool) subscription.Model {
	t.Helper()
	models := subscription.Models(provider)
	for _, model := range models {
		if !supportsDisable || model.SupportsThinkingDisable {
			return model
		}
	}
	t.Fatalf("provider %s has no suitable catalog model", provider)
	return subscription.Model{}
}

func TestNativeProfileReferenceTriStateAndModeSwitch(t *testing.T) {
	f := newNativeHTTPFixture(t)
	credential := createNativeProfileCredential(t, f, subscription.ProviderClaude)

	profileName := fmt.Sprintf("native-api-mode-switch-%d", time.Now().UnixNano())
	profileID, err := f.m.pg.SaveProfile(&db.LLMProfile{
		Name: profileName, Format: "anthropic", BaseURL: "https://legacy-gateway.invalid", Proxy: "http://proxy.invalid:8080",
		Model: "legacy-model", APIKey: "legacy-api-key", UserAgent: "LegacyClient/1.0", Streaming: false,
		PriceInputPerM: 1.25, PriceOutputPerM: 3.5, PriceCacheReadPerM: 0.25, PriceCacheWritePerM: 0.5,
	})
	if err != nil {
		t.Fatalf("seed API-key profile: %v", err)
	}
	f.cleanupProfileIDs = append(f.cleanupProfileIDs, profileID)

	legacyUpdate := f.request(http.MethodPost, "/api/llm/profiles", map[string]any{
		"id": profileID, "name": profileName, "format": "anthropic", "base_url": "https://legacy-gateway.invalid",
		"proxy": "__masked__", "model": "legacy-model", "user_agent": "LegacyClient/1.0", "streaming": false,
	}, f.token)
	if legacyUpdate.status != http.StatusOK {
		t.Fatalf("legacy update: HTTP %d %s", legacyUpdate.status, legacyUpdate.body.String())
	}
	legacy, err := f.m.pg.ProfileByID(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.AuthMode != "api_key" || legacy.CredentialID != nil || legacy.APIKey != "legacy-api-key" || legacy.Proxy != "http://proxy.invalid:8080" {
		t.Fatalf("legacy auth/key/proxy changed on absent native fields: %#v", legacy)
	}
	if legacy.PriceInputPerM != 1.25 || legacy.PriceOutputPerM != 3.5 || legacy.PriceCacheReadPerM != 0.25 || legacy.PriceCacheWritePerM != 0.5 {
		t.Fatalf("legacy prices changed when omitted: %#v", legacy)
	}

	model := catalogModel(t, subscription.ProviderClaude, true)
	overrideResponse := f.request(http.MethodPost, "/api/llm/profiles", map[string]any{
		"id": profileID, "name": profileName, "format": "anthropic", "model": model.ID,
		"auth_mode": "oauth", "credential_id": credential.ID,
		"base_url": "https://operator-destination.invalid", "user_agent": "SpoofedNativeIdentity/1.0",
	}, f.token)
	if overrideResponse.status != http.StatusBadRequest {
		t.Fatalf("native endpoint/identity override: HTTP %d %s", overrideResponse.status, overrideResponse.body.String())
	}
	switchResponse := f.request(http.MethodPost, "/api/llm/profiles", map[string]any{
		"id": profileID, "name": profileName, "format": "anthropic", "model": model.ID,
		"auth_mode": "oauth", "credential_id": credential.ID,
		"price_input_per_m": 9.0, "price_output_per_m": 11.0, "price_cache_read_per_m": 2.0, "price_cache_write_per_m": 3.0,
		"thinking_type": "disabled", "reasoning_effort": model.ReasoningEfforts[0], "max_tokens": model.MaxOutputTokens,
	}, f.token)
	if switchResponse.status != http.StatusOK {
		t.Fatalf("switch to native: HTTP %d %s", switchResponse.status, switchResponse.body.String())
	}
	native, err := f.m.pg.ProfileByID(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if native.AuthMode != "oauth" || native.CredentialID == nil || *native.CredentialID != credential.ID || native.AuthBindingGeneration != legacy.AuthBindingGeneration+1 {
		t.Fatalf("native binding = %#v", native)
	}
	if native.APIKey != "" || native.BaseURL != "" || native.UserAgent != "" {
		t.Fatalf("native profile retained operator credential destination/identity: %#v", native)
	}
	if !native.PoolExclude || native.IsDefault {
		t.Fatalf("new native mode was auto-pooled/activated: pool_exclude=%v default=%v", native.PoolExclude, native.IsDefault)
	}
	if native.PriceInputPerM != 0 || native.PriceOutputPerM != 0 || native.PriceCacheReadPerM != 0 || native.PriceCacheWritePerM != 0 {
		t.Fatalf("API token prices were represented as subscription charges: %#v", native)
	}

	generation := native.AuthBindingGeneration
	keepReference := f.request(http.MethodPost, "/api/llm/profiles", map[string]any{
		"id": profileID, "name": profileName, "format": "anthropic", "model": model.ID,
		"pool_exclude": true, "thinking_type": "disabled", "reasoning_effort": model.ReasoningEfforts[0], "max_tokens": model.MaxOutputTokens,
	}, f.token)
	if keepReference.status != http.StatusOK {
		t.Fatalf("native update with absent binding: HTTP %d %s", keepReference.status, keepReference.body.String())
	}
	kept, _ := f.m.pg.ProfileByID(profileID)
	if kept.AuthMode != "oauth" || kept.CredentialID == nil || *kept.CredentialID != credential.ID || kept.AuthBindingGeneration != generation {
		t.Fatalf("absent binding did not preserve identity: %#v", kept)
	}

	clearReference := f.request(http.MethodPost, "/api/llm/profiles", map[string]any{
		"id": profileID, "name": profileName, "format": "anthropic", "model": model.ID, "credential_id": nil,
	}, f.token)
	if clearReference.status != http.StatusBadRequest {
		t.Fatalf("null credential on OAuth profile: HTTP %d %s", clearReference.status, clearReference.body.String())
	}
	afterRejectedClear, _ := f.m.pg.ProfileByID(profileID)
	if afterRejectedClear.CredentialID == nil || *afterRejectedClear.CredentialID != credential.ID || afterRejectedClear.AuthBindingGeneration != generation {
		t.Fatalf("rejected clear mutated binding: %#v", afterRejectedClear)
	}

	blankKeySwitch := f.request(http.MethodPost, "/api/llm/profiles", map[string]any{
		"id": profileID, "name": profileName, "format": "anthropic", "model": "api-model", "auth_mode": "api_key", "credential_id": nil,
	}, f.token)
	if blankKeySwitch.status != http.StatusBadRequest {
		t.Fatalf("OAuth-to-API switch reused an absent key: HTTP %d %s", blankKeySwitch.status, blankKeySwitch.body.String())
	}
	explicitKeySwitch := f.request(http.MethodPost, "/api/llm/profiles", map[string]any{
		"id": profileID, "name": profileName, "format": "anthropic", "model": "api-model", "auth_mode": "api_key", "credential_id": nil,
		"api_key": "new-explicit-api-key", "base_url": "https://api-gateway.invalid", "user_agent": "APIClient/2.0",
	}, f.token)
	if explicitKeySwitch.status != http.StatusOK {
		t.Fatalf("explicit API-key switch: HTTP %d %s", explicitKeySwitch.status, explicitKeySwitch.body.String())
	}
	apiProfile, _ := f.m.pg.ProfileByID(profileID)
	if apiProfile.AuthMode != "api_key" || apiProfile.CredentialID != nil || apiProfile.APIKey != "new-explicit-api-key" || apiProfile.AuthBindingGeneration != generation+1 {
		t.Fatalf("explicit API-key switch did not replace identity atomically: %#v", apiProfile)
	}

	list := f.request(http.MethodGet, "/api/llm/profiles", nil, f.token)
	if list.status != http.StatusOK || strings.Contains(list.body.String(), "new-explicit-api-key") || strings.Contains(list.body.String(), "legacy-api-key") {
		t.Fatalf("profile DTO leaked key: HTTP %d %s", list.status, list.body.String())
	}
	if !strings.Contains(list.body.String(), `"auth_mode":"api_key"`) || !strings.Contains(list.body.String(), `"credential_id":null`) || !strings.Contains(list.body.String(), `"auth_binding_generation":`) {
		t.Fatalf("profile DTO omitted native binding fields: %s", list.body.String())
	}
}

func TestNativeProfileValidationAndPinnedModelCatalog(t *testing.T) {
	f := newNativeHTTPFixture(t)
	claudeCredential := createNativeProfileCredential(t, f, subscription.ProviderClaude)
	codexCredential := createNativeProfileCredential(t, f, subscription.ProviderCodex)
	claudeModel := catalogModel(t, subscription.ProviderClaude, false)
	codexModel := catalogModel(t, subscription.ProviderCodex, false)

	cases := []struct {
		name string
		body map[string]any
	}{
		{
			name: "wrong format for credential",
			body: map[string]any{"name": "native-api-wrong-format", "format": "openai-responses", "model": claudeModel.ID, "auth_mode": "oauth", "credential_id": claudeCredential.ID},
		},
		{
			name: "model outside selected catalog",
			body: map[string]any{"name": "native-api-wrong-model", "format": "anthropic", "model": codexModel.ID, "auth_mode": "oauth", "credential_id": claudeCredential.ID},
		},
		{
			name: "Codex unsupported output cap",
			body: map[string]any{"name": "native-api-codex-cap", "format": "openai-responses", "model": codexModel.ID, "auth_mode": "oauth", "credential_id": codexCredential.ID, "max_tokens": 1},
		},
		{
			name: "Claude cap above catalog hard limit",
			body: map[string]any{"name": "native-api-claude-cap", "format": "anthropic", "model": claudeModel.ID, "auth_mode": "oauth", "credential_id": claudeCredential.ID, "max_tokens": claudeModel.MaxOutputTokens + 1},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := f.request(http.MethodPost, "/api/llm/profiles", testCase.body, f.token)
			if response.status != http.StatusBadRequest {
				t.Fatalf("HTTP %d %s", response.status, response.body.String())
			}
		})
	}

	valid := f.request(http.MethodPost, "/api/llm/profiles", map[string]any{
		"name": "native-api-explicit-pool-opt-in", "format": "anthropic", "model": claudeModel.ID,
		"auth_mode": "oauth", "credential_id": claudeCredential.ID, "pool_exclude": false,
	}, f.token)
	if valid.status != http.StatusOK {
		t.Fatalf("valid native profile: HTTP %d %s", valid.status, valid.body.String())
	}
	profileID := int64(valid.mapBody(t)["id"].(float64))
	f.cleanupProfileIDs = append(f.cleanupProfileIDs, profileID)
	stored, _ := f.m.pg.ProfileByID(profileID)
	if stored == nil || stored.PoolExclude || stored.IsDefault {
		t.Fatalf("explicit native pool opt-in or activation state was wrong: %#v", stored)
	}

	codexRevision := codexCredential.Revision
	codexCredential.Status = subscription.CredentialRevoked
	if err := f.m.pg.UpdateLLMCredential(t.Context(), codexCredential, codexRevision); err != nil {
		t.Fatalf("mark Codex credential revoked: %v", err)
	}

	var arbitraryEndpointHits atomic.Int32
	arbitraryEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arbitraryEndpointHits.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(arbitraryEndpoint.Close)
	catalog := f.request(http.MethodPost, "/api/llm/models", map[string]any{
		"provider": "openai-responses", "auth_mode": "oauth", "credential_id": codexCredential.ID,
		"base_url": arbitraryEndpoint.URL, "api_key": "must-not-be-sent",
	}, f.token)
	if catalog.status != http.StatusOK || arbitraryEndpointHits.Load() != 0 {
		t.Fatalf("native model catalog contacted arbitrary endpoint: HTTP %d hits=%d body=%s", catalog.status, arbitraryEndpointHits.Load(), catalog.body.String())
	}
	catalogBody := catalog.mapBody(t)
	if catalogBody["source"] != "pinned_catalog" || catalogBody["availability"] != "unknown" || catalogBody["ok"] != true {
		t.Fatalf("catalog truth markers = %#v", catalogBody)
	}
	models, ok := catalogBody["models"].([]any)
	if !ok || len(models) == 0 {
		t.Fatalf("catalog models = %#v", catalogBody["models"])
	}
	first, ok := models[0].(map[string]any)
	if !ok || first["provider"] != subscription.ProviderCodex || first["supports_output_cap"] != false || first["context_window_k"] == nil || first["supports_tools"] != true {
		t.Fatalf("catalog capability shape = %#v", first)
	}

	mismatchedIdentity := f.request(http.MethodPost, "/api/llm/models", map[string]any{
		"provider": "anthropic", "auth_mode": "oauth", "profile_id": profileID, "credential_id": codexCredential.ID,
	}, f.token)
	if mismatchedIdentity.status != http.StatusBadRequest {
		t.Fatalf("model catalog accepted a different selected credential: HTTP %d %s", mismatchedIdentity.status, mismatchedIdentity.body.String())
	}
}

func TestAPIAndOllamaProfilesPreserveLegacyFields(t *testing.T) {
	f := newNativeHTTPFixture(t)
	apiName := fmt.Sprintf("native-api-preserve-%d", time.Now().UnixNano())
	apiID, err := f.m.pg.SaveProfile(&db.LLMProfile{
		Name: apiName, Format: "openai", BaseURL: "https://api.example.invalid/v1", Model: "api-model", APIKey: "preserved-api-key",
		Streaming: false, MaxTokens: 4096, MaxTokensField: "max_completion_tokens", UserAgent: "PreservedAPI/1.0",
		PriceInputPerM: 2.0, PriceOutputPerM: 4.0, PriceCacheReadPerM: 0.2, PriceCacheWritePerM: 0.4,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.cleanupProfileIDs = append(f.cleanupProfileIDs, apiID)
	response := f.request(http.MethodPost, "/api/llm/profiles", map[string]any{
		"id": apiID, "name": apiName, "format": "openai", "base_url": "https://api.example.invalid/v1", "model": "api-model",
		"streaming": false, "max_tokens": 4096, "max_tokens_field": "max_completion_tokens", "user_agent": "PreservedAPI/1.0",
	}, f.token)
	if response.status != http.StatusOK {
		t.Fatalf("API profile update: HTTP %d %s", response.status, response.body.String())
	}
	apiProfile, _ := f.m.pg.ProfileByID(apiID)
	if apiProfile.APIKey != "preserved-api-key" || apiProfile.AuthMode != "api_key" || apiProfile.CredentialID != nil || apiProfile.MaxTokens != 4096 || apiProfile.MaxTokensField != "max_completion_tokens" || apiProfile.Streaming {
		t.Fatalf("API profile behavior regressed: %#v", apiProfile)
	}
	if apiProfile.PriceInputPerM != 2.0 || apiProfile.PriceOutputPerM != 4.0 || apiProfile.PriceCacheReadPerM != 0.2 || apiProfile.PriceCacheWritePerM != 0.4 {
		t.Fatalf("API price fields were not preserved: %#v", apiProfile)
	}
	list := f.request(http.MethodGet, "/api/llm/profiles", nil, f.token)
	if list.status != http.StatusOK || !strings.Contains(list.body.String(), `"price_input_per_m":2`) || !strings.Contains(list.body.String(), `"price_output_per_m":4`) {
		t.Fatalf("API price fields missing from profile DTO: HTTP %d %s", list.status, list.body.String())
	}

	ollamaName := fmt.Sprintf("native-api-ollama-%d", time.Now().UnixNano())
	ollama := f.request(http.MethodPost, "/api/llm/profiles", map[string]any{
		"name": ollamaName, "format": "ollama", "model": "llama-fixture", "base_url": "http://127.0.0.1:11434/v1", "api_key": "",
	}, f.token)
	if ollama.status != http.StatusOK {
		t.Fatalf("keyless Ollama profile: HTTP %d %s", ollama.status, ollama.body.String())
	}
	ollamaID := int64(ollama.mapBody(t)["id"].(float64))
	f.cleanupProfileIDs = append(f.cleanupProfileIDs, ollamaID)
	ollamaProfile, _ := f.m.pg.ProfileByID(ollamaID)
	if ollamaProfile == nil || ollamaProfile.AuthMode != "api_key" || ollamaProfile.CredentialID != nil || ollamaProfile.APIKey != "" || !ollamaProfile.Streaming {
		t.Fatalf("Ollama defaults changed: %#v", ollamaProfile)
	}
}
