package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/internal/norma/llm"
)

type claudeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f claudeRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type claudeCapturedRequest struct {
	URL     string
	Headers http.Header
	Body    []byte
}

type claudeManualTimer struct {
	mu      sync.Mutex
	stopped bool
	fired   bool
	fire    func()
}

func (timer *claudeManualTimer) Stop() bool {
	timer.mu.Lock()
	defer timer.mu.Unlock()
	if timer.stopped || timer.fired {
		return false
	}
	timer.stopped = true
	return true
}

func (timer *claudeManualTimer) Fire() bool {
	timer.mu.Lock()
	if timer.stopped || timer.fired {
		timer.mu.Unlock()
		return false
	}
	timer.fired = true
	fire := timer.fire
	timer.mu.Unlock()
	fire()
	return true
}

type claudeGatedBody struct {
	reader  *strings.Reader
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
	closed  atomic.Bool
}

func (body *claudeGatedBody) Read(buffer []byte) (int, error) {
	body.once.Do(func() { close(body.started) })
	<-body.release
	return body.reader.Read(buffer)
}

func (body *claudeGatedBody) Close() error {
	body.closed.Store(true)
	return nil
}

type claudeCancelableBody struct {
	reader  *strings.Reader
	ctx     context.Context
	waiting chan struct{}
	once    sync.Once
	closed  atomic.Bool
}

