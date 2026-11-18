package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/norma/llm"
	"github.com/Autumn-27/artex/subscription"
)

func TestNativeProfileKeylessRevocationNeverUsesEnvironmentAPIKey(t *testing.T) {
	skipWithoutDSN(t)
	t.Setenv("ANTHROPIC_API_KEY", "environment-key-must-not-be-used")
	t.Setenv("OPENAI_API_KEY", "environment-key-must-not-be-used")

	var outbound atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		outbound.Add(1)
		http.Error(w, "unexpected model request", http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)

	manager, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	credential, err := manager.pg.CreateLLMCredential(ctx, subscription.ProviderClaude, fmt.Sprintf("native-revoked-%d", stamp), json.RawMessage(`{}`))
	if err != nil {
		manager.Close()
		t.Fatal(err)
	}
	credential.Status = subscription.CredentialRevoked
	if err := manager.pg.UpdateLLMCredential(ctx, credential, credential.Revision); err != nil {
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		manager.Close()
		t.Fatal(err)
	}
	profileID, err := manager.pg.SaveProfile(&db.LLMProfile{
		Name:         fmt.Sprintf("native-revoked-profile-%d", stamp),
		Format:       "anthropic",
		Model:        "claude-sonnet-4-5",
		Proxy:        proxy.URL,
		AuthMode:     "oauth",
		CredentialID: &credential.ID,
		PoolExclude:  true,
	})
	if err != nil {
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		manager.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = manager.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profileID)
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		_ = manager.Close()
	})

	s := &Server{m: manager, provByProfile: map[int64]*provEntry{}}
	cfg, ok := s.loadProfileConfig(profileID)
	if !ok {
		t.Fatal("revoked native profile must remain configured")
	}
	if cfg.APIKey != "" || cfg.NeedsAPIKey() {
		t.Fatalf("native config borrowed a raw key: key=%q needs_key=%v", cfg.APIKey, cfg.NeedsAPIKey())
	}
	if cfg.ProfileID != profileID || cfg.CredentialID != credential.ID || cfg.AuthMode != "oauth" {
		t.Fatalf("native binding = profile %d credential %d mode %q", cfg.ProfileID, cfg.CredentialID, cfg.AuthMode)
	}
	withUserAgent := cfg
	withUserAgent.UserAgent = "operator-override"
	if _, err := withUserAgent.NewProvider(manager.Subscriptions()); !subscription.IsTerminal(err) {
		t.Fatalf("native User-Agent override error = %T %v, want terminal validation", err, err)
	}
	withBaseURL := cfg
	withBaseURL.BaseURL = "https://operator.invalid"
	if _, err := withBaseURL.NewProvider(manager.Subscriptions()); !subscription.IsTerminal(err) {
		t.Fatalf("native base URL override error = %T %v, want terminal validation", err, err)
	}
	provider, err := cfg.NewProvider(manager.Subscriptions())
	if err != nil {
		t.Fatalf("build selected native provider: %v", err)
	}
	_, _, _, err = provider.Complete(ctx, llm.CompletionRequest{})
	if !subscription.IsTerminal(err) {
		t.Fatalf("revoked native completion error = %T %v, want terminal native error", err, err)
	}
	if outbound.Load() != 0 {
		t.Fatalf("revoked native profile dispatched %d model requests", outbound.Load())
	}
}

func TestInvalidSelectedNativeProfileBlocksGlobalAndEnvironmentFallback(t *testing.T) {
	skipWithoutDSN(t)
	t.Setenv("ANTHROPIC_API_KEY", "environment-key-must-not-be-used")

	manager, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	credential, err := manager.pg.CreateLLMCredential(ctx, subscription.ProviderClaude, fmt.Sprintf("native-invalid-%d", stamp), json.RawMessage(`{}`))
	if err != nil {
		manager.Close()
		t.Fatal(err)
	}
	profileID, err := manager.pg.SaveProfile(&db.LLMProfile{
		Name:         fmt.Sprintf("native-invalid-profile-%d", stamp),
		Format:       "anthropic",
		Model:        "not-in-the-native-catalog",
		AuthMode:     "oauth",
		CredentialID: &credential.ID,
		PoolExclude:  true,
	})
	if err != nil {
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		manager.Close()
		t.Fatal(err)
	}
	previousActive, _ := manager.pg.ActiveProfile()
	if err := manager.pg.SetActiveProfile(profileID); err != nil {
		_, _ = manager.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profileID)
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		manager.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = manager.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, profileID)
		_, _ = manager.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profileID)
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		if previousActive != nil {
			_ = manager.pg.SetActiveProfile(previousActive.ID)
		}
		_ = manager.Close()
	})

	global := &agent.ChatAgent{}
	s := &Server{
		m:              manager,
		chatAgent:      global,
		profChatAgents: map[int64]*agent.ChatAgent{},
		provByProfile:  map[int64]*provEntry{},
	}
	cfg, ok := s.loadLLMConfig()
	if !ok || cfg.AuthMode != "oauth" {
		t.Fatalf("active native config = %#v, ok=%v", cfg, ok)
	}
	if _, err := cfg.NewProvider(manager.Subscriptions()); !subscription.IsTerminal(err) {
		t.Fatalf("invalid native provider error = %T %v, want terminal native validation", err, err)
	}
	conversation := &db.Conversation{LLMProfileID: &profileID}
	if resolved := s.resolveChatAgent(conversation); resolved != nil {
		t.Fatal("selected invalid native profile fell back to the global API provider")
	}
	if err := s.nativeProfileSelectionError(profileID); !subscription.IsTerminal(err) {
		t.Fatalf("selection guard error = %T %v, want terminal native error", err, err)
	}
}

