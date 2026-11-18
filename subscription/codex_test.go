package subscription

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/internal/norma/llm"
)

type codexFixtureTransport struct {
	base   http.RoundTripper
	target *url.URL
	mu     sync.Mutex
	seen   []string
}

func (transport *codexFixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	requestURL := *request.URL
	transport.mu.Lock()
	transport.seen = append(transport.seen, request.URL.String())
	transport.mu.Unlock()
	requestURL.Scheme = transport.target.Scheme
	requestURL.Host = transport.target.Host
	clone.URL = &requestURL
	clone.Host = transport.target.Host
	return transport.base.RoundTrip(clone)
}

func (transport *codexFixtureTransport) origins() []string {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([]string(nil), transport.seen...)
}

type codexCapturedRequest struct {
	Header http.Header
	Body   map[string]any
}

type codexFixture struct {
	provider  llm.Provider
	store     *testStore
	binding   Binding
	transport *codexFixtureTransport
}

func newCodexFixture(t *testing.T, handler http.Handler, config func(*ProviderConfig)) *codexFixture {
	t.Helper()
	server := httptest.NewServer(handler)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse fixture URL: %v", err)
	}
	baseTransport := &http.Transport{}
	transport := &codexFixtureTransport{base: baseTransport, target: target}
	fixtureNow := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	store := newTestStore()
	credentialID := store.seed(ProviderCodex, CredentialReady, TokenSet{
		AccessToken:  "synthetic-access-one",
		RefreshToken: "synthetic-refresh",
		ExpiresAt:    fixtureNow.Add(time.Hour),
		AccountID:    "acct-fixture",
		Subject:      "subject-fixture",
	})
	binding := Binding{ProfileID: 81, CredentialID: credentialID, Generation: 4}
	store.bind(binding.ProfileID, binding.CredentialID, binding.Generation)
	manager := NewManager(store, Options{
		Drivers: []OAuthDriver{&testDriver{kind: ProviderCodex}},
		Now:     func() time.Time { return fixtureNow },
	})
	providerConfig := ProviderConfig{
		Binding:         binding,
		Model:           "gpt-5.6-sol",
		ThinkingType:    "enabled",
		ReasoningEffort: "high",
		HTTPClient:      &http.Client{Transport: transport},
	}
	if config != nil {
		config(&providerConfig)
	}
	provider, err := NewCodexProvider(manager, providerConfig)
	if err != nil {
		server.Close()
		t.Fatalf("NewCodexProvider: %v", err)
	}
	fixture := &codexFixture{
		provider: provider, store: store, binding: binding, transport: transport,
	}
	t.Cleanup(func() {
		server.Close()
		baseTransport.CloseIdleConnections()
		_ = manager.Close(context.Background())
	})
	return fixture
}

func setCodexFixtureAccessToken(t *testing.T, fixture *codexFixture, accessToken string) {
	t.Helper()
	fixture.store.mu.Lock()
	defer fixture.store.mu.Unlock()
	record := fixture.store.creds[fixture.binding.CredentialID]
	var payload credentialPayload
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		t.Fatalf("decode credential: %v", err)
	}
	payload.Tokens.AccessToken = accessToken
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode credential: %v", err)
	}
	record.Payload = encoded
}

func codexFixtureJWT(accountID, residency string) string {
	payload, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]string{
			"chatgpt_account_id":     accountID,
			"chatgpt_data_residency": residency,
		},
	})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func codexSSE(frames ...string) string {
	var stream strings.Builder
	for _, frame := range frames {
		var envelope struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(frame), &envelope)
		if envelope.Type != "" {
			stream.WriteString("event: ")
			stream.WriteString(envelope.Type)
			stream.WriteByte('\n')
		}
		stream.WriteString("data: ")
		stream.WriteString(frame)
		stream.WriteString("\n\n")
	}
	return stream.String()
}