func (body *claudeCancelableBody) Read(buffer []byte) (int, error) {
	if body.reader.Len() > 0 {
		return body.reader.Read(buffer)
	}
	body.once.Do(func() { close(body.waiting) })
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

func (body *claudeCancelableBody) Close() error {
	body.closed.Store(true)
	return nil
}

func claudeTestProviderModel(t *testing.T, provider, model string, transport http.RoundTripper, thinking, effort string) (llm.Provider, Binding) {
	t.Helper()
	store := newTestStore()
	tokens := TokenSet{
		AccessToken:  "synthetic-claude-token",
		RefreshToken: "synthetic-refresh-token",
		ExpiresAt:    time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		AccountID:    "account-test",
		OrgID:        "organization-test",
	}
	credentialID := store.seed(provider, CredentialReady, tokens)
	binding := Binding{ProfileID: 91, CredentialID: credentialID, Generation: 1}
	store.bind(binding.ProfileID, binding.CredentialID, binding.Generation)
	manager := NewManager(store, Options{
		Drivers: []OAuthDriver{NewClaudeOAuth(nil), NewCodexOAuth(nil)},
		Now: func() time.Time {
			return time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
		},
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	client := &http.Client{Transport: transport}
	modelProvider, err := NewClaudeProvider(manager, ProviderConfig{
		Binding:         binding,
		Model:           model,
		ThinkingType:    thinking,
		ReasoningEffort: effort,
		HTTPClient:      client,
	})
	if err != nil {
		t.Fatalf("NewClaudeProvider: %v", err)
	}
	return modelProvider, binding
}

func claudeTestProvider(t *testing.T, provider string, transport http.RoundTripper, thinking, effort string) (llm.Provider, Binding) {
	t.Helper()
	return claudeTestProviderModel(t, provider, "claude-sonnet-5", transport, thinking, effort)
}

func claudeJSONResponse(request *http.Request, status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: status,
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func claudeSSEResponse(request *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func claudeCollectStream(provider llm.Provider, request llm.CompletionRequest) ([]llm.StreamEvent, error) {
	var events []llm.StreamEvent
	for event, err := range provider.Stream(context.Background(), request) {
		if err != nil {
			return events, err
		}
		events = append(events, event)
	}
	return events, nil
}

func TestClaudeCompleteProtocolAndThinkingReplay(t *testing.T) {
	var mu sync.Mutex
	var captured []claudeCapturedRequest
	responses := []string{
		`{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"sig-1"},{"type":"redacted_thinking","data":"opaque-1"},{"type":"text","text":"calling"},{"type":"tool_use","id":"tool-1","name":"_lookup","input":{"q":"alpha"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":6,"cache_read_input_tokens":2,"cache_creation_input_tokens":3}}`,
		`{"type":"message","id":"msg-2","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":2}}`,
		`{"type":"message","id":"msg-3","role":"assistant","content":[{"type":"text","text":"done again"}],"stop_reason":"end_turn","usage":{"input_tokens":13,"output_tokens":3}}`,
	}
	transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		index := len(captured)
		captured = append(captured, claudeCapturedRequest{
			URL:     request.URL.String(),
			Headers: request.Header.Clone(),
			Body:    append([]byte(nil), body...),
		})
		mu.Unlock()
		return claudeJSONResponse(request, http.StatusOK, responses[index], nil), nil
	})
	provider, _ := claudeTestProvider(t, ProviderClaude, transport, "enabled", "high")
	temperature := 0.4
	request := llm.CompletionRequest{
		System:          []string{"static policy", "dynamic context"},
		DynamicBoundary: 1,
		Messages:        []llm.Message{llm.UserText("please look this up")},
		Tools: []llm.ToolSchema{{
			Name:        "lookup",
			Description: "Find an entry",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"q": map[string]any{"type": "string"}},
				"required":   []string{"q"},
			},
		}},
		MaxTokens:   4096,
		Temperature: &temperature,
	}
	message, stop, usage, err := provider.Complete(context.Background(), request)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if stop != "tool_use" || usage.InputTokens != 15 || usage.OutputTokens != 6 || usage.CacheReadTokens != 2 || usage.CacheWriteTokens != 3 {
		t.Fatalf("completion metadata: stop=%q usage=%+v", stop, usage)
	}
	if message.ProviderState == nil || message.ProviderState.Provider != ProviderClaude || message.ProviderState.Model != "claude-sonnet-5" || message.ProviderState.AuthScope == "" {
		t.Fatalf("provider state = %+v", message.ProviderState)
	}
	stateJSON, err := json.Marshal(message.ProviderState)
	if err != nil {
		t.Fatalf("provider state JSON: %v", err)
	}
	if strings.Contains(string(stateJSON), "synthetic-claude-token") || strings.Contains(string(stateJSON), "synthetic-refresh-token") {
		t.Fatalf("token leaked into provider state: %s", stateJSON)
	}
	if len(message.Content) != 4 || message.Content[0].Signature != "sig-1" || message.Content[1].Data != "opaque-1" {
		t.Fatalf("thinking content = %+v", message.Content)
	}
	if call := message.Content[3]; call.Type != llm.BlockToolUse || call.Name != "lookup" || call.ID != "tool-1" || string(call.Input) != `{"q":"alpha"}` {
		t.Fatalf("tool call = %+v", call)
	}

	request.Messages = append(request.Messages, message, llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Type:      llm.BlockToolResult,
			ToolUseID: "tool-1",
			Content:   []llm.ContentBlock{llm.TextBlock("result-alpha")},
		}},
	})
	if _, _, _, err := provider.Complete(context.Background(), request); err != nil {
		t.Fatalf("replay Complete: %v", err)
	}
	mismatched := message
	stateCopy := *message.ProviderState
	stateCopy.AuthScope = "different-account"
	mismatched.ProviderState = &stateCopy
	request.Messages = []llm.Message{llm.UserText("again"), mismatched, llm.UserText("continue")}
	if _, _, _, err := provider.Complete(context.Background(), request); err != nil {
		t.Fatalf("mismatch Complete: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 3 {
		t.Fatalf("captured requests = %d", len(captured))
	}
	first := captured[0]
	if first.URL != claudeMessagesURL {
		t.Fatalf("URL = %q", first.URL)
	}
	if first.Headers.Get("Authorization") != "Bearer synthetic-claude-token" || first.Headers.Get("X-Api-Key") != "" {
		t.Fatalf("authentication headers = %#v", first.Headers)
	}
	if first.Headers.Get("anthropic-version") != claudeAPIVersion || !strings.Contains(first.Headers.Get("anthropic-beta"), "oauth-2025-04-20") {
		t.Fatalf("protocol headers = %#v", first.Headers)
	}
	if bytesContain := strings.Contains(string(first.Body), "synthetic-claude-token") || strings.Contains(string(first.Body), "synthetic-refresh-token"); bytesContain {
		t.Fatal("token leaked into request body")
	}
	var firstBody map[string]any
	if err := json.Unmarshal(first.Body, &firstBody); err != nil {
		t.Fatalf("first request JSON: %v", err)
	}
	if firstBody["model"] != "claude-sonnet-5" || firstBody["stream"] != false || firstBody["max_tokens"] != float64(4096) {
		t.Fatalf("first request controls = %#v", firstBody)
	}
	if _, exists := firstBody["temperature"]; exists {
		t.Fatalf("unsupported adaptive sampling field = %#v", firstBody["temperature"])
	}
	tools := firstBody["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "_lookup" {
		t.Fatalf("wire tool = %#v", tools[0])
	}
	system := firstBody["system"].([]any)
	if len(system) != 4 || !strings.HasPrefix(system[0].(map[string]any)["text"].(string), claudeBillingPrefix) || strings.Contains(system[0].(map[string]any)["text"].(string), claudeBillingPlaceholder) {
		t.Fatalf("system identity = %#v", system)
	}

	var replayBody map[string]any
	if err := json.Unmarshal(captured[1].Body, &replayBody); err != nil {
		t.Fatalf("replay request JSON: %v", err)
	}
	replayJSON := string(captured[1].Body)
	for _, required := range []string{`"signature":"sig-1"`, `"data":"opaque-1"`, `"name":"_lookup"`, `"tool_use_id":"tool-1"`} {
		if !strings.Contains(replayJSON, required) {
			t.Fatalf("replay body missing %s: %s", required, replayJSON)
		}
	}
	mismatchJSON := string(captured[2].Body)
	if strings.Contains(mismatchJSON, "sig-1") || strings.Contains(mismatchJSON, "opaque-1") {
		t.Fatalf("account-bound thinking leaked across auth scope: %s", mismatchJSON)
	}
}