func TestLegacyLLMConfigSavePreservesNativeReference(t *testing.T) {
	skipWithoutDSN(t)
	manager, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	credential, err := manager.pg.CreateLLMCredential(ctx, subscription.ProviderCodex, fmt.Sprintf("native-legacy-%d", stamp), json.RawMessage(`{}`))
	if err != nil {
		manager.Close()
		t.Fatal(err)
	}
	profileID, err := manager.pg.SaveProfile(&db.LLMProfile{
		Name:         "default",
		Format:       "openai-responses",
		Model:        "gpt-5.6-sol",
		AuthMode:     "oauth",
		CredentialID: &credential.ID,
		PoolExclude:  true,
		Streaming:    true,
	})
	if err != nil {
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		manager.Close()
		t.Fatal(err)
	}
	previousActive, _ := manager.pg.ActiveProfile()
	if err := manager.pg.SetActiveProfile(profileID); err != nil {
		_, _ = manager.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profileID)
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		manager.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = manager.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, profileID)
		_, _ = manager.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profileID)
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		if previousActive != nil {
			_ = manager.pg.SetActiveProfile(previousActive.ID)
		}
		_ = manager.Close()
	})

	s := &Server{
		m:              manager,
		ctx:            context.Background(),
		profChatAgents: map[int64]*agent.ChatAgent{},
		provByProfile:  map[int64]*provEntry{},
		taskAgents:     map[string]*taskAgentBundle{},
		llmHealth:      newLLMHealthRegistry(manager.pg),
	}
	cfg, ok := s.loadLLMConfig()
	if !ok {
		t.Fatal("native default did not load")
	}
	s.llmCfg, s.llmOn, s.llmProf = cfg, true, "default"
	generation := cfg.AuthBindingGeneration
	body, err := json.Marshal(map[string]any{
		"provider": "openai-responses",
		"model":    "gpt-5.6-sol",
		"base_url": "",
		"proxy":    "",
		"api_key":  "",
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	s.setLLM(recorder, httptest.NewRequest(http.MethodPost, "/api/llm", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("legacy set status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		AuthMode              string `json:"auth_mode"`
		CredentialID          *int64 `json:"credential_id"`
		AuthBindingGeneration int64  `json:"auth_binding_generation"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.AuthMode != "oauth" || response.CredentialID == nil || *response.CredentialID != credential.ID || response.AuthBindingGeneration != generation {
		t.Fatalf("legacy response lost native reference: %+v", response)
	}
	after, err := manager.pg.ProfileByID(profileID)
	if err != nil || after == nil {
		t.Fatalf("reload native default: %v", err)
	}
	if after.AuthMode != "oauth" || after.CredentialID == nil || *after.CredentialID != credential.ID {
		t.Fatalf("legacy save lost native reference: mode=%q credential=%v", after.AuthMode, after.CredentialID)
	}
	if after.AuthBindingGeneration != generation {
		t.Fatalf("unchanged native binding generation = %d, want %d", after.AuthBindingGeneration, generation)
	}
}

func TestManagerNativeLifecycleIsLazyAndCloseCancelsRequests(t *testing.T) {
	skipWithoutDSN(t)
	// Construction must not resolve or bind this host; callback listeners are
	// created only after an authenticated Begin call.
	t.Setenv("ARTEX_OAUTH_CALLBACK_LISTEN_HOST", "not-a-local-listener.invalid")
	manager, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	subscriptions := manager.Subscriptions()
	if subscriptions == nil {
		manager.Close()
		t.Fatal("native subscription manager was not initialized")
	}
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	credential, err := manager.pg.CreateLLMCredential(ctx, subscription.ProviderClaude, fmt.Sprintf("native-close-%d", stamp), json.RawMessage(`{}`))
	if err != nil {
		manager.Close()
		t.Fatal(err)
	}
	profileID, err := manager.pg.SaveProfile(&db.LLMProfile{
		Name:         fmt.Sprintf("native-close-profile-%d", stamp),
		Format:       "anthropic",
		Model:        "claude-sonnet-4-5",
		AuthMode:     "oauth",
		CredentialID: &credential.ID,
		PoolExclude:  true,
	})
	if err != nil {
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		manager.Close()
		t.Fatal(err)
	}
	profile, err := manager.pg.ProfileByID(profileID)
	if err != nil || profile == nil {
		manager.Close()
		t.Fatalf("reload profile: %v", err)
	}
	requestCtx, release, err := subscriptions.RegisterRequest(ctx, subscription.Binding{
		ProfileID: profileID, CredentialID: credential.ID, Generation: profile.AuthBindingGeneration,
	})
	if err != nil {
		manager.Close()
		t.Fatalf("register native request: %v", err)
	}
	_, _ = manager.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profileID)
	_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
	manager.BeginNativeShutdown()
	select {
	case <-requestCtx.Done():
	case <-time.After(time.Second):
		release()
		t.Fatal("native shutdown did not cancel an admitted request")
	}
	if _, err := manager.pg.Exec(`SELECT 1`); err != nil {
		release()
		t.Fatalf("native shutdown closed the database before task settlement: %v", err)
	}
	if err := manager.Close(); err != nil {
		release()
		t.Fatalf("close manager: %v", err)
	}
	defer release()
	if manager.Subscriptions() != nil {
		t.Fatal("closed manager still exposes native admission")
	}
	if _, err := subscriptions.Begin(ctx, "owner", subscription.ProviderClaude, 0); !subscription.IsTerminal(err) {
		t.Fatalf("Begin after close error = %T %v, want terminal closed error", err, err)
	}
}