func codexTextSuccess(text string) string {
	encoded, _ := json.Marshal(text)
	return codexSSE(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":`+string(encoded)+`}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":`+string(encoded)+`}]}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15,"input_tokens_details":{"cached_tokens":3}}}}`,
	)
}

func captureCodexRequest(request *http.Request) (codexCapturedRequest, error) {
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		return codexCapturedRequest{}, err
	}
	return codexCapturedRequest{Header: request.Header.Clone(), Body: body}, nil
}

func codexAssertEmptyReasoningSummary(t *testing.T, body map[string]any, encrypted string) ([]any, int) {
	t.Helper()
	input, ok := body["input"].([]any)
	if !ok {
		t.Fatalf("Codex input = %#v, want array", body["input"])
	}
	found := -1
	for index, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if item["type"] != "reasoning" {
			if _, present := item["summary"]; present {
				t.Fatalf("non-reasoning input gained summary: %#v", item)
			}
			continue
		}
		if item["encrypted_content"] != encrypted {
			continue
		}
		summary, present := item["summary"]
		if !present {
			t.Fatalf("reasoning input omitted summary: %#v", item)
		}
		parts, ok := summary.([]any)
		if !ok || len(parts) != 0 {
			t.Fatalf("reasoning input summary = %#v, want []", summary)
		}
		found = index
	}
	if found < 0 {
		t.Fatalf("reasoning input for %q not found: %#v", encrypted, input)
	}
	return input, found
}

func TestCodexModelsPinnedCapabilities(t *testing.T) {
	models := CodexModels()
	wantIDs := []string{
		"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-6-astra",
		"gpt-6-luna", "gpt-6-sol", "gpt-6.1-sol", "gpt-daybreak-blue-latest",
	}
	wantContextWindowK := []int{1000, 1000, 1000, 272, 272, 272, 500, 272}
	if len(models) != len(wantIDs) {
		t.Fatalf("CodexModels length = %d, want %d", len(models), len(wantIDs))
	}
	for index, model := range models {
		if model.ID != wantIDs[index] {
			t.Fatalf("model[%d].ID = %q, want %q", index, model.ID, wantIDs[index])
		}
		if model.Provider != ProviderCodex || !model.SupportsTools || model.SupportsOutputCap || model.SupportsThinkingDisable {
			t.Fatalf("model[%d] capabilities = %+v", index, model)
		}
		if model.ContextWindowK != wantContextWindowK[index] || model.MaxOutputTokens != 128000 ||
			strings.Join(model.ReasoningEfforts, ",") != "low,medium,high,xhigh,max" {
			t.Fatalf("model[%d] limits/efforts = %+v", index, model)
		}
	}
	models[0].ReasoningEfforts[0] = "changed"
	if CodexModels()[0].ReasoningEfforts[0] != "low" {
		t.Fatal("CodexModels returned shared mutable effort data")
	}
}

func TestCodexRequestUsesFixedSubscriptionWireContract(t *testing.T) {
	requests := make(chan codexCapturedRequest, 1)
	fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured, err := captureCodexRequest(request)
		if err != nil {
			t.Errorf("capture request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- captured
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, codexTextSuccess("hello"))
	}), nil)

	temperature := 0.25
	message, stop, usage, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{
		System:   []string{"system one", "system two"},
		Messages: []llm.Message{llm.UserText("hello")},
		Tools: []llm.ToolSchema{{
			Name: "read", Description: "Read a file",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
		}},
		MaxTokens:   777,
		Temperature: &temperature,
		Stop:        []string{"unsupported"},
		Thinking:    "disabled",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if message.Text() != "hello" || stop != "end_turn" || usage.InputTokens != 11 || usage.CacheReadTokens != 3 || usage.OutputTokens != 4 {
		t.Fatalf("completion = text %q stop %q usage %+v", message.Text(), stop, usage)
	}
	captured := <-requests
	if got := fixture.transport.origins(); len(got) != 1 || got[0] != codexResponsesURL {
		t.Fatalf("original request URLs = %v, want fixed %q", got, codexResponsesURL)
	}
	if captured.Header.Get("Authorization") != "Bearer synthetic-access-one" ||
		captured.Header.Get("chatgpt-account-id") != "acct-fixture" ||
		captured.Header.Get("originator") != "omp" || captured.Header.Get("version") != codexClientVersion ||
		captured.Header.Get("x-codex-routing-hint") != "model=gpt-5.6-sol" ||
		captured.Header.Get("Accept") != "text/event-stream" || captured.Header.Get("Content-Type") != "application/json" ||
		captured.Header.Get("User-Agent") != codexUserAgent || captured.Header.Get("x-api-key") != "" ||
		captured.Header.Get("OpenAI-Beta") != "" {
		t.Fatalf("Codex request headers = %#v", captured.Header)
	}
	if captured.Body["model"] != "gpt-5.6-sol" || captured.Body["stream"] != true || captured.Body["store"] != false {
		t.Fatalf("Codex request body basics = %#v", captured.Body)
	}
	for _, unsupported := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens", "temperature", "stop"} {
		if _, present := captured.Body[unsupported]; present {
			t.Fatalf("unsupported field %q reached Codex: %#v", unsupported, captured.Body)
		}
	}
	reasoning, ok := captured.Body["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "low" {
		t.Fatalf("disabled thinking reasoning = %#v, want honest lowest effort", captured.Body["reasoning"])
	}
	if _, present := reasoning["summary"]; present {
		t.Fatalf("disabled thinking requested a summary: %#v", reasoning)
	}
	tools, ok := captured.Body["tools"].([]any)
	if !ok || len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" {
		t.Fatalf("Codex tools = %#v", captured.Body["tools"])
	}
	encoded, _ := json.Marshal(captured.Body)
	if bytes.Contains(encoded, []byte("web_search")) || bytes.Contains(encoded, []byte("computer")) {
		t.Fatalf("hosted tool leaked onto wire: %s", encoded)
	}
	if bytes.Contains(encoded, []byte("synthetic-access")) || bytes.Contains(encoded, []byte("synthetic-refresh")) {
		t.Fatalf("credential material leaked into Codex body: %s", encoded)
	}
	input := captured.Body["input"].([]any)
	if input[0].(map[string]any)["role"] != "developer" {
		t.Fatalf("second system segment was not a developer input: %#v", input)
	}
}

func TestCodexToollessInternalCompletionOmitsWireOutputCap(t *testing.T) {
	requests := make(chan codexCapturedRequest, 1)
	fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured, err := captureCodexRequest(request)
		if err != nil {
			t.Errorf("capture request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- captured
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, codexTextSuccess("summary"))
	}), nil)

	message, stop, _, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{
		Messages:  []llm.Message{llm.UserText("summarize")},
		MaxTokens: 4096,
		Thinking:  "disabled",
	})
	if err != nil || message.Text() != "summary" || stop != "end_turn" {
		t.Fatalf("tool-less Complete = text %q stop %q error %v", message.Text(), stop, err)
	}
	body := (<-requests).Body
	if _, present := body["tools"]; present {
		t.Fatalf("tool-less completion sent tools: %#v", body["tools"])
	}
	for _, capField := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		if _, present := body[capField]; present {
			t.Fatalf("internal MaxTokens leaked as %q: %#v", capField, body)
		}
	}
}

func TestCodexInterleavedToolsAndOpaqueJSONResume(t *testing.T) {
	var mu sync.Mutex
	var captured []codexCapturedRequest
	requestNumber := 0
	fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := captureCodexRequest(request)
		if err != nil {
			t.Errorf("capture request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		captured = append(captured, body)
		requestNumber++
		current := requestNumber
		mu.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		if current != 1 {
			_, _ = io.WriteString(writer, codexTextSuccess("resumed"))
			return
		}
		_, _ = io.WriteString(writer, codexSSE(
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"planning"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"planning"}],"encrypted_content":"opaque-reasoning-one"}}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_a","name":"read","arguments":""}}`,
			`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_2","call_id":"call_b","name":"write","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"path\":"}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_2","output_index":2,"delta":"{\"path\":"}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"\"a\"}"}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_2","output_index":2,"delta":"\"b\"}"}`,
			`{"type":"response.function_call_arguments.done","item_id":"fc_2","output_index":2,"arguments":"{\"path\":\"b\"}"}`,
			`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":1,"arguments":"{\"path\":\"a\"}"}`,
			`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_2","call_id":"call_b","name":"write","arguments":"{\"path\":\"b\"}"}}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_a","name":"read","arguments":"{\"path\":\"a\"}"}}`,
			`{"type":"response.completed","response":{"id":"resp_tools","status":"completed","usage":{"input_tokens":23,"output_tokens":9,"input_tokens_details":{"cached_tokens":7}}}}`,
		))
	}), nil)

	accumulator := llm.NewAccumulator()
	var streamErr error
	var toolInputIDs []string
	for event, eventErr := range fixture.provider.Stream(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{llm.UserText("use tools")},
	}) {
		if eventErr != nil {
			streamErr = eventErr
			continue
		}
		if event.Type == llm.SEToolInputJSON {
			toolInputIDs = append(toolInputIDs, event.ToolID)
		}
		accumulator.Add(event)
	}
	if streamErr != nil {
		t.Fatalf("first Stream: %v", streamErr)
	}
	first, stop, usage := accumulator.Message(), accumulator.StopReason, accumulator.Usage
	if stop != "tool_use" || usage.InputTokens != 23 || usage.OutputTokens != 9 || usage.CacheReadTokens != 7 {
		t.Fatalf("first stop/usage = %q %+v", stop, usage)
	}
	if strings.Join(toolInputIDs, ",") != "call_a,call_b" {
		t.Fatalf("tool input routing IDs = %v", toolInputIDs)
	}
	if len(first.Content) != 3 || first.Content[0].Type != llm.BlockThinking || first.Content[0].Thinking != "planning" {
		t.Fatalf("first content = %#v", first.Content)
	}
	if first.Content[1].ID != "call_a" || string(first.Content[1].Input) != `{"path":"a"}` ||
		first.Content[2].ID != "call_b" || string(first.Content[2].Input) != `{"path":"b"}` {
		t.Fatalf("interleaved calls were mixed: %#v", first.Content)
	}
	if first.ProviderState == nil {
		t.Fatal("Codex response omitted provider state")
	}
	if bytes.Contains(first.ProviderState.Data, []byte("planning")) || bytes.Contains(first.ProviderState.Data, []byte(`"path"`)) {
		t.Fatalf("provider state duplicated visible transcript: %s", first.ProviderState.Data)
	}
	if !bytes.Contains(first.ProviderState.Data, []byte("opaque-reasoning-one")) {
		t.Fatalf("provider state lost encrypted reasoning: %s", first.ProviderState.Data)
	}

	serialized, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first response: %v", err)
	}
	var resumed llm.Message
	if err := json.Unmarshal(serialized, &resumed); err != nil {
		t.Fatalf("unmarshal first response: %v", err)
	}
	history := []llm.Message{
		llm.UserText("use tools"),
		resumed,
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.ToolResultText("call_a", "A", false), llm.ToolResultText("call_b", "B", false),
		}},
	}
	if _, _, _, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{Messages: history}); err != nil {
		t.Fatalf("resumed Complete: %v", err)
	}

	mismatchedScope := resumed
	scopeState := *mismatchedScope.ProviderState
	scopeState.AuthScope = "different-account-scope"
	mismatchedScope.ProviderState = &scopeState
	history[1] = mismatchedScope
	if _, _, _, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{Messages: history}); err != nil {
		t.Fatalf("mismatched-scope Complete: %v", err)
	}

	mismatchedModel := resumed
	modelState := *mismatchedModel.ProviderState
	modelState.Model = "gpt-6-sol"
	mismatchedModel.ProviderState = &modelState
	history[1] = mismatchedModel
	if _, _, _, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{Messages: history}); err != nil {
		t.Fatalf("mismatched-model Complete: %v", err)
	}

	history[1] = resumed
	if _, _, _, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{
		Messages: history,
		Thinking: "disabled",
	}); err != nil {
		t.Fatalf("disabled-thinking Complete: %v", err)
	}

	mu.Lock()
	requests := append([]codexCapturedRequest(nil), captured...)
	mu.Unlock()
	if len(requests) != 5 {
		t.Fatalf("captured %d requests, want 5", len(requests))
	}
	validInput, _ := json.Marshal(requests[1].Body["input"])
	if !bytes.Contains(validInput, []byte(`"type":"reasoning"`)) ||
		!bytes.Contains(validInput, []byte("opaque-reasoning-one")) ||
		!bytes.Contains(validInput, []byte(`"type":"function_call"`)) ||
		!bytes.Contains(validInput, []byte(`"type":"function_call_output"`)) {
		t.Fatalf("same-scope resumed input lost native history: %s", validInput)
	}
	for index, mismatch := range []string{"scope", "model", "disabled thinking"} {
		input, _ := json.Marshal(requests[index+2].Body["input"])
		if bytes.Contains(input, []byte("opaque-reasoning-one")) || bytes.Contains(input, []byte(`"type":"reasoning"`)) {
			t.Fatalf("mismatched %s replayed opaque reasoning: %s", mismatch, input)
		}
		if !bytes.Contains(input, []byte(`"type":"function_call"`)) {
			t.Fatalf("mismatched %s lost neutral tool history: %s", mismatch, input)
		}
	}
	disabledReasoning, ok := requests[4].Body["reasoning"].(map[string]any)
	if !ok || disabledReasoning["effort"] != "low" {
		t.Fatalf("disabled override reasoning = %#v", requests[4].Body["reasoning"])
	}
	if _, present := disabledReasoning["summary"]; present {
		t.Fatalf("disabled override requested reasoning summaries: %#v", disabledReasoning)
	}
}