func TestClaudeStreamFragmentedToolsThinkingAndLifecycle(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-stream\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"cache_read_input_tokens\":2,\"cache_creation_input_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"fallback\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"reason\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig-\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"stream\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-1\",\"name\":\"_lookup\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"one\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-2\",\"name\":\"_fetch\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"n\\\":2}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"cache_read_input_tokens\":2,\"cache_creation_input_tokens\":1}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}, "")
	transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return claudeSSEResponse(request, stream), nil
	})
	provider, _ := claudeTestProvider(t, ProviderClaude, transport, "enabled", "medium")
	events, err := claudeCollectStream(provider, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("go")}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	accumulator := llm.NewAccumulator()
	stops := 0
	terminalDelta := false
	toolInput := make(map[string]string)
	for _, event := range events {
		if event.Type == llm.SEToolUseStart || event.Type == llm.SEToolInputJSON {
			if !terminalDelta {
				t.Fatalf("tool event escaped before terminal message_delta: %+v", event)
			}
		}
		if event.Type == llm.SEToolInputJSON {
			if event.ToolID == "" {
				t.Fatal("Claude tool input fragment had no tool ID")
			}
			toolInput[event.ToolID] += event.Text
		}
		if event.Type == llm.SEMessageDelta && event.StopReason != "" {
			terminalDelta = true
		}
		accumulator.Add(event)
		if event.Type == llm.SEMessageStop {
			stops++
		}
	}
	if toolInput["call-1"] != `{"q":"one"}` || toolInput["call-2"] != `{"n":2}` {
		t.Fatalf("routed tool fragments = %#v", toolInput)
	}
	if stops != 1 {
		t.Fatalf("message stops = %d", stops)
	}
	message := accumulator.Message()
	if message.ProviderState == nil || message.ProviderState.AuthScope == "" {
		t.Fatalf("provider state = %+v", message.ProviderState)
	}
	if len(message.Content) != 3 {
		t.Fatalf("content = %+v", message.Content)
	}
	if thinking := message.Content[0]; thinking.Type != llm.BlockThinking || thinking.Thinking != "reason" || thinking.Signature != "sig-stream" {
		t.Fatalf("thinking = %+v", thinking)
	}
	if call := message.Content[1]; call.Name != "lookup" || call.ID != "call-1" || string(call.Input) != `{"q":"one"}` {
		t.Fatalf("first call = %+v", call)
	}
	if call := message.Content[2]; call.Name != "fetch" || call.ID != "call-2" || string(call.Input) != `{"n":2}` {
		t.Fatalf("second call = %+v", call)
	}
	if accumulator.StopReason != "tool_use" || accumulator.Usage.InputTokens != 13 || accumulator.Usage.OutputTokens != 5 || accumulator.Usage.CacheReadTokens != 2 || accumulator.Usage.CacheWriteTokens != 1 {
		t.Fatalf("accumulator metadata: stop=%q usage=%+v", accumulator.StopReason, accumulator.Usage)
	}
}

func TestClaudeFailuresAreTerminalAndPreserveMetadata(t *testing.T) {
	t.Run("HTTP nested rate limit", func(t *testing.T) {
		transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			headers := make(http.Header)
			headers.Set("Request-Id", "req-limit")
			headers.Set("Retry-After", "30")
			return claudeJSONResponse(request, http.StatusTooManyRequests, `{"type":"error","error":{"type":"rate_limit_error","message":"secret provider detail"},"usage":{"input_tokens":7,"output_tokens":1}}`, headers), nil
		})
		provider, _ := claudeTestProvider(t, ProviderClaude, transport, "", "")
		_, _, usage, err := provider.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("go")}})
		if err == nil || !IsTerminal(err) {
			t.Fatalf("error = %v", err)
		}
		var terminal *Error
		if !errors.As(err, &terminal) || terminal.Code != CodeSubscriptionLimit || terminal.Status != http.StatusTooManyRequests || terminal.RequestID != "req-limit" || terminal.ResetAt.IsZero() {
			t.Fatalf("terminal error = %+v", terminal)
		}
		if strings.Contains(err.Error(), "secret provider detail") {
			t.Fatalf("provider body leaked: %v", err)
		}
		if usage.InputTokens != 7 || usage.OutputTokens != 1 {
			t.Fatalf("usage = %+v", usage)
		}
	})

	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "EOF with pending tool",
			body: strings.Join([]string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input_tokens\":1}}}\n\n",
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-eof\",\"name\":\"_lookup\",\"input\":{}}}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\\\"partial\"}}\n\n",
			}, ""),
		},
		{
			name: "EOF after completed tool",
			body: strings.Join([]string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input_tokens\":1}}}\n\n",
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-eof-complete\",\"name\":\"_lookup\",\"input\":{}}}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n",
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			}, ""),
		},
		{
			name: "malformed frame",
			body: "event: message_start\ndata: not-json\n\n",
		},
		{
			name: "nested stream error",
			body: strings.Join([]string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input_tokens\":1}}}\n\n",
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-error\",\"name\":\"_lookup\",\"input\":{}}}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n",
				"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"do not expose\"},\"request_id\":\"req-auth\",\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}\n\n",
			}, ""),
		},
		{
			name: "malformed fragmented tool arguments",
			body: strings.Join([]string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input_tokens\":1}}}\n\n",
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-bad\",\"name\":\"_lookup\",\"input\":{}}}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\"}}\n\n",
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			}, ""),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				return claudeSSEResponse(request, test.body), nil
			})
			provider, _ := claudeTestProvider(t, ProviderClaude, transport, "", "")
			events, err := claudeCollectStream(provider, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("go")}})
			if err == nil || !IsTerminal(err) {
				t.Fatalf("error = %v", err)
			}
			wouldExecute := 0
			for _, event := range events {
				if event.Type == llm.SEToolUseStart {
					wouldExecute++
				}
				if event.Type == llm.SEToolInputJSON {
					t.Fatalf("failed stream emitted buffered tool input: %+v", event)
				}
			}
			if wouldExecute != 0 {
				t.Fatalf("failed stream crossed tool execution boundary %d times", wouldExecute)
			}
			if test.name == "nested stream error" {
				var observed llm.Usage
				for _, event := range events {
					if event.Type == llm.SEMessageDelta {
						observed = event.Usage
					}
				}
				if observed.InputTokens != 4 || observed.OutputTokens != 2 {
					t.Fatalf("stream error usage = %+v", observed)
				}
			}
			var terminal *Error
			if !errors.As(err, &terminal) || terminal.Status != http.StatusOK {
				t.Fatalf("stream terminal metadata = %+v", terminal)
			}
			if test.name == "nested stream error" && terminal.RequestID != "req-auth" {
				t.Fatalf("stream request ID = %q", terminal.RequestID)
			}
			if strings.Contains(err.Error(), "do not expose") {
				t.Fatalf("stream body leaked: %v", err)
			}
		})
	}
}

