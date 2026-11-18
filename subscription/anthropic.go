// Portions of this file are adapted from oh-my-pi's Anthropic provider at
// commit 1bdbf6d242d99ca62bdf2feeff4519ff992f647d, used under the MIT License.
package subscription

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math/bits"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Autumn-27/artex/internal/norma/llm"
	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const (
	claudeMessagesURL           = "https://api.anthropic.com/v1/messages?beta=true"
	claudeAPIVersion            = "2023-06-01"
	claudeCodeVersion           = "2.1.321"
	claudeSDKVersion            = "0.128.0"
	claudeRuntimeVersion        = "v26.12.1"
	claudeMaxResponseBytes      = 128 << 20
	claudeMaxErrorBodyBytes     = 1 << 20
	claudeOutputFallbackTokens  = 4000
	claudeResponseHeaderTimeout = 600 * time.Second

	claudeCodeProviderFailure = "subscription_provider_error"
	claudeCodeTransport       = "subscription_transport_error"
	claudeCodeMalformed       = "subscription_malformed_response"
	claudeCodeCanceled        = "subscription_request_canceled"
)

const (
	claudeBillingPrefix      = "x-anthropic-billing-header:"
	claudeBillingPlaceholder = "cch=00000"
	claudeCCHSeed            = uint64(0x4d659218e32a3268)
)

var errClaudeResponseHeaderTimeout = errors.New("Claude response header timeout")

type claudeTimer interface {
	Stop() bool
}

type claudeProviderStateData struct {
	Version      int    `json:"version"`
	PrefixDigest string `json:"prefix_digest"`
}

type claudeThinkingBlockBinding struct {
	PrefixMismatchBehavior string `json:"prefix_mismatch_behavior"`
}

var claudeBuiltinToolNames = map[string]struct{}{
	"code_execution": {},
	"computer":       {},
	"text_editor":    {},
	"web_search":     {},
}

type claudeProvider struct {
	manager          *Manager
	binding          Binding
	model            Model
	thinkingType     string
	effort           string
	httpClient       *http.Client
	startHeaderTimer func(time.Duration, func()) claudeTimer
}

type claudeRequest struct {
	Model             string                   `json:"model"`
	Messages          []claudeMessage          `json:"messages"`
	System            []claudeSystemBlock      `json:"system,omitempty"`
	Tools             []claudeTool             `json:"tools,omitempty"`
	MaxTokens         int                      `json:"max_tokens"`
	Temperature       *float64                 `json:"temperature,omitempty"`
	Thinking          *claudeThinking          `json:"thinking,omitempty"`
	ContextManagement *claudeContextManagement `json:"context_management,omitempty"`
	OutputConfig      *claudeOutputConfig      `json:"output_config,omitempty"`
	Stream            bool                     `json:"stream"`
	StopSequences     []string                 `json:"stop_sequences,omitempty"`
}

type claudeSystemBlock struct {
	Type         string              `json:"type"`
	Text         string              `json:"text"`
	CacheControl *claudeCacheControl `json:"cache_control,omitempty"`
}

type claudeCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type claudeMessage struct {
	Role    llm.Role        `json:"role"`
	Content []claudeContent `json:"content"`
}

type claudeContent struct {
	kind      string
	text      string
	thinking  string
	signature string
	data      string
	id        string
	name      string
	input     json.RawMessage
	toolUseID string
	result    []claudeTextContent
	isError   bool
}

type claudeTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (c claudeContent) MarshalJSON() ([]byte, error) {
	switch c.kind {
	case "text":
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: c.kind, Text: c.text})
	case "thinking":
		return json.Marshal(struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{Type: c.kind, Thinking: c.thinking, Signature: c.signature})
	case "redacted_thinking":
		return json.Marshal(struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}{Type: c.kind, Data: c.data})
	case "tool_use":
		return json.Marshal(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{Type: c.kind, ID: c.id, Name: c.name, Input: c.input})
	case "tool_result":
		if len(c.result) == 0 {
			return json.Marshal(struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Content   string `json:"content"`
				IsError   bool   `json:"is_error,omitempty"`
			}{Type: c.kind, ToolUseID: c.toolUseID, Content: "", IsError: c.isError})
		}
		return json.Marshal(struct {
			Type      string              `json:"type"`
			ToolUseID string              `json:"tool_use_id"`
			Content   []claudeTextContent `json:"content"`
			IsError   bool                `json:"is_error,omitempty"`
		}{Type: c.kind, ToolUseID: c.toolUseID, Content: c.result, IsError: c.isError})
	default:
		return nil, fmt.Errorf("unsupported Claude content block %q", c.kind)
	}
}

type claudeTool struct {
	Name                string         `json:"name"`
	Description         string         `json:"description"`
	InputSchema         map[string]any `json:"input_schema"`
	EagerInputStreaming bool           `json:"eager_input_streaming"`
}

type claudeThinking struct {
	Type         string                      `json:"type"`
	BudgetTokens int                         `json:"budget_tokens,omitempty"`
	Display      string                      `json:"display,omitempty"`
	BlockBinding *claudeThinkingBlockBinding `json:"block_binding,omitempty"`
}

type claudeOutputConfig struct {
	Effort string `json:"effort"`
}

type claudeContextManagement struct {
	Edits []claudeContextEdit `json:"edits"`
}

type claudeContextEdit struct {
	Type string `json:"type"`
	Keep string `json:"keep"`
}

type claudeThinkingPlan struct {
	wire   *claudeThinking
	output *claudeOutputConfig
	replay bool
	active bool
}