func TestCodexReasoningReplayAlwaysIncludesSummary(t *testing.T) {
	tests := []struct {
		name         string
		thinking     string
		summaryField string
		encrypted    string
	}{
		{name: "empty upstream summary", thinking: "enabled", summaryField: `,"summary":[]`, encrypted: "opaque-empty-summary"},
		{name: "disabled thinking without summary", thinking: "disabled", encrypted: "opaque-disabled-summary"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requestMu sync.Mutex
			var requests []codexCapturedRequest
			fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				captured, err := captureCodexRequest(request)
				if err != nil {
					t.Errorf("capture request: %v", err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				requestMu.Lock()
				requests = append(requests, captured)
				requestNumber := len(requests)
				requestMu.Unlock()
				writer.Header().Set("Content-Type", "text/event-stream")
				if requestNumber == 1 {
					_, _ = io.WriteString(writer, codexSSE(
						`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_empty"`+test.summaryField+`}}`,
						`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_empty","encrypted_content":"`+test.encrypted+`"`+test.summaryField+`}}`,
						`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}`,
					))
					return
				}
				_, _ = io.WriteString(writer, codexTextSuccess("resumed"))
			}), nil)

			first, _, _, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{
				Messages: []llm.Message{llm.UserText("first")},
				Thinking: test.thinking,
			})
			if err != nil {
				t.Fatalf("first Complete: %v", err)
			}
			if first.ProviderState == nil {
				t.Fatal("summary-less reasoning omitted provider state")
			}
			transcript, err := json.Marshal(first)
			if err != nil {
				t.Fatalf("marshal transcript: %v", err)
			}
			var resumed llm.Message
			if err := json.Unmarshal(transcript, &resumed); err != nil {
				t.Fatalf("unmarshal transcript: %v", err)
			}
			if _, _, _, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{
				llm.UserText("first"),
				resumed,
				llm.UserText("continue"),
			}}); err != nil {
				t.Fatalf("resumed Complete: %v", err)
			}
			requestMu.Lock()
			captured := append([]codexCapturedRequest(nil), requests...)
			requestMu.Unlock()
			if len(captured) != 2 {
				t.Fatalf("captured %d requests, want 2", len(captured))
			}
			input, reasoningIndex := codexAssertEmptyReasoningSummary(t, captured[1].Body, test.encrypted)
			if reasoningIndex != 1 || input[0].(map[string]any)["role"] != "user" ||
				input[2].(map[string]any)["role"] != "user" {
				t.Fatalf("summary-less replay order = %#v", input)
			}
		})
	}

	t.Run("invalid summary span falls back to empty summary in place", func(t *testing.T) {
		var requestMu sync.Mutex
		var requests []codexCapturedRequest
		fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			captured, err := captureCodexRequest(request)
			if err != nil {
				t.Errorf("capture request: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			requestMu.Lock()
			requests = append(requests, captured)
			requestNumber := len(requests)
			requestMu.Unlock()
			writer.Header().Set("Content-Type", "text/event-stream")
			if requestNumber == 1 {
				_, _ = io.WriteString(writer, codexSSE(
					`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_seed","summary":[]}}`,
					`{"type":"response.reasoning_summary_text.delta","item_id":"rs_seed","output_index":0,"delta":"thought"}`,
					`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_seed","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":"opaque-seed"}}`,
					`{"type":"response.completed","response":{"status":"completed"}}`,
				))
				return
			}
			_, _ = io.WriteString(writer, codexTextSuccess("resumed"))
		}), nil)

		seed, _, _, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{
			Messages: []llm.Message{llm.UserText("seed")},
		})
		if err != nil || seed.ProviderState == nil {
			t.Fatalf("seed Complete = state %#v error %v", seed.ProviderState, err)
		}
		fallbackState, err := json.Marshal(codexReplayState{
			Version: codexReplayStateVersion,
			Reasoning: []codexReplayReasoning{{
				Before:           1,
				Summary:          true,
				SummarySpans:     []codexReplaySpan{{Start: 0, End: 99}},
				EncryptedContent: json.RawMessage(`"opaque-fallback"`),
			}},
		})
		if err != nil {
			t.Fatalf("marshal fallback state: %v", err)
		}
		seed.Content = []llm.ContentBlock{
			{Type: llm.BlockText, Text: "before"},
			{Type: llm.BlockThinking, Thinking: "thought"},
			{Type: llm.BlockText, Text: "after"},
		}
		seed.ProviderState.Data = fallbackState
		transcript, err := json.Marshal(seed)
		if err != nil {
			t.Fatalf("marshal fallback transcript: %v", err)
		}
		var resumed llm.Message
		if err := json.Unmarshal(transcript, &resumed); err != nil {
			t.Fatalf("unmarshal fallback transcript: %v", err)
		}
		if _, _, _, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{
			llm.UserText("seed"),
			resumed,
			llm.UserText("continue"),
		}}); err != nil {
			t.Fatalf("fallback Complete: %v", err)
		}
		requestMu.Lock()
		captured := append([]codexCapturedRequest(nil), requests...)
		requestMu.Unlock()
		if len(captured) != 2 {
			t.Fatalf("captured %d requests, want 2", len(captured))
		}
		input, reasoningIndex := codexAssertEmptyReasoningSummary(t, captured[1].Body, "opaque-fallback")
		if reasoningIndex != 2 || len(input) != 5 {
			t.Fatalf("fallback replay order = %#v", input)
		}
		before := input[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
		after := input[3].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
		if before != "before" || after != "after" {
			t.Fatalf("fallback replay moved neutral content: %#v", input)
		}
	})
}