func TestClaudeRejectsMalformedAndHostedToolOutputs(t *testing.T) {
	responses := []string{
		`{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"call","name":"_lookup","input":[]}],"stop_reason":"tool_use","usage":{"input_tokens":1}}`,
		`{"type":"message","role":"assistant","content":[{"type":"server_tool_use","id":"hosted","name":"web_search","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":1}}`,
	}
	var calls atomic.Int32
	transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		index := int(calls.Add(1)) - 1
		return claudeJSONResponse(request, http.StatusOK, responses[index], nil), nil
	})
	provider, _ := claudeTestProvider(t, ProviderClaude, transport, "", "")
	for index := range responses {
		_, _, _, err := provider.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("go")}})
		if err == nil || !IsTerminal(err) {
			t.Fatalf("response %d error = %v", index, err)
		}
	}
}

func TestClaudeCancellationAndProviderMismatch(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		started := make(chan struct{})
		transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			close(started)
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		provider, _ := claudeTestProvider(t, ProviderClaude, transport, "", "")
		testCtx, stopTest := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopTest()
		ctx, cancel := context.WithCancel(testCtx)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, _, _, err := provider.Complete(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("go")}})
			done <- err
		}()
		select {
		case <-started:
		case err := <-done:
			t.Fatalf("request failed before HTTP admission: %v", err)
		case <-testCtx.Done():
			t.Fatalf("request was not admitted before deadline: %v", testCtx.Err())
		}
		cancel()
		var err error
		select {
		case err = <-done:
		case <-testCtx.Done():
			t.Fatalf("canceled request did not return before deadline: %v", testCtx.Err())
		}
		var terminal *Error
		if !errors.As(err, &terminal) || terminal.Code != claudeCodeCanceled || !IsTerminal(err) {
			t.Fatalf("cancellation error = %v", err)
		}
	})

	t.Run("wrong provider credential", func(t *testing.T) {
		var calls atomic.Int32
		transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			return claudeJSONResponse(request, http.StatusOK, `{}`, nil), nil
		})
		provider, _ := claudeTestProvider(t, ProviderCodex, transport, "", "")
		_, _, _, err := provider.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("go")}})
		var terminal *Error
		if !errors.As(err, &terminal) || terminal.Code != CodeProviderUnsupported || calls.Load() != 0 {
			t.Fatalf("mismatch: error=%v calls=%d", err, calls.Load())
		}
	})
}

func TestClaudePendingToolCanceledBeforeExecution(t *testing.T) {
	prefix := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-cancel\",\"name\":\"_lookup\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	}, "")
	waiting := make(chan struct{})
	var body *claudeCancelableBody
	transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body = &claudeCancelableBody{
			reader:  strings.NewReader(prefix),
			ctx:     request.Context(),
			waiting: waiting,
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
			Request:    request,
		}, nil
	})
	provider, _ := claudeTestProvider(t, ProviderClaude, transport, "", "")
	testCtx, stopTest := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopTest()
	ctx, cancel := context.WithCancel(testCtx)
	defer cancel()
	type streamResult struct {
		events []llm.StreamEvent
		err    error
	}
	done := make(chan streamResult, 1)
	go func() {
		var result streamResult
		for event, err := range provider.Stream(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("go")}}) {
			if err != nil {
				result.err = err
				break
			}
			result.events = append(result.events, event)
		}
		done <- result
	}()
	select {
	case <-waiting:
	case <-testCtx.Done():
		t.Fatalf("stream did not reach pending tool boundary: %v", testCtx.Err())
	}
	cancel()
	var result streamResult
	select {
	case result = <-done:
	case <-testCtx.Done():
		t.Fatalf("canceled stream did not return: %v", testCtx.Err())
	}
	var terminal *Error
	if !errors.As(result.err, &terminal) || terminal.Code != claudeCodeCanceled {
		t.Fatalf("canceled stream error = %v", result.err)
	}
	for _, event := range result.events {
		if event.Type == llm.SEToolUseStart || event.Type == llm.SEToolInputJSON {
			t.Fatalf("canceled stream crossed tool execution boundary: %+v", event)
		}
	}
	if body == nil || !body.closed.Load() {
		t.Fatal("canceled stream body was not closed")
	}
}

