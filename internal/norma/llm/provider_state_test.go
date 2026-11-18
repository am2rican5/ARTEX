// Modified by ARTEX for in-tree integration; see ../NOTICE.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderStateJSONRoundTrip(t *testing.T) {
	want := Message{
		Role:    RoleAssistant,
		Content: []ContentBlock{TextBlock("answer")},
		ProviderState: &ProviderState{
			Provider:  "openai-codex-oauth",
			Model:     "codex-model",
			AuthScope: "account:workspace",
			Data:      json.RawMessage(`{"items":[{"type":"reasoning","id":"r1"}]}`),
		},
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"provider_state"`)) {
		t.Fatalf("serialized message omitted provider state: %s", encoded)
	}

	var got Message
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ProviderState == nil {
		t.Fatal("resumed message has nil provider state")
	}
	if got.ProviderState.Provider != want.ProviderState.Provider ||
		got.ProviderState.Model != want.ProviderState.Model ||
		got.ProviderState.AuthScope != want.ProviderState.AuthScope ||
		!bytes.Equal(got.ProviderState.Data, want.ProviderState.Data) {
		t.Fatalf("resumed provider state = %+v, want %+v", got.ProviderState, want.ProviderState)
	}
	if got.Text() != "answer" {
		t.Fatalf("resumed text = %q", got.Text())
	}
}

func TestAccumulatorOwnsProviderStateAndResetClearsIt(t *testing.T) {
	raw := json.RawMessage(`{"opaque":"original"}`)
	state := &ProviderState{
		Provider:  "anthropic-oauth",
		Model:     "claude-model",
		AuthScope: "account:org",
		Data:      raw,
	}
	acc := NewAccumulator()
	acc.Add(StreamEvent{Type: SETextDelta, Text: "answer"})
	acc.Add(StreamEvent{Type: SEProviderState, ProviderState: state})
	acc.Add(StreamEvent{Type: SEMessageDelta, StopReason: "end_turn", Usage: Usage{OutputTokens: 2}})

	state.Provider = "mutated-provider"
	raw[11] = 'X'
	first := acc.Message()
	if first.ProviderState == nil {
		t.Fatal("assembled message has nil provider state")
	}
	if first.ProviderState.Provider != "anthropic-oauth" || string(first.ProviderState.Data) != `{"opaque":"original"}` {
		t.Fatalf("assembled provider state aliased event input: %+v", first.ProviderState)
	}

	first.ProviderState.Model = "mutated-model"
	first.ProviderState.Data[11] = 'Y'
	second := acc.Message()
	if second.ProviderState.Model != "claude-model" || string(second.ProviderState.Data) != `{"opaque":"original"}` {
		t.Fatalf("message result aliased accumulator state: %+v", second.ProviderState)
	}

	acc.Reset()
	reset := acc.Message()
	if reset.ProviderState != nil || len(reset.Content) != 0 || acc.StopReason != "" || acc.Usage != (Usage{}) {
		t.Fatalf("Reset retained completion state: message=%+v stop=%q usage=%+v", reset, acc.StopReason, acc.Usage)
	}
}

func TestMessagesForAPIKeepsStateOnlyWithOriginalTurn(t *testing.T) {
	state := &ProviderState{
		Provider:  "openai-codex-oauth",
		Model:     "codex-model",
		AuthScope: "account:workspace",
		Data:      json.RawMessage(`{"item":"r1"}`),
	}
	intact := Message{
		Role:          RoleAssistant,
		Content:       []ContentBlock{TextBlock("answer")},
		ProviderState: state,
	}
	view := MessagesForAPI([]Message{intact})
	if len(view) != 1 || view[0].ProviderState == nil || string(view[0].ProviderState.Data) != `{"item":"r1"}` {
		t.Fatalf("intact turn lost provider state: %+v", view)
	}
	view[0].ProviderState.Data[9] = 'X'
	if string(state.Data) != `{"item":"r1"}` {
		t.Fatal("API projection returned provider state that aliases conversation history")
	}

	partial := Message{
		Role: RoleAssistant,
		Content: []ContentBlock{
			TextBlock("answer"),
			{Type: BlockToolUse, ID: "orphan", Name: "lookup", Input: json.RawMessage(`{}`)},
		},
		ProviderState: state,
	}
	view = MessagesForAPI([]Message{partial})
	if len(view) != 1 || view[0].Text() != "answer" {
		t.Fatalf("paired view did not retain visible content: %+v", view)
	}
	if view[0].ProviderState != nil {
		t.Fatalf("provider state survived removal of part of its original turn: %+v", view[0].ProviderState)
	}
}

func TestMessagesForAPIPreservesOpaqueOnlyAssistantAfterJSONRoundTrip(t *testing.T) {
	history := []Message{
		{
			Role: RoleAssistant,
			ProviderState: &ProviderState{
				Provider:  "openai-codex-oauth",
				Model:     "codex-model",
				AuthScope: "account:workspace",
				Data:      json.RawMessage(`{"items":[{"type":"reasoning","id":"r1"}]}`),
			},
		},
		UserText("continue"),
		{
			Role: RoleAssistant,
			Content: []ContentBlock{
				{Type: BlockToolUse, ID: "call-1", Name: "lookup", Input: json.RawMessage(`{"q":"x"}`)},
			},
		},
		{Role: RoleUser, Content: []ContentBlock{ToolResultText("call-1", "done", false)}},
		UserText("next"),
	}

	persisted, err := json.Marshal(history)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var resumed []Message
	if err := json.Unmarshal(persisted, &resumed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	view := MessagesForAPI(resumed)
	if len(view) != 5 {
		t.Fatalf("projected history length = %d, want 5: %+v", len(view), view)
	}
	if view[0].Role != RoleAssistant || view[0].Content != nil || view[0].ProviderState == nil {
		t.Fatalf("opaque-only assistant was not retained intact: %+v", view[0])
	}
	if got := string(view[0].ProviderState.Data); got != `{"items":[{"type":"reasoning","id":"r1"}]}` {
		t.Fatalf("opaque replay state = %s", got)
	}
	if view[1].Text() != "continue" {
		t.Fatalf("message after opaque state = %+v, want continue", view[1])
	}
	uses := view[2].ToolUses()
	if len(uses) != 1 || uses[0].ID != "call-1" {
		t.Fatalf("tool continuation moved or changed: %+v", view[2])
	}
	if len(view[3].Content) != 1 || view[3].Content[0].Type != BlockToolResult || view[3].Content[0].ToolUseID != "call-1" {
		t.Fatalf("tool result moved or changed: %+v", view[3])
	}
	if view[4].Text() != "next" {
		t.Fatalf("next user turn moved or changed: %+v", view[4])
	}
}

func rejectInvalidBuiltInRequest(format Format, body []byte) string {
	if bytes.Contains(body, []byte(`"provider_state"`)) ||
		bytes.Contains(body, []byte("must-not-leak")) ||
		bytes.Contains(body, []byte("native-auth-scope")) {
		return "opaque provider state is not valid in this protocol"
	}
	if bytes.Contains(body, []byte(`"content":"…"`)) {
		return "synthetic assistant content is not valid fixture history"
	}

	switch format {
	case FormatAnthropic:
		var req struct {
			Messages []struct {
				Role    string            `json:"role"`
				Content []json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(body, &req) != nil {
			return "invalid Anthropic request JSON"
		}
		for _, m := range req.Messages {
			if m.Role == "assistant" && len(m.Content) == 0 {
				return "Anthropic assistant content must not be empty"
			}
		}
	case FormatOpenAI:
		var req struct {
			Messages []struct {
				Role      string            `json:"role"`
				Content   string            `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"messages"`
		}
		if json.Unmarshal(body, &req) != nil {
			return "invalid Chat Completions request JSON"
		}
		for _, m := range req.Messages {
			if m.Role == "assistant" && m.Content == "" && len(m.ToolCalls) == 0 {
				return "Chat Completions assistant requires content or tool calls"
			}
		}
	case FormatOpenAIResponses:
		var req struct {
			Input []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"input"`
		}
		if json.Unmarshal(body, &req) != nil {
			return "invalid Responses request JSON"
		}
		for _, item := range req.Input {
			if item.Role == "assistant" && item.Content == "" {
				return "Responses assistant input must not be empty"
			}
		}
	}
	return ""
}