func TestCodexTerminalStreamFailuresPreserveUsage(t *testing.T) {
	tests := []struct {
		name       string
		stream     string
		wantUsage  llm.Usage
		wantCode   string
		wantStatus int
	}{
		{
			name:      "failed nested error",
			stream:    codexSSE(`{"type":"response.failed","response":{"status":"failed","usage":{"input_tokens":17,"output_tokens":2,"input_tokens_details":{"cached_tokens":5}},"error":{"code":"model_error","message":"internal detail"}}}`),
			wantUsage: llm.Usage{InputTokens: 17, OutputTokens: 2, CacheReadTokens: 5},
			wantCode:  "model_error",
		},
		{
			name:      "incomplete max output",
			stream:    codexSSE(`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":19,"output_tokens":8,"input_tokens_details":{"cached_tokens":4}}}}`),
			wantUsage: llm.Usage{InputTokens: 19, OutputTokens: 8, CacheReadTokens: 4},
			wantCode:  "max_output_tokens",
		},
		{
			name:      "incomplete content filter",
			stream:    codexSSE(`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"},"usage":{"input_tokens":5,"output_tokens":1}}}`),
			wantUsage: llm.Usage{InputTokens: 5, OutputTokens: 1},
			wantCode:  "content_filter",
		},
		{
			name:      "completed with nested error",
			stream:    codexSSE(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":7,"output_tokens":1,"input_tokens_details":{"cached_tokens":2}},"error":{"code":"safety_error","message":"private detail"}}}`),
			wantUsage: llm.Usage{InputTokens: 7, OutputTokens: 1, CacheReadTokens: 2},
			wantCode:  "safety_error",
		},
		{
			name:       "error SSE event",
			stream:     codexSSE(`{"type":"error","code":"server_error","message":"private detail"}`),
			wantCode:   "server_error",
			wantStatus: http.StatusOK,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				writer.Header().Set("x-request-id", "request-fixture")
				_, _ = io.WriteString(writer, test.stream)
			}), nil)
			message, stop, usage, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("x")}})
			if err == nil || !IsTerminal(err) {
				t.Fatalf("Complete error = %v, want terminal", err)
			}
			if len(message.Content) != 0 || stop != "" {
				t.Fatalf("failed Complete returned successful partial message: %#v %q", message, stop)
			}
			if usage != test.wantUsage {
				t.Fatalf("usage = %+v, want %+v", usage, test.wantUsage)
			}
			var terminal *Error
			if !errors.As(err, &terminal) || terminal.RequestID != "request-fixture" ||
				terminal.Code != test.wantCode {
				t.Fatalf("typed error = %#v, want code %q", err, test.wantCode)
			}
			if test.wantStatus != 0 && terminal.Status != test.wantStatus {
				t.Fatalf("error status = %d, want %d", terminal.Status, test.wantStatus)
			}
		})
	}
}