func TestClaudeResponseHeaderDeadlineEndsAtEstablishment(t *testing.T) {
	t.Run("deadline is transport timeout", func(t *testing.T) {
		transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		provider, _ := claudeTestProvider(t, ProviderClaude, transport, "", "")
		timers := make(chan *claudeManualTimer, 1)
		provider.(*claudeProvider).startHeaderTimer = func(_ time.Duration, fire func()) claudeTimer {
			timer := &claudeManualTimer{fire: fire}
			timers <- timer
			return timer
		}
		testCtx, stopTest := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopTest()
		done := make(chan error, 1)
		go func() {
			_, _, _, err := provider.Complete(testCtx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("go")}})
			done <- err
		}()
		var timer *claudeManualTimer
		select {
		case timer = <-timers:
		case <-testCtx.Done():
			t.Fatalf("header timer was not installed: %v", testCtx.Err())
		}
		if !timer.Fire() {
			t.Fatal("header timer stopped before response establishment")
		}
		var err error
		select {
		case err = <-done:
		case <-testCtx.Done():
			t.Fatalf("header timeout did not return: %v", testCtx.Err())
		}
		var terminal *Error
		if !errors.As(err, &terminal) || terminal.Code != claudeCodeTransport || terminal.Code == claudeCodeCanceled {
			t.Fatalf("header timeout error = %v", err)
		}
	})

	t.Run("established stream has no whole-request timer", func(t *testing.T) {
		stream := strings.Join([]string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input_tokens\":1}}}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}, "")
		started := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseBody := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseBody()
		body := &claudeGatedBody{
			reader:  strings.NewReader(stream),
			started: started,
			release: release,
		}
		transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
				Request:    request,
			}, nil
		})
		provider, _ := claudeTestProvider(t, ProviderClaude, transport, "", "")
		timers := make(chan *claudeManualTimer, 1)
		provider.(*claudeProvider).startHeaderTimer = func(_ time.Duration, fire func()) claudeTimer {
			timer := &claudeManualTimer{fire: fire}
			timers <- timer
			return timer
		}
		testCtx, stopTest := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopTest()
		type streamResult struct {
			events []llm.StreamEvent
			err    error
		}
		done := make(chan streamResult, 1)
		go func() {
			events, err := claudeCollectStream(provider, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("go")}})
			done <- streamResult{events: events, err: err}
		}()
		var timer *claudeManualTimer
		select {
		case timer = <-timers:
		case <-testCtx.Done():
			t.Fatalf("header timer was not installed: %v", testCtx.Err())
		}
		select {
		case <-started:
		case <-testCtx.Done():
			t.Fatalf("stream body was not read: %v", testCtx.Err())
		}
		if timer.Fire() {
			t.Fatal("header timer remained armed after response establishment")
		}
		releaseBody()
		var result streamResult
		select {
		case result = <-done:
		case <-testCtx.Done():
			t.Fatalf("established stream did not finish: %v", testCtx.Err())
		}
		if result.err != nil {
			t.Fatalf("established stream: %v", result.err)
		}
		if len(result.events) == 0 || result.events[len(result.events)-1].Type != llm.SEMessageStop {
			t.Fatalf("established stream events = %+v", result.events)
		}
		if !body.closed.Load() {
			t.Fatal("established stream body was not closed")
		}
	})
}