type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u claudeUsage) normalized() llm.Usage {
	return llm.Usage{
		InputTokens:      u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

type claudeWireError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type claudeResponseBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	Data      string          `json:"data"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type claudeResponse struct {
	Type       string                `json:"type"`
	ID         string                `json:"id"`
	Role       string                `json:"role"`
	Content    []claudeResponseBlock `json:"content"`
	StopReason string                `json:"stop_reason"`
	Usage      *claudeUsage          `json:"usage"`
	Error      *claudeWireError      `json:"error"`
	RequestID  string                `json:"request_id"`
}

type claudeStreamEnvelope struct {
	Type         string               `json:"type"`
	Index        *int                 `json:"index"`
	ContentBlock *claudeResponseBlock `json:"content_block"`
	Delta        *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage   *claudeUsage `json:"usage"`
	Message *struct {
		ID    string       `json:"id"`
		Role  string       `json:"role"`
		Usage *claudeUsage `json:"usage"`
	} `json:"message"`
	Error     *claudeWireError `json:"error"`
	RequestID string           `json:"request_id"`
}

type claudeOpenBlock struct {
	kind           string
	id             string
	name           string
	startInput     json.RawMessage
	input          strings.Builder
	signature      strings.Builder
	signatureDelta bool
}

type claudeCompletedTool struct {
	id    string
	name  string
	input json.RawMessage
}

type claudeStreamState struct {
	started         bool
	terminalDelta   bool
	stopped         bool
	consumerStopped bool
	stopReason      string
	status          int
	requestID       string
	usage           llm.Usage
	blocks          map[int]*claudeOpenBlock
	tools           []claudeCompletedTool
	providerState   *llm.ProviderState
}

// NewClaudeProvider constructs the direct Anthropic OAuth Messages adapter.
// Tokens are resolved from Manager for every physical request and are never
// retained on the returned provider.
func NewClaudeProvider(manager *Manager, config ProviderConfig) (llm.Provider, error) {
	if manager == nil {
		return nil, newError(CodeInvalidRequest, "subscription manager is required")
	}
	if err := validateBinding(config.Binding); err != nil {
		return nil, err
	}
	model, ok := claudeCatalogModel(config.Model)
	if !ok {
		return nil, newError(CodeInvalidRequest, "Claude model is not in the pinned catalog")
	}
	if _, err := claudeResolveThinking(model, config.ThinkingType, config.ReasoningEffort, ""); err != nil {
		return nil, err
	}
	baseClient := config.HTTPClient
	if baseClient == nil {
		baseClient = http.DefaultClient
	}
	client := *baseClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	provider := &claudeProvider{
		manager:      manager,
		binding:      config.Binding,
		model:        model,
		thinkingType: config.ThinkingType,
		effort:       config.ReasoningEffort,
		httpClient:   &client,
		startHeaderTimer: func(timeout time.Duration, fire func()) claudeTimer {
			return time.AfterFunc(timeout, fire)
		},
	}
	return provider, nil
}

func (p *claudeProvider) Stream(ctx context.Context, request llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		response, providerState, release, err := p.claudeStartRequest(ctx, request, true)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		defer release()
		defer response.Body.Close()

		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			usage, responseErr := claudeResponseError(response, llm.Usage{})
			if usage != (llm.Usage{}) && !yield(llm.StreamEvent{Type: llm.SEMessageDelta, Usage: usage}, nil) {
				return
			}
			yield(llm.StreamEvent{}, responseErr)
			return
		}

		state := &claudeStreamState{
			status:        response.StatusCode,
			requestID:     claudeRequestID(response),
			blocks:        make(map[int]*claudeOpenBlock),
			providerState: providerState,
		}
		err = visitNativeSSE(response.Request.Context(), response.Body, func(event string, data []byte) (bool, error) {
			return p.claudeConsumeSSE(state, event, data, yield)
		})
		if state.consumerStopped {
			return
		}
		if err != nil {
			if IsTerminal(err) {
				yield(llm.StreamEvent{}, claudeAttachResponseMetadata(err, response, ""))
				return
			}
			yield(llm.StreamEvent{}, claudeRequestError(response.Request.Context(), claudeCodeMalformed, "Claude stream could not be read", response.StatusCode, claudeRequestID(response), err))
			return
		}
		if !state.stopped {
			yield(llm.StreamEvent{}, claudeRequestError(response.Request.Context(), claudeCodeMalformed, "Claude stream ended before message_stop", response.StatusCode, claudeRequestID(response), nil))
		}
	}
}

func (p *claudeProvider) Complete(ctx context.Context, request llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	response, providerState, release, err := p.claudeStartRequest(ctx, request, false)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	defer release()
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		usage, responseErr := claudeResponseError(response, llm.Usage{})
		return llm.Message{}, "", usage, responseErr
	}

	decoder := json.NewDecoder(io.LimitReader(response.Body, claudeMaxResponseBytes+1))
	var wire claudeResponse
	if err := decoder.Decode(&wire); err != nil {
		return llm.Message{}, "", llm.Usage{}, claudeRequestError(response.Request.Context(), claudeCodeMalformed, "Claude returned malformed JSON", response.StatusCode, claudeRequestID(response), err)
	}
	if err := claudeRequireJSONEOF(decoder); err != nil {
		return llm.Message{}, "", llm.Usage{}, claudeRequestError(response.Request.Context(), claudeCodeMalformed, "Claude returned multiple JSON values", response.StatusCode, claudeRequestID(response), err)
	}
	var usage llm.Usage
	if wire.Usage != nil {
		usage = wire.Usage.normalized()
	}
	requestID := claudeRequestID(response)
	if requestID == "" {
		requestID = wire.RequestID
	}
	if wire.Type == "error" || wire.Error != nil {
		return llm.Message{}, "", usage, claudeAttachResponseMetadata(claudeProviderError(response.StatusCode, requestID, wire.Error, usage), response, requestID)
	}
	if wire.Type != "message" || wire.Role != "assistant" || wire.StopReason == "" {
		return llm.Message{}, "", usage, claudeRequestError(response.Request.Context(), claudeCodeMalformed, "Claude response was incomplete", response.StatusCode, requestID, nil)
	}
	message, err := p.claudeDecodeResponse(wire.Content, providerState)
	if err != nil {
		return llm.Message{}, "", usage, claudeAttachResponseMetadata(err, response, requestID)
	}
	return message, wire.StopReason, usage, nil
}

func (p *claudeProvider) claudeStartRequest(ctx context.Context, request llm.CompletionRequest, stream bool) (*http.Response, *llm.ProviderState, func(), error) {
	requestCtx, unregister, err := p.manager.RegisterRequest(ctx, p.binding)
	if err != nil {
		return nil, nil, nil, claudeNormalizeManagerError(ctx, err)
	}
	requestCtx, cancelRequest := context.WithCancelCause(requestCtx)
	release := func() {
		cancelRequest(nil)
		unregister()
	}
	fail := func(err error) (*http.Response, *llm.ProviderState, func(), error) {
		release()
		return nil, nil, nil, err
	}

	snapshot, err := p.manager.Credential(requestCtx, p.binding)
	if err != nil {
		return fail(claudeNormalizeManagerError(requestCtx, err))
	}
	if snapshot.Provider != ProviderClaude {
		return fail(newError(CodeProviderUnsupported, "credential is not a Claude subscription"))
	}
	if strings.TrimSpace(snapshot.Tokens.AccessToken) == "" {
		return fail(newError(CodeAuthRequired, "Claude authorization is required"))
	}

	body, betas, stateData, err := p.claudeBuildBody(request, snapshot, stream)
	if err != nil {
		return fail(err)
	}
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, claudeMessagesURL, bytes.NewReader(body))
	if err != nil {
		return fail(claudeRequestError(requestCtx, claudeCodeTransport, "Claude request could not be created", 0, "", err))
	}
	claudeSetHeaders(httpRequest, snapshot.Tokens.AccessToken, betas)
	headerDeadlineDone := make(chan struct{})
	headerTimer := p.startHeaderTimer(claudeResponseHeaderTimeout, func() {
		cancelRequest(errClaudeResponseHeaderTimeout)
		close(headerDeadlineDone)
	})
	response, err := p.httpClient.Do(httpRequest)
	if !headerTimer.Stop() {
		<-headerDeadlineDone
	}
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return fail(claudeRequestError(requestCtx, claudeCodeTransport, "Claude request failed", 0, "", err))
	}
	if errors.Is(context.Cause(requestCtx), errClaudeResponseHeaderTimeout) {
		response.Body.Close()
		return fail(claudeRequestError(requestCtx, claudeCodeTransport, "Claude response headers timed out", response.StatusCode, claudeRequestID(response), errClaudeResponseHeaderTimeout))
	}
	// RoundTrippers may continue reading the dispatched request until the
	// response body closes. Keep that request untouched, but expose only a
	// sanitized clone through response.Request.
	sanitizedRequest := httpRequest.Clone(requestCtx)
	sanitizedRequest.Header.Del("Authorization")
	response.Request = sanitizedRequest
	decodedBody, err := claudeDecodedBody(response)
	if err != nil {
		response.Body.Close()
		return fail(claudeRequestError(requestCtx, claudeCodeMalformed, "Claude response encoding was invalid", response.StatusCode, claudeRequestID(response), err))
	}
	response.Body = decodedBody
	state := &llm.ProviderState{
		Provider:  ProviderClaude,
		Model:     p.model.ID,
		AuthScope: snapshot.AuthScope,
		Data:      stateData,
	}
	return response, state, release, nil
}

func (p *claudeProvider) claudeBuildBody(request llm.CompletionRequest, snapshot Snapshot, stream bool) ([]byte, []string, json.RawMessage, error) {
	plan, err := claudeResolveThinking(p.model, p.thinkingType, p.effort, request.Thinking)
	if err != nil {
		return nil, nil, nil, err
	}
	system := claudeSystemBlocks(request)
	tools, err := claudeEncodeTools(request.Tools)
	if err != nil {
		return nil, nil, nil, err
	}
	messages, err := p.claudeEncodeMessages(request.Messages, snapshot.AuthScope, plan.replay, system, tools)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(messages) == 0 {
		return nil, nil, nil, newError(CodeInvalidRequest, "Claude request has no messages")
	}
	stateData, err := claudeEncodeProviderState(system, tools, messages)
	if err != nil {
		return nil, nil, nil, err
	}
	maxTokens := request.MaxTokens
	if maxTokens <= 0 || maxTokens > p.model.MaxOutputTokens {
		maxTokens = p.model.MaxOutputTokens
	}
	if plan.wire != nil && plan.wire.Type == "enabled" {
		minimum := plan.wire.BudgetTokens + claudeOutputFallbackTokens
		if maxTokens < minimum {
			maxTokens = minimum
			if maxTokens > p.model.MaxOutputTokens {
				maxTokens = p.model.MaxOutputTokens
			}
		}
		if available := maxTokens - claudeOutputFallbackTokens; plan.wire.BudgetTokens > available {
			plan.wire.BudgetTokens = available
		}
		if plan.wire.BudgetTokens <= 0 {
			return nil, nil, nil, newError(CodeInvalidRequest, "Claude output cap is too small for thinking")
		}
	}
	wire := claudeRequest{
		Model:         p.model.ID,
		Messages:      messages,
		System:        system,
		Tools:         tools,
		MaxTokens:     maxTokens,
		Temperature:   claudeSamplingTemperature(p.model, plan, request.Temperature),
		Thinking:      plan.wire,
		OutputConfig:  plan.output,
		Stream:        stream,
		StopSequences: claudeStopSequences(request.Stop),
	}
	if plan.active {
		wire.ContextManagement = &claudeContextManagement{Edits: []claudeContextEdit{{
			Type: "clear_thinking_20251015",
			Keep: "all",
		}}}
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, nil, nil, newError(CodeInvalidRequest, "Claude request could not be encoded")
	}
	claudePatchBillingAttestation(body)
	return body, claudeBetas(len(tools) > 0, plan.active, plan.output != nil, plan.wire != nil && plan.wire.BlockBinding != nil), stateData, nil
}

func (p *claudeProvider) claudeEncodeMessages(messages []llm.Message, authScope string, replayThinking bool, system []claudeSystemBlock, tools []claudeTool) ([]claudeMessage, error) {
	encoded := make([]claudeMessage, 0, len(messages))
	for _, message := range messages {
		if message.Role != llm.RoleUser && message.Role != llm.RoleAssistant {
			return nil, newError(CodeInvalidRequest, "Claude message role is invalid")
		}
		allowThinking := replayThinking && message.Role == llm.RoleAssistant &&
			claudeStateMatches(message.ProviderState, p.model.ID, authScope, "")
		if allowThinking && claudeThinkingPrefixRequiresClientValidation(p.model.ID) {
			prefixDigest, err := claudePrefixDigest(system, tools, encoded)
			if err != nil {
				return nil, err
			}
			allowThinking = claudeStateMatches(message.ProviderState, p.model.ID, authScope, prefixDigest)
		}
		content := make([]claudeContent, 0, len(message.Content))
		for _, block := range message.Content {
			switch block.Type {
			case llm.BlockText:
				content = append(content, claudeContent{kind: "text", text: block.Text})
			case llm.BlockThinking:
				if message.Role != llm.RoleAssistant {
					return nil, newError(CodeInvalidRequest, "Claude thinking content requires an assistant message")
				}
				if allowThinking && block.Signature != "" {
					content = append(content, claudeContent{kind: "thinking", thinking: block.Thinking, signature: block.Signature})
				}
			case llm.BlockRedactedThinking:
				if message.Role != llm.RoleAssistant {
					return nil, newError(CodeInvalidRequest, "Claude redacted thinking requires an assistant message")
				}
				if allowThinking && block.Data != "" {
					content = append(content, claudeContent{kind: "redacted_thinking", data: block.Data})
				}
			case llm.BlockToolUse:
				if message.Role != llm.RoleAssistant {
					return nil, newError(CodeInvalidRequest, "Claude tool calls require an assistant message")
				}
				input, err := claudeObjectJSON(block.Input)
				if err != nil {
					return nil, newError(CodeInvalidRequest, "Claude tool arguments must be a JSON object")
				}
				if block.ID == "" || block.Name == "" {
					return nil, newError(CodeInvalidRequest, "Claude tool call is missing its ID or name")
				}
				content = append(content, claudeContent{kind: "tool_use", id: block.ID, name: claudeEncodeToolName(block.Name), input: input})
			case llm.BlockToolResult:
				if message.Role != llm.RoleUser {
					return nil, newError(CodeInvalidRequest, "Claude tool results require a user message")
				}
				if block.ToolUseID == "" {
					return nil, newError(CodeInvalidRequest, "Claude tool result is missing its tool call ID")
				}
				result := make([]claudeTextContent, 0, len(block.Content))
				for _, child := range block.Content {
					if child.Type != llm.BlockText {
						return nil, newError(CodeInvalidRequest, "Claude tool results support text content only")
					}
					result = append(result, claudeTextContent{Type: "text", Text: child.Text})
				}
				content = append(content, claudeContent{kind: "tool_result", toolUseID: block.ToolUseID, result: result, isError: block.IsError})
			default:
				return nil, newError(CodeInvalidRequest, "Claude message contains an unsupported content block")
			}
		}
		if len(content) == 0 {
			continue
		}
		if n := len(encoded); n > 0 && encoded[n-1].Role == message.Role {
			encoded[n-1].Content = append(encoded[n-1].Content, content...)
			continue
		}
		encoded = append(encoded, claudeMessage{Role: message.Role, Content: content})
	}
	return encoded, nil
}

func claudeEncodeProviderState(system []claudeSystemBlock, tools []claudeTool, messages []claudeMessage) (json.RawMessage, error) {
	prefixDigest, err := claudePrefixDigest(system, tools, messages)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(claudeProviderStateData{Version: 2, PrefixDigest: prefixDigest})
	if err != nil {
		return nil, newError(CodeInvalidRequest, "Claude replay state could not be encoded")
	}
	return data, nil
}

func claudePrefixDigest(system []claudeSystemBlock, tools []claudeTool, messages []claudeMessage) (string, error) {
	prefix, err := json.Marshal(struct {
		System   []claudeSystemBlock `json:"system"`
		Tools    []claudeTool        `json:"tools"`
		Messages []claudeMessage     `json:"messages"`
	}{
		System:   system,
		Tools:    tools,
		Messages: messages,
	})
	if err != nil {
		return "", newError(CodeInvalidRequest, "Claude replay prefix could not be encoded")
	}
	digest := sha256.Sum256(prefix)
	return hex.EncodeToString(digest[:]), nil
}

func (p *claudeProvider) claudeDecodeResponse(blocks []claudeResponseBlock, providerState *llm.ProviderState) (llm.Message, error) {
	content := make([]llm.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			content = append(content, llm.TextBlock(block.Text))
		case "thinking":
			if block.Signature == "" {
				return llm.Message{}, newError(claudeCodeMalformed, "Claude thinking block is missing its signature")
			}
			content = append(content, llm.ContentBlock{Type: llm.BlockThinking, Thinking: block.Thinking, Signature: block.Signature})
		case "redacted_thinking":
			if block.Data == "" {
				return llm.Message{}, newError(claudeCodeMalformed, "Claude redacted thinking block is empty")
			}
			content = append(content, llm.ContentBlock{Type: llm.BlockRedactedThinking, Data: block.Data})
		case "tool_use":
			input, err := claudeObjectJSON(block.Input)
			if err != nil || block.ID == "" || block.Name == "" {
				return llm.Message{}, newError(claudeCodeMalformed, "Claude returned a malformed tool call")
			}
			content = append(content, llm.ContentBlock{Type: llm.BlockToolUse, ID: block.ID, Name: claudeDecodeToolName(block.Name), Input: input})
		case "server_tool_use", "web_search_tool_result", "tool_search_tool_result":
			return llm.Message{}, newError(claudeCodeMalformed, "Claude returned an unsupported hosted tool block")
		default:
			return llm.Message{}, newError(claudeCodeMalformed, "Claude returned an unknown content block")
		}
	}
	return llm.Message{Role: llm.RoleAssistant, Content: content, ProviderState: providerState}, nil
}

func (p *claudeProvider) claudeConsumeSSE(state *claudeStreamState, event string, data []byte, yield func(llm.StreamEvent, error) bool) (bool, error) {
	if event == "ping" {
		return true, nil
	}
	var envelope claudeStreamEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return false, newError(claudeCodeMalformed, "Claude stream contained malformed JSON")
	}
	if event == "" || event == "message" {
		event = envelope.Type
	}
	if envelope.Type != "" && envelope.Type != event {
		return false, newError(claudeCodeMalformed, "Claude stream event type did not match its envelope")
	}
	emit := func(output llm.StreamEvent) bool {
		if !yield(output, nil) {
			state.consumerStopped = true
			return false
		}
		return true
	}

	switch event {
	case "ping":
		return true, nil
	case "error":
		if envelope.Usage != nil {
			usage := envelope.Usage.normalized()
			state.usage.MergeEcho(usage)
			if !emit(llm.StreamEvent{Type: llm.SEMessageDelta, Usage: usage}) {
				return false, nil
			}
		}
		requestID := envelope.RequestID
		if requestID == "" {
			requestID = state.requestID
		}
		return false, claudeProviderError(state.status, requestID, envelope.Error, state.usage)
	case "message_start":
		if state.started || state.stopped || envelope.Message == nil || envelope.Message.Usage == nil ||
			envelope.Message.Role != "assistant" {
			return false, newError(claudeCodeMalformed, "Claude stream contained an invalid message_start")
		}
		state.started = true
		usage := envelope.Message.Usage.normalized()
		usage.OutputTokens = 0
		state.usage.Add(usage)
		if !emit(llm.StreamEvent{Type: llm.SEMessageStart, Usage: usage}) {
			return false, nil
		}
		if !emit(llm.StreamEvent{Type: llm.SEProviderState, ProviderState: state.providerState}) {
			return false, nil
		}
		return true, nil
	case "content_block_start":
		if !state.started || state.terminalDelta || state.stopped || envelope.Index == nil || envelope.ContentBlock == nil {
			return false, newError(claudeCodeMalformed, "Claude stream contained an invalid content_block_start")
		}
		index := *envelope.Index
		if index < 0 || state.blocks[index] != nil {
			return false, newError(claudeCodeMalformed, "Claude stream reused a content block index")
		}
		block := envelope.ContentBlock
		open := &claudeOpenBlock{kind: block.Type, id: block.ID, name: block.Name, startInput: append(json.RawMessage(nil), block.Input...)}
		state.blocks[index] = open
		switch block.Type {
		case "text":
			if block.Text != "" && !emit(llm.StreamEvent{Type: llm.SETextDelta, Text: block.Text}) {
				return false, nil
			}
		case "thinking":
			if block.Thinking != "" && !emit(llm.StreamEvent{Type: llm.SEThinkingDelta, Text: block.Thinking}) {
				return false, nil
			}
			if block.Signature != "" {
				open.signature.WriteString(block.Signature)
				if !emit(llm.StreamEvent{Type: llm.SEThinkingSignature, Text: open.signature.String()}) {
					return false, nil
				}
			}
		case "redacted_thinking":
			if block.Data == "" {
				return false, newError(claudeCodeMalformed, "Claude redacted thinking block was empty")
			}
			if !emit(llm.StreamEvent{Type: llm.SERedactedThinking, Text: block.Data}) {
				return false, nil
			}
		case "tool_use":
			if block.ID == "" || block.Name == "" {
				return false, newError(claudeCodeMalformed, "Claude tool block was missing its ID or name")
			}
		case "server_tool_use", "web_search_tool_result", "tool_search_tool_result":
			return false, newError(claudeCodeMalformed, "Claude returned an unsupported hosted tool block")
		default:
			return false, newError(claudeCodeMalformed, "Claude stream opened an unknown content block")
		}
		return true, nil
	case "content_block_delta":
		if !state.started || state.terminalDelta || state.stopped || envelope.Index == nil || envelope.Delta == nil {
			return false, newError(claudeCodeMalformed, "Claude stream contained an invalid content_block_delta")
		}
		open := state.blocks[*envelope.Index]
		if open == nil {
			return false, newError(claudeCodeMalformed, "Claude stream updated an unopened content block")
		}
		switch envelope.Delta.Type {
		case "text_delta":
			if open.kind != "text" {
				return false, newError(claudeCodeMalformed, "Claude text delta targeted the wrong block")
			}
			if !emit(llm.StreamEvent{Type: llm.SETextDelta, Text: envelope.Delta.Text}) {
				return false, nil
			}
		case "thinking_delta":
			if open.kind != "thinking" {
				return false, newError(claudeCodeMalformed, "Claude thinking delta targeted the wrong block")
			}
			if !emit(llm.StreamEvent{Type: llm.SEThinkingDelta, Text: envelope.Delta.Thinking}) {
				return false, nil
			}
		case "signature_delta":
			if open.kind != "thinking" || envelope.Delta.Signature == "" {
				return false, newError(claudeCodeMalformed, "Claude signature delta targeted the wrong block")
			}
			if !open.signatureDelta {
				open.signature.Reset()
				open.signatureDelta = true
			}
			open.signature.WriteString(envelope.Delta.Signature)
			if !emit(llm.StreamEvent{Type: llm.SEThinkingSignature, Text: open.signature.String()}) {
				return false, nil
			}
		case "input_json_delta":
			if open.kind != "tool_use" {
				return false, newError(claudeCodeMalformed, "Claude tool input delta targeted the wrong block")
			}
			open.input.WriteString(envelope.Delta.PartialJSON)
		default:
			return false, newError(claudeCodeMalformed, "Claude stream contained an unknown content delta")
		}
		return true, nil
	case "content_block_stop":
		if !state.started || state.terminalDelta || state.stopped || envelope.Index == nil {
			return false, newError(claudeCodeMalformed, "Claude stream contained an invalid content_block_stop")
		}
		index := *envelope.Index
		open := state.blocks[index]
		if open == nil {
			return false, newError(claudeCodeMalformed, "Claude stream closed an unopened content block")
		}
		delete(state.blocks, index)
		if open.kind == "thinking" && open.signature.Len() == 0 {
			return false, newError(claudeCodeMalformed, "Claude thinking block ended without a signature")
		}
		if open.kind == "tool_use" {
			raw := []byte(open.input.String())
			if len(bytes.TrimSpace(raw)) == 0 {
				raw = open.startInput
			}
			input, err := claudeObjectJSON(raw)
			if err != nil {
				return false, newError(claudeCodeMalformed, "Claude returned malformed tool arguments")
			}
			state.tools = append(state.tools, claudeCompletedTool{
				id:    open.id,
				name:  claudeDecodeToolName(open.name),
				input: input,
			})
		}
		return true, nil
	case "message_delta":
		if !state.started || state.terminalDelta || state.stopped || envelope.Delta == nil || len(state.blocks) != 0 || envelope.Delta.StopReason == "" {
			return false, newError(claudeCodeMalformed, "Claude stream contained an invalid terminal message_delta")
		}
		state.terminalDelta = true
		state.stopReason = envelope.Delta.StopReason
		var usage llm.Usage
		if envelope.Usage != nil {
			usage = envelope.Usage.normalized()
			state.usage.MergeEcho(usage)
		}
		if !emit(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: state.stopReason, Usage: usage}) {
			return false, nil
		}
		return true, nil
	case "message_stop":
		if !state.started || !state.terminalDelta || state.stopped || len(state.blocks) != 0 {
			return false, newError(claudeCodeMalformed, "Claude stream contained an invalid message_stop")
		}
		state.stopped = true
		for _, tool := range state.tools {
			if !emit(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: tool.id, ToolName: tool.name}) {
				return false, nil
			}
			if !emit(llm.StreamEvent{Type: llm.SEToolInputJSON, ToolID: tool.id, Text: string(tool.input)}) {
				return false, nil
			}
		}
		if !emit(llm.StreamEvent{Type: llm.SEMessageStop}) {
			return false, nil
		}
		return false, nil
	default:
		// Forward-compatible metadata and keepalive events are not semantic
		// completion and may be ignored safely.
		return true, nil
	}
}

func claudeResolveThinking(model Model, configured, effort, override string) (claudeThinkingPlan, error) {
	mode := configured
	if override != "" {
		mode = override
	}
	if effort != "" && !claudeSupportsEffort(model, effort) {
		return claudeThinkingPlan{}, newError(CodeInvalidRequest, "Claude reasoning effort is not supported by this model")
	}
	plan := claudeThinkingPlan{}
	switch mode {
	case "":
		if effort != "" {
			return plan, newError(CodeInvalidRequest, "Claude reasoning effort requires thinking to be enabled")
		}
		if !claudeUsesBudgetThinking(model.ID) {
			// Modern adaptive models think by default even when the field is
			// omitted. Keep signed history replayable; prefix-bound models with
			// controls need an explicit adaptive object to carry drop_block.
			plan.replay = true
			if claudeSupportsThinkingBindingControls(model.ID) {
				plan.wire = &claudeThinking{Type: "adaptive"}
			}
		}
	case "enabled", "adaptive":
		if claudeUsesBudgetThinking(model.ID) {
			plan.wire = &claudeThinking{
				Type:         "enabled",
				BudgetTokens: claudeThinkingBudget(effort),
				Display:      "summarized",
			}
			if strings.HasPrefix(model.ID, "claude-opus-4-5") && effort != "" {
				plan.output = &claudeOutputConfig{Effort: claudeWireEffort(effort)}
			}
		} else {
			plan.wire = &claudeThinking{Type: "adaptive"}
			if claudeThinkingSupportsDisplay(model.ID) {
				plan.wire.Display = "summarized"
			}
			if effort != "" {
				plan.output = &claudeOutputConfig{Effort: claudeWireEffort(effort)}
			}
		}
		plan.replay = true
	case "disabled":
		switch {
		case model.ID == "claude-haiku-5-5" && (effort == "xhigh" || effort == "max"):
			// Haiku accepts explicit disabled only through high. The pinned
			// adapter falls back to its adaptive default at minimum effort.
			plan.wire = &claudeThinking{Type: "adaptive"}
			plan.output = &claudeOutputConfig{Effort: "low"}
			plan.replay = true
		case model.SupportsThinkingDisable:
			plan.wire = &claudeThinking{Type: "disabled"}
		case model.ID == "claude-sonnet-5-5" && (effort == "xhigh" || effort == "max"):
			// between_tools is rejected above high; retain the requested
			// adaptive effort instead of sending an invalid combination.
			plan.wire = &claudeThinking{Type: "adaptive"}
			plan.output = &claudeOutputConfig{Effort: claudeWireEffort(effort)}
			plan.replay = true
		case model.ID == "claude-sonnet-5-5":
			plan.wire = &claudeThinking{Type: "between_tools"}
		default:
			// Adaptive-only models cannot turn reasoning off. Omission means
			// adaptive thinking, so use minimum effort and retain valid signed
			// history rather than falsely treating this as true-off.
			plan.output = &claudeOutputConfig{Effort: "low"}
			plan.replay = true
			if claudeSupportsThinkingBindingControls(model.ID) {
				plan.wire = &claudeThinking{Type: "adaptive"}
			}
		}
	case "between_tools":
		if model.ID != "claude-sonnet-5-5" {
			return plan, newError(CodeInvalidRequest, "Claude model does not support between-tools thinking")
		}
		if effort == "xhigh" || effort == "max" {
			plan.wire = &claudeThinking{Type: "adaptive"}
			plan.output = &claudeOutputConfig{Effort: claudeWireEffort(effort)}
			plan.replay = true
		} else {
			plan.wire = &claudeThinking{Type: "between_tools"}
		}
	default:
		return plan, newError(CodeInvalidRequest, "Claude thinking mode is invalid")
	}
	if plan.wire != nil && (plan.wire.Type == "adaptive" || plan.wire.Type == "enabled") {
		plan.active = true
		if claudeSupportsThinkingBindingControls(model.ID) {
			plan.wire.BlockBinding = &claudeThinkingBlockBinding{PrefixMismatchBehavior: "drop_block"}
		}
	}
	return plan, nil
}

func claudeSupportsThinkingBindingControls(modelID string) bool {
	switch modelID {
	case "claude-fable-5-1", "claude-fable-5-2", "claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5":
		return true
	default:
		return false
	}
}

func claudeThinkingPrefixRequiresClientValidation(modelID string) bool {
	// Mythos 5.1/5.2 bind signed thinking to the preceding prefix but do not
	// support block_binding controls on Anthropic's pinned route.
	return modelID == "claude-mythos-5-1" || modelID == "claude-mythos-5-2"
}

func claudeUsesBudgetThinking(modelID string) bool {
	return strings.HasPrefix(modelID, "claude-haiku-4-5") ||
		strings.HasPrefix(modelID, "claude-opus-4-5") ||
		strings.HasPrefix(modelID, "claude-sonnet-4-5")
}

func claudeThinkingSupportsDisplay(modelID string) bool {
	return strings.HasPrefix(modelID, "claude-fable-") ||
		strings.HasPrefix(modelID, "claude-mythos-") ||
		strings.HasPrefix(modelID, "claude-haiku-5-5") ||
		strings.HasPrefix(modelID, "claude-opus-4-7") ||
		strings.HasPrefix(modelID, "claude-opus-4-8") ||
		strings.HasPrefix(modelID, "claude-opus-5") ||
		strings.HasPrefix(modelID, "claude-sonnet-5")
}

func claudeThinkingBudget(effort string) int {
	switch effort {
	case "low":
		return 4096
	case "medium":
		return 8192
	case "high":
		return 16384
	case "xhigh", "max":
		return 32768
	default:
		return 1024
	}
}

func claudeSamplingTemperature(model Model, plan claudeThinkingPlan, temperature *float64) *float64 {
	if temperature == nil || !claudeSupportsSampling(model.ID) {
		return nil
	}
	if plan.wire != nil && plan.wire.Type != "disabled" {
		return nil
	}
	value := *temperature
	return &value
}

func claudeSupportsSampling(modelID string) bool {
	return claudeUsesBudgetThinking(modelID) ||
		modelID == "claude-opus-4-6" ||
		modelID == "claude-sonnet-4-6"
}

func claudeSupportsEffort(model Model, effort string) bool {
	for _, supported := range model.ReasoningEfforts {
		if effort == supported {
			return true
		}
	}
	return false
}

func claudeWireEffort(effort string) string {
	if effort == "minimal" {
		return "low"
	}
	return effort
}

func claudeEncodeTools(tools []llm.ToolSchema) ([]claudeTool, error) {
	encoded := make([]claudeTool, 0, len(tools))
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) == "" {
			return nil, newError(CodeInvalidRequest, "Claude tool name is required")
		}
		wireName := claudeEncodeToolName(tool.Name)
		if _, exists := seen[wireName]; exists {
			return nil, newError(CodeInvalidRequest, "Claude tool names collide after wire normalization")
		}
		seen[wireName] = struct{}{}
		schema := make(map[string]any, len(tool.InputSchema)+3)
		for key, value := range tool.InputSchema {
			schema[key] = value
		}
		schema["type"] = "object"
		if properties, ok := schema["properties"].(map[string]any); !ok || properties == nil {
			schema["properties"] = map[string]any{}
		}
		switch required := schema["required"].(type) {
		case []string:
			schema["required"] = required
		case []any:
			normalized := make([]string, 0, len(required))
			for _, value := range required {
				if name, ok := value.(string); ok {
					normalized = append(normalized, name)
				}
			}
			schema["required"] = normalized
		default:
			schema["required"] = []string{}
		}
		encoded = append(encoded, claudeTool{
			Name:                wireName,
			Description:         tool.Description,
			InputSchema:         schema,
			EagerInputStreaming: tool.Name != "yield",
		})
	}
	return encoded, nil
}

func claudeEncodeToolName(name string) string {
	if _, builtin := claudeBuiltinToolNames[strings.ToLower(name)]; builtin {
		return name
	}
	return "_" + name
}

func claudeDecodeToolName(name string) string {
	if _, builtin := claudeBuiltinToolNames[strings.ToLower(name)]; builtin {
		return name
	}
	if strings.HasPrefix(name, "_") {
		return name[1:]
	}
	return name
}

func claudeObjectJSON(raw []byte) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}"), nil
	}
	var object map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, errors.New("not a JSON object")
	}
	if err := claudeRequireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), raw...), nil
}

func claudeRequireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("additional JSON value")
	}
	return err
}

func claudeSystemBlocks(request llm.CompletionRequest) []claudeSystemBlock {
	firstUser := claudeFirstUserText(request.Messages)
	key := claudeBillingFingerprintUnits(firstUser)
	var hashInput [64]byte
	input := append(hashInput[:0], "59cf53e54c78"...)
	for _, unit := range key {
		if utf16.IsSurrogate(unit) {
			unit = utf8.RuneError
		}
		input = utf8.AppendRune(input, unit)
	}
	input = append(input, claudeCodeVersion...)
	digest := sha256.Sum256(input)
	versionSuffix := hex.EncodeToString(digest[:2])[:3]
	billing := fmt.Sprintf("%s cc_version=%s.%s; cc_entrypoint=cli; %s;", claudeBillingPrefix, claudeCodeVersion, versionSuffix, claudeBillingPlaceholder)
	cache := &claudeCacheControl{Type: "ephemeral", TTL: "1h"}
	blocks := []claudeSystemBlock{
		{Type: "text", Text: billing},
		{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude.", CacheControl: cache},
	}
	if len(request.System) == 0 {
		return blocks
	}
	boundary := request.DynamicBoundary
	if boundary <= 0 {
		if text := claudeJoinSystem(request.System); text != "" {
			blocks = append(blocks, claudeSystemBlock{Type: "text", Text: text})
		}
		return blocks
	}
	if boundary >= len(request.System) {
		if text := claudeJoinSystem(request.System); text != "" {
			blocks = append(blocks, claudeSystemBlock{Type: "text", Text: text, CacheControl: &claudeCacheControl{Type: "ephemeral", TTL: "1h"}})
		}
		return blocks
	}
	if text := claudeJoinSystem(request.System[:boundary]); text != "" {
		blocks = append(blocks, claudeSystemBlock{Type: "text", Text: text, CacheControl: &claudeCacheControl{Type: "ephemeral", TTL: "1h"}})
	}
	if text := claudeJoinSystem(request.System[boundary:]); text != "" {
		blocks = append(blocks, claudeSystemBlock{Type: "text", Text: text})
	}
	return blocks
}

func claudeBillingFingerprintUnits(text string) [3]rune {
	key := [3]rune{'0', '0', '0'}
	targets := [...]int{4, 7, 20}
	target := 0
	position := 0
	record := func(unit rune) {
		if target < len(targets) && position == targets[target] {
			key[target] = unit
			target++
		}
		position++
	}
	for _, value := range text {
		if value > 0xffff {
			high, low := utf16.EncodeRune(value)
			record(high)
			if target == len(targets) {
				break
			}
			record(low)
		} else {
			record(value)
		}
		if target == len(targets) {
			break
		}
	}
	return key
}

func claudeJoinSystem(segments []string) string {
	if len(segments) == 0 {
		return ""
	}
	if len(segments) == 1 {
		if strings.TrimSpace(segments[0]) == "" {
			return ""
		}
		return segments[0]
	}
	clean := make([]string, 0, len(segments))
	for _, segment := range segments {
		if strings.TrimSpace(segment) != "" {
			clean = append(clean, segment)
		}
	}
	return strings.Join(clean, "\n\n")
}

func claudeFirstUserText(messages []llm.Message) string {
	for _, message := range messages {
		if message.Role != llm.RoleUser {
			continue
		}
		for _, block := range message.Content {
			if block.Type == llm.BlockText && strings.TrimSpace(block.Text) != "" {
				return block.Text
			}
		}
	}
	return ""
}

func claudeStopSequences(stop []string) []string {
	if len(stop) <= 4 {
		return stop
	}
	return stop[:4]
}

func claudeStateMatches(state *llm.ProviderState, model, authScope, prefixDigest string) bool {
	if state == nil || state.Provider != ProviderClaude || state.Model != model || state.AuthScope != authScope {
		return false
	}
	var data claudeProviderStateData
	if json.Unmarshal(state.Data, &data) != nil || data.Version != 2 || data.PrefixDigest == "" {
		return false
	}
	return prefixDigest == "" || data.PrefixDigest == prefixDigest
}

func claudeBetas(hasTools, thinking, hasEffort, thinkingBinding bool) []string {
	betas := make([]string, 0, 9)
	if hasTools || thinking {
		betas = append(betas,
			"claude-code-20250219",
			"oauth-2025-04-20",
			"interleaved-thinking-2025-05-14",
			"thinking-token-count-2026-05-13",
			"context-management-2025-06-27",
			"prompt-caching-scope-2026-01-05",
		)
		if thinking || hasEffort {
			betas = append(betas, "effort-2025-11-24")
		}
		if thinkingBinding {
			betas = append(betas, "thinking-binding-controls-2026-08-01")
		}
		betas = append(betas, "fallback-credit-2026-06-01")
		return betas
	}
	betas = append(betas,
		"oauth-2025-04-20",
		"interleaved-thinking-2025-05-14",
		"thinking-token-count-2026-05-13",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
		"structured-outputs-2025-12-15",
	)
	if hasEffort {
		betas = append(betas, "effort-2025-11-24")
	}
	if thinkingBinding {
		betas = append(betas, "thinking-binding-controls-2026-08-01")
	}
	return betas
}

func claudeSetHeaders(request *http.Request, accessToken string, betas []string) {
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	request.Header.Set("Connection", "keep-alive")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "claude-cli/"+claudeCodeVersion+" (external, cli)")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("anthropic-version", claudeAPIVersion)
	request.Header.Set("anthropic-dangerous-direct-browser-access", "true")
	request.Header.Set("x-app", "cli")
	request.Header.Set("X-Stainless-Arch", claudeStainlessArch(runtime.GOARCH))
	request.Header.Set("X-Stainless-Lang", "js")
	request.Header.Set("X-Stainless-OS", claudeStainlessOS(runtime.GOOS))
	request.Header.Set("X-Stainless-Package-Version", claudeSDKVersion)
	request.Header.Set("X-Stainless-Retry-Count", "0")
	request.Header.Set("X-Stainless-Runtime", "node")
	request.Header.Set("X-Stainless-Runtime-Version", claudeRuntimeVersion)
	request.Header.Set("X-Stainless-Timeout", "600")
	if len(betas) > 0 {
		request.Header.Set("anthropic-beta", strings.Join(betas, ","))
	}
}

func claudeStainlessOS(value string) string {
	switch strings.ToLower(value) {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "freebsd":
		return "FreeBSD"
	default:
		return "Other::" + strings.ToLower(value)
	}
}

func claudeStainlessArch(value string) string {
	switch strings.ToLower(value) {
	case "amd64", "x64":
		return "x64"
	case "arm64", "aarch64":
		return "arm64"
	case "386", "x86":
		return "x86"
	default:
		return "other::" + strings.ToLower(value)
	}
}

func claudePatchBillingAttestation(body []byte) {
	marker := []byte(`"system":[{"type":"text","text":"` + claudeBillingPrefix)
	markerIndex := bytes.Index(body, marker)
	if markerIndex < 0 {
		return
	}
	searchStart := markerIndex + len(marker)
	searchEnd := searchStart + 150
	if searchEnd > len(body) {
		searchEnd = len(body)
	}
	relative := bytes.Index(body[searchStart:searchEnd], []byte(claudeBillingPlaceholder))
	if relative < 0 {
		return
	}
	placeholderIndex := searchStart + relative
	hash := claudeXXHash64(body, claudeCCHSeed) & 0xfffff
	encoded := fmt.Sprintf("%05x", hash)
	copy(body[placeholderIndex+4:placeholderIndex+9], encoded)
}

func claudeXXHash64(data []byte, seed uint64) uint64 {
	const (
		prime1 = uint64(11400714785074694791)
		prime2 = uint64(14029467366897019727)
		prime3 = uint64(1609587929392839161)
		prime4 = uint64(9650029242287828579)
		prime5 = uint64(2870177450012600261)
	)
	round := func(acc, input uint64) uint64 {
		acc += input * prime2
		acc = bits.RotateLeft64(acc, 31)
		return acc * prime1
	}
	mergeRound := func(acc, value uint64) uint64 {
		acc ^= round(0, value)
		return acc*prime1 + prime4
	}

	offset := 0
	var hash uint64
	if len(data) >= 32 {
		v1 := seed + prime1 + prime2
		v2 := seed + prime2
		v3 := seed
		v4 := seed - prime1
		limit := len(data) - 32
		for offset <= limit {
			v1 = round(v1, binary.LittleEndian.Uint64(data[offset:]))
			v2 = round(v2, binary.LittleEndian.Uint64(data[offset+8:]))
			v3 = round(v3, binary.LittleEndian.Uint64(data[offset+16:]))
			v4 = round(v4, binary.LittleEndian.Uint64(data[offset+24:]))
			offset += 32
		}
		hash = bits.RotateLeft64(v1, 1) + bits.RotateLeft64(v2, 7) + bits.RotateLeft64(v3, 12) + bits.RotateLeft64(v4, 18)
		hash = mergeRound(hash, v1)
		hash = mergeRound(hash, v2)
		hash = mergeRound(hash, v3)
		hash = mergeRound(hash, v4)
	} else {
		hash = seed + prime5
	}
	hash += uint64(len(data))
	for ; offset+8 <= len(data); offset += 8 {
		value := round(0, binary.LittleEndian.Uint64(data[offset:]))
		hash ^= value
		hash = bits.RotateLeft64(hash, 27)*prime1 + prime4
	}
	if offset+4 <= len(data) {
		hash ^= uint64(binary.LittleEndian.Uint32(data[offset:])) * prime1
		hash = bits.RotateLeft64(hash, 23)*prime2 + prime3
		offset += 4
	}
	for ; offset < len(data); offset++ {
		hash ^= uint64(data[offset]) * prime5
		hash = bits.RotateLeft64(hash, 11) * prime1
	}
	hash ^= hash >> 33
	hash *= prime2
	hash ^= hash >> 29
	hash *= prime3
	hash ^= hash >> 32
	return hash
}

type claudeBodyCloser struct {
	io.Reader
	close func() error
}

func (body *claudeBodyCloser) Close() error { return body.close() }

func claudeDecodedBody(response *http.Response) (io.ReadCloser, error) {
	original := response.Body
	switch strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return original, nil
	case "gzip":
		reader, err := gzip.NewReader(original)
		if err != nil {
			return nil, err
		}
		return &claudeBodyCloser{Reader: reader, close: func() error {
			readerErr := reader.Close()
			bodyErr := original.Close()
			if readerErr != nil {
				return readerErr
			}
			return bodyErr
		}}, nil
	case "deflate":
		reader, err := zlib.NewReader(original)
		if err != nil {
			return nil, err
		}
		return &claudeBodyCloser{Reader: reader, close: func() error {
			readerErr := reader.Close()
			bodyErr := original.Close()
			if readerErr != nil {
				return readerErr
			}
			return bodyErr
		}}, nil
	case "br":
		return &claudeBodyCloser{Reader: brotli.NewReader(original), close: original.Close}, nil
	case "zstd":
		reader, err := zstd.NewReader(original)
		if err != nil {
			return nil, err
		}
		return &claudeBodyCloser{Reader: reader, close: func() error {
			reader.Close()
			return original.Close()
		}}, nil
	default:
		return nil, errors.New("unsupported content encoding")
	}
}

func claudeRequestID(response *http.Response) string {
	if response == nil {
		return ""
	}
	if requestID := response.Header.Get("request-id"); requestID != "" {
		return requestID
	}
	return response.Header.Get("x-request-id")
}

func claudeResponseError(response *http.Response, usage llm.Usage) (llm.Usage, error) {
	requestID := claudeRequestID(response)
	var envelope struct {
		Error     *claudeWireError `json:"error"`
		RequestID string           `json:"request_id"`
		Usage     *claudeUsage     `json:"usage"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, claudeMaxErrorBodyBytes))
	if decoder.Decode(&envelope) == nil {
		if requestID == "" {
			requestID = envelope.RequestID
		}
		if envelope.Usage != nil {
			usage = envelope.Usage.normalized()
		}
	}
	responseErr := claudeProviderError(response.StatusCode, requestID, envelope.Error, usage)
	if terminal, ok := responseErr.(*Error); ok {
		terminal.ResetAt = claudeResetAt(response.Header)
	}
	return usage, responseErr
}

func claudeProviderError(status int, requestID string, wire *claudeWireError, _ llm.Usage) error {
	code := claudeCodeProviderFailure
	message := "Claude rejected the request"
	wireType := ""
	if wire != nil {
		wireType = wire.Type
		if wireType == "" {
			wireType = wire.Code
		}
	}
	switch {
	case status == http.StatusUnauthorized || wireType == "authentication_error":
		code = CodeAuthRequired
		message = "Claude authorization is required"
	case status == http.StatusForbidden || wireType == "permission_error":
		code = CodeConsentRequired
		message = "Claude account permission is required"
	case status == http.StatusTooManyRequests || wireType == "rate_limit_error":
		code = CodeSubscriptionLimit
		message = "Claude subscription limit was reached"
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity || wireType == "invalid_request_error":
		code = CodeInvalidRequest
		message = "Claude rejected the request as invalid"
	}
	return &Error{Code: code, Message: message, Status: status, RequestID: requestID}
}

func claudeResetAt(headers http.Header) time.Time {
	value := strings.TrimSpace(headers.Get("retry-after"))
	if value == "" {
		return time.Time{}
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds >= 0 {
		return time.Now().Add(time.Duration(seconds * float64(time.Second)))
	}
	reset, _ := http.ParseTime(value)
	return reset
}

func claudeNormalizeManagerError(ctx context.Context, err error) error {
	if err == nil || IsTerminal(err) {
		return err
	}
	return claudeRequestError(ctx, claudeCodeTransport, "Claude credential could not be resolved", 0, "", err)
}

func claudeRequestError(ctx context.Context, code, message string, status int, requestID string, cause error) error {
	if errors.Is(cause, errClaudeResponseHeaderTimeout) || errors.Is(context.Cause(ctx), errClaudeResponseHeaderTimeout) {
		return &Error{Code: claudeCodeTransport, Message: "Claude response headers timed out", Status: status, RequestID: requestID}
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) || ctx.Err() != nil {
		return &Error{Code: claudeCodeCanceled, Message: "Claude request was canceled", Status: status, RequestID: requestID}
	}
	return &Error{Code: code, Message: message, Status: status, RequestID: requestID}
}

func claudeAttachResponseMetadata(err error, response *http.Response, requestID string) error {
	var terminal *Error
	if !errors.As(err, &terminal) {
		return err
	}
	copy := *terminal
	if response != nil {
		if copy.Status == 0 {
			copy.Status = response.StatusCode
		}
		if copy.RequestID == "" {
			copy.RequestID = requestID
			if copy.RequestID == "" {
				copy.RequestID = claudeRequestID(response)
			}
		}
		if copy.ResetAt.IsZero() {
			copy.ResetAt = claudeResetAt(response.Header)
		}
	}
	return &copy
}