func TestCodexMalformedPartialUnknownAndEOF(t *testing.T) {
	tests := []struct {
		name      string
		stream    string
		wantText  string
		wantError bool
	}{
		{
			name: "unknown progress ignored",
			stream: codexSSE(
				`{"type":"response.future_progress","value":1}`,
				`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","id":"msg_terminal","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"input_tokens_details":{"cached_tokens":0}}}}`,
			),
			wantText: "ok",
		},
		{
			name:      "malformed frame",
			stream:    "event: response.output_text.delta\ndata: {not-json}\n\n",
			wantError: true,
		},
		{
			name: "partial EOF",
			stream: codexSSE(
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_partial","content":[]}}`,
				`{"type":"response.output_text.delta","item_id":"msg_partial","output_index":0,"delta":"partial"}`,
			),
			wantError: true,
		},
		{
			name:      "done is not completed",
			stream:    codexSSE(`{"type":"response.done","response":{"status":"completed"}}`),
			wantError: true,
		},
		{
			name: "malformed fragmented tool arguments",
			stream: codexSSE(
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_bad","call_id":"call_bad","name":"read","arguments":""}}`,
				`{"type":"response.function_call_arguments.delta","item_id":"fc_bad","output_index":0,"delta":"{bad"}`,
				`{"type":"response.function_call_arguments.done","item_id":"fc_bad","output_index":0,"arguments":"{bad"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_bad","call_id":"call_bad","name":"read","arguments":"{bad"}}`,
				`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":2,"output_tokens":2,"input_tokens_details":{"cached_tokens":0}}}}`,
			),
			wantError: true,
		},
		{
			name: "function call identity changed",
			stream: codexSSE(
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_identity","call_id":"call_original","name":"read","arguments":""}}`,
				`{"type":"response.function_call_arguments.done","item_id":"fc_identity","output_index":0,"arguments":"{}"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_identity","call_id":"call_changed","name":"read","arguments":"{}"}}`,
			),
			wantError: true,
		},
		{
			name:      "hosted tool rejected",
			stream:    codexSSE(`{"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws_1"}}`),
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(writer, test.stream)
			}), nil)
			message, _, _, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("x")}})
			if test.wantError {
				if err == nil || !IsTerminal(err) {
					t.Fatalf("error = %v, want terminal", err)
				}
				if len(message.Content) != 0 {
					t.Fatalf("failed Complete returned partial content: %#v", message.Content)
				}
				return
			}
			if err != nil || message.Text() != test.wantText {
				t.Fatalf("Complete = text %q error %v", message.Text(), err)
			}
		})
	}
}