func TestClaudePerCallThinkingOverrideAndCatalogIsolation(t *testing.T) {
	var captured []byte
	transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		captured, _ = io.ReadAll(request.Body)
		return claudeJSONResponse(request, http.StatusOK, `{"type":"message","role":"assistant","content":[{"type":"text","text":"summary"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`, nil), nil
	})
	provider, _ := claudeTestProvider(t, ProviderClaude, transport, "enabled", "high")
	foreignThinking := llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{{
			Type:      llm.BlockThinking,
			Thinking:  "must not replay",
			Signature: "scope-bound-signature",
		}, llm.TextBlock("prior answer")},
		ProviderState: &llm.ProviderState{
			Provider:  ProviderClaude,
			Model:     "claude-sonnet-5",
			AuthScope: "wrong-scope",
			Data:      json.RawMessage(`{"version":2,"prefix_digest":"foreign"}`),
		},
	}
	_, _, _, err := provider.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{llm.UserText("summarize"), foreignThinking, llm.UserText("now")},
		Thinking: "disabled",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("request JSON: %v", err)
	}
	if _, exists := body["thinking"]; exists {
		t.Fatalf("disabled adaptive thinking sent an unsupported switch: %#v", body["thinking"])
	}
	output := body["output_config"].(map[string]any)
	if output["effort"] != "low" || strings.Contains(string(captured), "scope-bound-signature") {
		t.Fatalf("disabled override body = %s", captured)
	}

	models := ClaudeModels()
	if len(models) != 21 {
		t.Fatalf("ClaudeModels length = %d", len(models))
	}
	if models[0].Provider != ProviderClaude || models[0].ContextWindowK != 1000 || models[0].MaxOutputTokens != 128000 || !models[0].SupportsTools || models[0].SupportsThinkingDisable || !models[0].SupportsOutputCap {
		t.Fatalf("catalog model = %+v", models[0])
	}
	if models[3].ID != "claude-haiku-4-5" || models[3].ContextWindowK != 200 ||
		models[3].MaxOutputTokens != 64000 || !models[3].SupportsThinkingDisable {
		t.Fatalf("budget catalog model = %+v", models[3])
	}
	var haiku55 Model
	foundHaiku55 := false
	for _, model := range models {
		if model.ID == "claude-haiku-5-5" {
			haiku55 = model
			foundHaiku55 = true
			break
		}
	}
	if !foundHaiku55 || !haiku55.SupportsThinkingDisable {
		t.Fatalf("Haiku 5.5 catalog capability = %+v found=%t", haiku55, foundHaiku55)
	}
	models[0].ReasoningEfforts[0] = "mutated"
	if ClaudeModels()[0].ReasoningEfforts[0] == "mutated" {
		t.Fatal("ClaudeModels returned shared capability storage")
	}
}

func TestClaudeAdaptiveDefaultAndUnsupportedDisableKeepSignedToolLoop(t *testing.T) {
	var captured [][]byte
	transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		captured = append(captured, body)
		response := `{"type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		if len(captured) == 1 {
			response = `{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"adaptive-signature"},{"type":"tool_use","id":"call-1","name":"_lookup","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
		}
		return claudeJSONResponse(request, http.StatusOK, response, nil), nil
	})
	provider, _ := claudeTestProviderModel(t, ProviderClaude, "claude-sonnet-5", transport, "", "")
	initial := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("go")}}
	message, _, _, err := provider.Complete(context.Background(), initial)
	if err != nil {
		t.Fatalf("adaptive-default Complete: %v", err)
	}
	follow := llm.CompletionRequest{
		Messages: []llm.Message{
			initial.Messages[0],
			message,
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("call-1", "done", false)}},
		},
		Thinking: "disabled",
	}
	if _, _, _, err := provider.Complete(context.Background(), follow); err != nil {
		t.Fatalf("minimum-reasoning Complete: %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("captured requests = %d", len(captured))
	}
	var body map[string]any
	if err := json.Unmarshal(captured[1], &body); err != nil {
		t.Fatalf("minimum-reasoning request JSON: %v", err)
	}
	if _, exists := body["thinking"]; exists {
		t.Fatalf("adaptive-only model received unsupported disabled thinking: %s", captured[1])
	}
	if body["output_config"].(map[string]any)["effort"] != "low" ||
		!strings.Contains(string(captured[1]), "adaptive-signature") {
		t.Fatalf("minimum-reasoning tool loop lost effective adaptive state: %s", captured[1])
	}
}

func TestClaudeThinkingBindingControlsOnChangedPrefix(t *testing.T) {
	for _, model := range []string{
		"claude-fable-5-1",
		"claude-fable-5-2",
		"claude-opus-5-5",
		"claude-sonnet-5-5",
		"claude-haiku-5-5",
	} {
		t.Run(model, func(t *testing.T) {
			var captured []claudeCapturedRequest
			transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				captured = append(captured, claudeCapturedRequest{Headers: request.Header.Clone(), Body: body})
				response := `{"type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
				if len(captured) == 1 {
					response = `{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"bound","signature":"bound-signature"},{"type":"text","text":"first"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
				}
				return claudeJSONResponse(request, http.StatusOK, response, nil), nil
			})
			provider, _ := claudeTestProviderModel(t, ProviderClaude, model, transport, "", "")
			initial := llm.CompletionRequest{
				System:   []string{"stable system"},
				Messages: []llm.Message{llm.UserText("first user")},
			}
			message, _, _, err := provider.Complete(context.Background(), initial)
			if err != nil {
				t.Fatalf("initial Complete: %v", err)
			}
			changed := initial
			changed.System = []string{"changed system"}
			changed.Messages = []llm.Message{initial.Messages[0], message, llm.UserText("continue")}
			if _, _, _, err := provider.Complete(context.Background(), changed); err != nil {
				t.Fatalf("changed-prefix Complete: %v", err)
			}
			if len(captured) != 2 {
				t.Fatalf("captured requests = %d", len(captured))
			}
			for index, request := range captured {
				var body map[string]any
				if err := json.Unmarshal(request.Body, &body); err != nil {
					t.Fatalf("request %d JSON: %v", index, err)
				}
				thinking, ok := body["thinking"].(map[string]any)
				if !ok || thinking["type"] != "adaptive" {
					t.Fatalf("request %d thinking = %#v", index, body["thinking"])
				}
				binding, ok := thinking["block_binding"].(map[string]any)
				if !ok || binding["prefix_mismatch_behavior"] != "drop_block" {
					t.Fatalf("request %d binding = %#v", index, thinking["block_binding"])
				}
				if !strings.Contains(request.Headers.Get("Anthropic-Beta"), "thinking-binding-controls-2026-08-01") {
					t.Fatalf("request %d betas = %q", index, request.Headers.Get("Anthropic-Beta"))
				}
			}
			if !strings.Contains(string(captured[1].Body), "bound-signature") {
				t.Fatalf("drop_block request discarded replay before the server could validate it: %s", captured[1].Body)
			}
		})
	}
}