func TestBuiltInWireOmitsProviderState(t *testing.T) {
	tests := []struct {
		name       string
		format     Format
		thinking   string
		response   string
		mustRetain []string
	}{
		{
			name:       "anthropic",
			format:     FormatAnthropic,
			thinking:   "enabled",
			response:   `{"type":"message","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`,
			mustRetain: []string{`"signature":"anthropic-signature"`, `"data":"redacted-state"`},
		},
		{
			name:       "openai-chat",
			format:     FormatOpenAI,
			response:   `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`,
			mustRetain: []string{`"reasoning_content":"analysis"`, `"thought_signature":"gemini-signature"`},
		},
		{
			name:     "openai-responses",
			format:   FormatOpenAIResponses,
			response: `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var outgoing []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var err error
				outgoing, err = io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request: %v", err)
				}
				w.Header().Set("content-type", "application/json")
				if rejection := rejectInvalidBuiltInRequest(tt.format, outgoing); rejection != "" {
					http.Error(w, rejection, http.StatusBadRequest)
					return
				}
				_, _ = w.Write([]byte(tt.response))
			}))
			defer srv.Close()

			provider, err := NewProvider(Config{
				Format:               tt.format,
				BaseURL:              srv.URL,
				APIKey:               "fixture-key",
				Model:                "fixture-model",
				ThinkingType:         tt.thinking,
				MaxRetries:           -1,
				EmptyResponseRetries: -1,
			})
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}

			history := []Message{
				{
					Role: RoleAssistant,
					ProviderState: &ProviderState{
						Provider:  "native-provider",
						Model:     "native-model",
						AuthScope: "native-auth-scope",
						Data:      json.RawMessage(`{"opaque":"must-not-leak"}`),
					},
				},
				{
					Role: RoleAssistant,
					Content: []ContentBlock{
						{Type: BlockThinking, Thinking: "analysis", Signature: "anthropic-signature"},
						{Type: BlockRedactedThinking, Data: "redacted-state"},
						{Type: BlockToolUse, ID: "call-1", Name: "lookup", Input: json.RawMessage(`{"q":"x"}`), ThoughtSignature: "gemini-signature"},
					},
					ProviderState: &ProviderState{
						Provider:  "native-provider",
						Model:     "native-model",
						AuthScope: "native-auth-scope",
						Data:      json.RawMessage(`{"opaque":"must-not-leak"}`),
					},
				},
				{Role: RoleUser, Content: []ContentBlock{ToolResultText("call-1", "done", false)}},
			}
			history = MessagesForAPI(history)
			if _, _, _, err := provider.Complete(context.Background(), CompletionRequest{Messages: history}); err != nil {
				t.Fatalf("Complete: %v", err)
			}

			for _, forbidden := range [][]byte{[]byte(`"provider_state"`), []byte("must-not-leak"), []byte("native-auth-scope")} {
				if bytes.Contains(outgoing, forbidden) {
					t.Fatalf("provider state leaked to %s wire: %s", tt.format, outgoing)
				}
			}
			for _, retained := range tt.mustRetain {
				if !bytes.Contains(outgoing, []byte(retained)) {
					t.Fatalf("existing provider signature %q lost from %s wire: %s", retained, tt.format, outgoing)
				}
			}
		})
	}
}