func TestCodexStreamExposesPartialTextButNeverSuccessfulStopOnEOF(t *testing.T) {
	fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, codexSSE(
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_partial","content":[]}}`,
			`{"type":"response.output_text.delta","item_id":"msg_partial","output_index":0,"delta":"visible partial"}`,
		))
	}), nil)
	var text strings.Builder
	var terminal error
	sawStop := false
	for event, err := range fixture.provider.Stream(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("x")}}) {
		if err != nil {
			terminal = err
			continue
		}
		if event.Type == llm.SETextDelta {
			text.WriteString(event.Text)
		}
		if event.Type == llm.SEMessageStop {
			sawStop = true
		}
	}
	if text.String() != "visible partial" || terminal == nil || !IsTerminal(terminal) || sawStop {
		t.Fatalf("stream partial=%q error=%v sawStop=%v", text.String(), terminal, sawStop)
	}
}

func TestCodexHTTPErrorPreservesSafeFields(t *testing.T) {
	fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("x-request-id", "req-limit-1")
		writer.Header().Set("x-codex-primary-reset-at", "2000000000")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, `{"error":{"code":"usage_limit_reached","message":"secret upstream body"}}`)
	}), nil)
	_, _, _, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("x")}})
	if err == nil || !IsTerminal(err) {
		t.Fatalf("error = %v, want terminal", err)
	}
	var terminal *Error
	if !errors.As(err, &terminal) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if terminal.Code != "usage_limit_reached" || terminal.Status != http.StatusTooManyRequests ||
		terminal.RequestID != "req-limit-1" || terminal.ResetAt.Unix() != 2000000000 {
		t.Fatalf("terminal error fields = %+v", terminal)
	}
	if strings.Contains(err.Error(), "secret upstream body") {
		t.Fatalf("sanitized error leaked upstream body: %v", err)
	}
}