func TestClaudeMythosPrefixBindingReplaysOnlyUnchangedToolLoop(t *testing.T) {
	for _, model := range []string{"claude-mythos-5-1", "claude-mythos-5-2"} {
		t.Run(model, func(t *testing.T) {
			var captured []claudeCapturedRequest
			transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				captured = append(captured, claudeCapturedRequest{Headers: request.Header.Clone(), Body: body})
				response := `{"type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
				if len(captured) == 1 {
					response = `{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"mythos-signature"},{"type":"tool_use","id":"call-1","name":"_lookup","input":{"q":"one"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
				}
				return claudeJSONResponse(request, http.StatusOK, response, nil), nil
			})
			provider, _ := claudeTestProviderModel(t, ProviderClaude, model, transport, "", "")
			tool := llm.ToolSchema{
				Name:        "lookup",
				Description: "lookup one value",
				InputSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"q": map[string]any{"type": "string"}},
					"required":   []string{"q"},
				},
			}
			initial := llm.CompletionRequest{
				System:   []string{"stable system"},
				Messages: []llm.Message{llm.UserText("first user")},
				Tools:    []llm.ToolSchema{tool},
			}
			message, _, _, err := provider.Complete(context.Background(), initial)
			if err != nil {
				t.Fatalf("initial Complete: %v", err)
			}
			follow := initial
			follow.Messages = []llm.Message{
				initial.Messages[0],
				message,
				{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("call-1", "one", false)}},
			}
			if _, _, _, err := provider.Complete(context.Background(), follow); err != nil {
				t.Fatalf("valid tool-loop Complete: %v", err)
			}
			changedSystem := follow
			changedSystem.System = []string{"changed system"}
			if _, _, _, err := provider.Complete(context.Background(), changedSystem); err != nil {
				t.Fatalf("changed-system Complete: %v", err)
			}
			changedTools := follow
			changedTool := tool
			changedTool.Description = "changed tool"
			changedTools.Tools = []llm.ToolSchema{changedTool}
			if _, _, _, err := provider.Complete(context.Background(), changedTools); err != nil {
				t.Fatalf("changed-tools Complete: %v", err)
			}
			changedHistory := follow
			changedHistory.Messages = append([]llm.Message(nil), follow.Messages...)
			changedHistory.Messages[0] = llm.UserText("changed first user")
			if _, _, _, err := provider.Complete(context.Background(), changedHistory); err != nil {
				t.Fatalf("changed-history Complete: %v", err)
			}
			if len(captured) != 5 {
				t.Fatalf("captured requests = %d", len(captured))
			}
			if strings.Contains(string(captured[0].Body), `"block_binding"`) ||
				strings.Contains(captured[0].Headers.Get("Anthropic-Beta"), "thinking-binding-controls-2026-08-01") {
				t.Fatalf("Mythos advertised unsupported binding controls: body=%s beta=%q", captured[0].Body, captured[0].Headers.Get("Anthropic-Beta"))
			}
			if !strings.Contains(string(captured[1].Body), "mythos-signature") {
				t.Fatalf("unchanged tool loop lost signed thinking: %s", captured[1].Body)
			}
			for index := 2; index < len(captured); index++ {
				if strings.Contains(string(captured[index].Body), "mythos-signature") {
					t.Fatalf("changed prefix %d replayed invalid signed thinking: %s", index, captured[index].Body)
				}
			}
		})
	}
}

func TestClaudeHaiku55DisabledThinkingAndHighEffortConflicts(t *testing.T) {
	var captured [][]byte
	transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		captured = append(captured, body)
		return claudeJSONResponse(request, http.StatusOK, `{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, nil), nil
	})
	for _, test := range []struct {
		model  string
		effort string
	}{
		{model: "claude-haiku-5-5", effort: "high"},
		{model: "claude-haiku-5-5", effort: "xhigh"},
		{model: "claude-sonnet-5-5", effort: "max"},
	} {
		provider, _ := claudeTestProviderModel(t, ProviderClaude, test.model, transport, "enabled", test.effort)
		_, _, _, err := provider.Complete(context.Background(), llm.CompletionRequest{
			Messages: []llm.Message{llm.UserText("go")},
			Thinking: "disabled",
		})
		if err != nil {
			t.Fatalf("%s/%s Complete: %v", test.model, test.effort, err)
		}
	}
	if len(captured) != 3 {
		t.Fatalf("captured requests = %d", len(captured))
	}
	var haikuHigh map[string]any
	if err := json.Unmarshal(captured[0], &haikuHigh); err != nil {
		t.Fatalf("Haiku high request JSON: %v", err)
	}
	if thinking := haikuHigh["thinking"].(map[string]any); thinking["type"] != "disabled" {
		t.Fatalf("Haiku high disabled thinking = %s", captured[0])
	}
	if _, exists := haikuHigh["output_config"]; exists {
		t.Fatalf("Haiku high disabled retained effort: %s", captured[0])
	}
	var haikuXHigh map[string]any
	if err := json.Unmarshal(captured[1], &haikuXHigh); err != nil {
		t.Fatalf("Haiku xhigh request JSON: %v", err)
	}
	haikuThinking := haikuXHigh["thinking"].(map[string]any)
	haikuBinding := haikuThinking["block_binding"].(map[string]any)
	if haikuThinking["type"] != "adaptive" || haikuBinding["prefix_mismatch_behavior"] != "drop_block" ||
		haikuXHigh["output_config"].(map[string]any)["effort"] != "low" {
		t.Fatalf("Haiku xhigh disabled fallback = %s", captured[1])
	}
	var sonnetMax map[string]any
	if err := json.Unmarshal(captured[2], &sonnetMax); err != nil {
		t.Fatalf("Sonnet max request JSON: %v", err)
	}
	sonnetThinking := sonnetMax["thinking"].(map[string]any)
	if sonnetThinking["type"] != "adaptive" || sonnetMax["output_config"].(map[string]any)["effort"] != "max" {
		t.Fatalf("Sonnet max between-tools fallback = %s", captured[2])
	}
}

func TestClaudeBillingFingerprintUsesUTF16CodeUnits(t *testing.T) {
	for _, test := range []struct {
		name   string
		prompt string
		suffix string
	}{
		{
			name:   "Korean",
			prompt: "가나다라마바사아자차카타파하거너더러머버서",
			suffix: "ac8",
		},
		{
			name:   "emoji surrogate",
			prompt: "abcd😀efghijklmnopqrstuvwxyz" + strings.Repeat("z", 1<<20),
			suffix: "7f3",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			blocks := claudeSystemBlocks(llm.CompletionRequest{
				Messages: []llm.Message{llm.UserText(test.prompt)},
			})
			expected := "cc_version=2.1.321." + test.suffix
			if len(blocks) == 0 {
				t.Fatal("billing system block was empty")
			}
			if !strings.Contains(blocks[0].Text, expected) {
				t.Fatalf("billing fingerprint = %q, want %q", blocks[0].Text, expected)
			}
		})
	}
}

func TestClaudeBudgetThinkingAndSamplingNormalization(t *testing.T) {
	var captured [][]byte
	transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		captured = append(captured, body)
		return claudeJSONResponse(request, http.StatusOK, `{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, nil), nil
	})
	provider, _ := claudeTestProviderModel(t, ProviderClaude, "claude-opus-4-5", transport, "enabled", "xhigh")
	temperature := 0.7
	request := llm.CompletionRequest{
		Messages:    []llm.Message{llm.UserText("go")},
		MaxTokens:   4096,
		Temperature: &temperature,
	}
	if _, _, _, err := provider.Complete(context.Background(), request); err != nil {
		t.Fatalf("enabled Complete: %v", err)
	}
	request.Thinking = "disabled"
	if _, _, _, err := provider.Complete(context.Background(), request); err != nil {
		t.Fatalf("disabled Complete: %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("captured requests = %d", len(captured))
	}
	var enabled map[string]any
	if err := json.Unmarshal(captured[0], &enabled); err != nil {
		t.Fatalf("enabled request JSON: %v", err)
	}
	thinking := enabled["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(32768) ||
		thinking["display"] != "summarized" || enabled["max_tokens"] != float64(36768) {
		t.Fatalf("enabled budget request = %s", captured[0])
	}
	if _, exists := enabled["temperature"]; exists {
		t.Fatalf("sampling leaked into thinking request: %s", captured[0])
	}
	output := enabled["output_config"].(map[string]any)
	if output["effort"] != "xhigh" {
		t.Fatalf("enabled effort = %#v", output)
	}

	var disabled map[string]any
	if err := json.Unmarshal(captured[1], &disabled); err != nil {
		t.Fatalf("disabled request JSON: %v", err)
	}
	disabledThinking := disabled["thinking"].(map[string]any)
	if disabledThinking["type"] != "disabled" || disabled["temperature"] != 0.7 ||
		disabled["max_tokens"] != float64(4096) {
		t.Fatalf("disabled budget request = %s", captured[1])
	}
}

func TestClaudeDoesNotFollowCredentialBearingRedirects(t *testing.T) {
	var calls atomic.Int32
	var redirectedAuthorization string
	transport := claudeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) > 1 {
			redirectedAuthorization = request.Header.Get("Authorization")
		}
		header := make(http.Header)
		header.Set("Location", "https://untrusted.invalid/capture")
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader("redirect")),
			Request:    request,
		}, nil
	})
	provider, _ := claudeTestProvider(t, ProviderClaude, transport, "", "")
	_, _, _, err := provider.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("go")}})
	if err == nil || !IsTerminal(err) {
		t.Fatalf("redirect error = %v", err)
	}
	if calls.Load() != 1 || redirectedAuthorization != "" {
		t.Fatalf("redirect followed: calls=%d authorization=%q", calls.Load(), redirectedAuthorization)
	}
}