func TestCodexHTTPAuthorizationClassificationIsTerminal(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantCode string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantCode: CodeAuthRequired},
		{name: "forbidden", status: http.StatusForbidden, wantCode: CodeConsentRequired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := make(chan struct{}, 2)
			fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				calls <- struct{}{}
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("x-request-id", "req-auth")
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, `{}`)
			}), nil)
			_, _, _, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{
				Messages: []llm.Message{llm.UserText("x")},
			})
			var terminal *Error
			if !errors.As(err, &terminal) || !IsTerminal(err) || terminal.Code != test.wantCode ||
				terminal.Status != test.status || terminal.RequestID != "req-auth" {
				t.Fatalf("HTTP %d error = %#v, want terminal code %q", test.status, err, test.wantCode)
			}
			if len(calls) != 1 {
				t.Fatalf("HTTP %d made %d physical requests, want 1", test.status, len(calls))
			}
			if origins := fixture.transport.origins(); len(origins) != 1 || origins[0] != codexResponsesURL {
				t.Fatalf("HTTP %d origins = %v, want fixed Codex origin", test.status, origins)
			}
		})
	}
}

func TestCodexEarlyConsumerStopClosesPhysicalResponse(t *testing.T) {
	responseReleased := make(chan bool, 1)
	fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, codexSSE(
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_early","content":[]}}`,
			`{"type":"response.output_text.delta","item_id":"msg_early","output_index":0,"delta":"first"}`,
		))
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-request.Context().Done():
			responseReleased <- true
		case <-time.After(time.Second):
			responseReleased <- false
		}
	}), nil)

	sawText := false
	for event, err := range fixture.provider.Stream(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{llm.UserText("x")},
	}) {
		if err != nil {
			t.Fatalf("Stream before early stop: %v", err)
		}
		if event.Type == llm.SETextDelta {
			sawText = true
			break
		}
	}
	if !sawText {
		t.Fatal("stream did not expose text before consumer stop")
	}
	select {
	case closed := <-responseReleased:
		if !closed {
			t.Fatal("physical response remained open after consumer stopped")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("physical response closure was not observed")
	}
}

func TestCodexCancellationIsTerminalAndClosesPhysicalCall(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		select {
		case <-request.Context().Done():
			close(canceled)
		case <-time.After(time.Second):
		}
	}), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, _, err := fixture.provider.Complete(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("wait")}})
		result <- err
	}()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("request exited before reaching transport: %v", err)
	case <-time.After(time.Second):
		t.Fatal("request did not reach transport")
	}
	cancel()
	var err error
	select {
	case err = <-result:
	case <-time.After(time.Second):
		t.Fatal("canceled request did not return")
	}
	if err == nil || !IsTerminal(err) {
		t.Fatalf("cancellation error = %v, want terminal", err)
	}
	var terminal *Error
	if !errors.As(err, &terminal) || terminal.Code != codexCodeCanceled {
		t.Fatalf("cancellation error = %#v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("model request context was not canceled")
	}
}

func TestCodexResolvesFreshCredentialForEveryPhysicalCall(t *testing.T) {
	authorizations := make(chan string, 2)
	fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorizations <- request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, codexTextSuccess("ok"))
	}), nil)
	request := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("x")}}
	if _, _, _, err := fixture.provider.Complete(context.Background(), request); err != nil {
		t.Fatalf("first Complete: %v", err)
	}

	setCodexFixtureAccessToken(t, fixture, "synthetic-access-two")

	if _, _, _, err := fixture.provider.Complete(context.Background(), request); err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	if first, second := <-authorizations, <-authorizations; first != "Bearer synthetic-access-one" || second != "Bearer synthetic-access-two" {
		t.Fatalf("authorization snapshots = %q then %q", first, second)
	}
}

func TestCodexBindsJWTAccountAndResidency(t *testing.T) {
	t.Run("account mismatch", func(t *testing.T) {
		called := make(chan struct{}, 1)
		fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			called <- struct{}{}
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(writer, codexTextSuccess("unexpected"))
		}), nil)
		setCodexFixtureAccessToken(t, fixture, codexFixtureJWT("different-account", "eu"))
		_, _, _, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("x")}})
		var terminal *Error
		if !errors.As(err, &terminal) || terminal.Code != CodeIdentityChanged {
			t.Fatalf("account mismatch error = %v", err)
		}
		select {
		case <-called:
			t.Fatal("account-mismatched token reached Codex transport")
		default:
		}
	})

	t.Run("residency header", func(t *testing.T) {
		residencies := make(chan string, 1)
		fixture := newCodexFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			residencies <- request.Header.Get("x-openai-internal-codex-residency")
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(writer, codexTextSuccess("ok"))
		}), nil)
		token := codexFixtureJWT("acct-fixture", "eu")
		setCodexFixtureAccessToken(t, fixture, token)
		if _, _, _, err := fixture.provider.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("x")}}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if got := <-residencies; got != "eu" {
			t.Fatalf("residency header = %q, want eu", got)
		}
	})
}
