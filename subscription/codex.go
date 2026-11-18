package subscription

// This file translates the OpenAI Codex Responses transport from Oh My Pi
// commit 1bdbf6d242d99ca62bdf2feeff4519ff992f647d. OMP is MIT licensed:
// Copyright (c) 2025 Mario Zechner
// Copyright (c) 2025-2026 Can Bölük
// Copyright (c) 2026 Stencil Labs, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"iter"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/internal/norma/llm"
)

const (
	codexResponsesURL  = "https://chatgpt.com/backend-api/codex/responses"
	codexClientVersion = "0.159.0"
	codexUserAgent     = "artex/1"

	codexCodeRequestFailed = "subscription_provider_request_failed"
	codexCodeStreamFailed  = "subscription_provider_stream_failed"
	codexCodeCanceled      = "subscription_provider_request_canceled"
)

type codexProvider struct {
	manager *Manager
	config  ProviderConfig
	client  *http.Client
	model   Model
}

// NewCodexProvider builds the direct legacy Codex subscription transport. It
// always targets ChatGPT's fixed Codex Responses origin; HTTPClient can replace
// transport behavior for recording and tests, but cannot replace the URL.
func NewCodexProvider(manager *Manager, config ProviderConfig) (llm.Provider, error) {
	if manager == nil {
		return nil, newError(CodeInvalidRequest, "subscription manager is required")
	}
	model, ok := codexCatalogModel(config.Model)
	if !ok {
		return nil, newError(CodeInvalidRequest, "unsupported Codex model")
	}
	if config.ThinkingType != "" && config.ThinkingType != "enabled" && config.ThinkingType != "disabled" {
		return nil, newError(CodeInvalidRequest, "invalid Codex thinking mode")
	}
	if config.ReasoningEffort != "" && !codexSupportsEffort(model, config.ReasoningEffort) {
		return nil, newError(CodeInvalidRequest, "unsupported Codex reasoning effort")
	}
	baseClient := config.HTTPClient
	if baseClient == nil {
		baseClient = http.DefaultClient
	}
	client := *baseClient
	client.Jar = nil
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &codexProvider{manager: manager, config: config, client: &client, model: model}, nil
}

func codexCatalogModel(id string) (Model, bool) {
	for _, model := range codexModels {
		if model.ID == id {
			return model, true
		}
	}
	return Model{}, false
}

func codexSupportsEffort(model Model, effort string) bool {
	for _, supported := range model.ReasoningEfforts {
		if effort == supported {
			return true
		}
	}
	return false
}

func (p *codexProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for event, err := range p.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", acc.Usage, err
		}
		acc.Add(event)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

func (p *codexProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		thinking, err := p.codexThinking(req.Thinking)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}

		requestCtx, release, err := p.manager.RegisterRequest(ctx, p.config.Binding)
		if err != nil {
			yield(llm.StreamEvent{}, codexTerminalContext(err))
			return
		}
		defer release()

		snapshot, err := p.manager.Credential(requestCtx, p.config.Binding)
		if err != nil {
			yield(llm.StreamEvent{}, codexTerminalContext(err))
			return
		}
		if snapshot.Provider != ProviderCodex {
			yield(llm.StreamEvent{}, newError(CodeCredentialInvalid, "credential provider does not match Codex"))
			return
		}
		if snapshot.Tokens.AccessToken == "" || snapshot.Tokens.AccountID == "" || snapshot.AuthScope == "" {
			yield(llm.StreamEvent{}, newError(CodeCredentialInvalid, "Codex credential identity is incomplete"))
			return
		}
		claims := codexAccessTokenClaims(snapshot.Tokens.AccessToken)
		if claims.accountID != "" && claims.accountID != snapshot.Tokens.AccountID {
			yield(llm.StreamEvent{}, newError(CodeIdentityChanged, "Codex access token account changed"))
			return
		}

		body, err := p.codexBuildBody(req, snapshot.AuthScope, thinking)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		httpReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, codexResponsesURL, bytes.NewReader(body))
		if err != nil {
			yield(llm.StreamEvent{}, codexTerminal(codexCodeRequestFailed, "could not create Codex request"))
			return
		}
		p.codexSetHeaders(httpReq, snapshot, claims.residency)

		response, err := p.client.Do(httpReq)
		if err != nil {
			yield(llm.StreamEvent{}, codexTransportError(requestCtx, err))
			return
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			yield(llm.StreamEvent{}, codexHTTPError(response))
			return
		}

		processor := newCodexStreamProcessor(p.config.Model, snapshot.AuthScope, thinking.replay, response.Header, yield)
		err = visitNativeSSE(requestCtx, response.Body, processor.visit)
		if processor.stopped {
			return
		}
		if err != nil {
			yield(llm.StreamEvent{}, codexStreamError(requestCtx, err, processor.requestID))
			return
		}
		if !processor.completed {
			yield(llm.StreamEvent{}, codexTerminalWithRequest(codexCodeStreamFailed, "Codex stream ended before response.completed", processor.requestID))
		}
	}
}

type codexThinkingSelection struct {
	effort string
	replay bool
}

func (p *codexProvider) codexThinking(override string) (codexThinkingSelection, error) {
	mode := p.config.ThinkingType
	if override != "" {
		mode = override
	}
	if mode != "" && mode != "enabled" && mode != "disabled" {
		return codexThinkingSelection{}, newError(CodeInvalidRequest, "invalid Codex thinking override")
	}
	if mode == "disabled" {
		// The pinned Codex models expose lowest-effort, not true reasoning-off.
		// Keep the wire honest while suppressing incompatible reasoning replay and
		// summaries for auxiliary completion calls.
		return codexThinkingSelection{effort: p.model.ReasoningEfforts[0], replay: false}, nil
	}
	effort := p.config.ReasoningEffort
	if effort == "" {
		effort = "medium"
	}
	if !codexSupportsEffort(p.model, effort) {
		return codexThinkingSelection{}, newError(CodeInvalidRequest, "unsupported Codex reasoning effort")
	}
	return codexThinkingSelection{effort: effort, replay: true}, nil
}

type codexRequestBody struct {
	Model        string           `json:"model"`
	Instructions string           `json:"instructions,omitempty"`
	Input        []codexInputItem `json:"input"`
	Tools        []codexTool      `json:"tools,omitempty"`
	Reasoning    codexReasoning   `json:"reasoning"`
	Include      []string         `json:"include"`
	Store        bool             `json:"store"`
	Stream       bool             `json:"stream"`
}

type codexInputItem struct {
	Type             string              `json:"type,omitempty"`
	Role             string              `json:"role,omitempty"`
	Content          []codexContentPart  `json:"content,omitempty"`
	CallID           string              `json:"call_id,omitempty"`
	Name             string              `json:"name,omitempty"`
	Arguments        string              `json:"arguments,omitempty"`
	Output           string              `json:"output,omitempty"`
	EncryptedContent json.RawMessage     `json:"encrypted_content,omitempty"`
	Summary          *[]codexContentPart `json:"summary,omitempty"`
}

type codexContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type codexReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary,omitempty"`
}

func (p *codexProvider) codexBuildBody(req llm.CompletionRequest, authScope string, thinking codexThinkingSelection) ([]byte, error) {
	body := codexRequestBody{
		Model:     p.config.Model,
		Input:     make([]codexInputItem, 0, len(req.Messages)),
		Reasoning: codexReasoning{Effort: thinking.effort},
		Include:   []string{"reasoning.encrypted_content"},
		Store:     false,
		Stream:    true,
	}
	if thinking.replay {
		body.Reasoning.Summary = "auto"
	}
	finalInstruction := ""
	if len(req.System) > 0 {
		body.Instructions = req.System[0]
		if strings.TrimSpace(body.Instructions) != "" {
			finalInstruction = body.Instructions
		}
		for _, developer := range req.System[1:] {
			if strings.TrimSpace(developer) != "" {
				body.Input = append(body.Input, codexMessageItem("developer", developer, "input_text"))
				finalInstruction = developer
			}
		}
	}
	for _, message := range llm.MessagesForAPI(req.Messages) {
		items, err := p.codexMessageItems(message, authScope, thinking.replay)
		if err != nil {
			return nil, err
		}
		body.Input = append(body.Input, items...)
	}
	hasVisibleInput := false
	for _, item := range body.Input {
		if item.Role != "developer" {
			hasVisibleInput = true
			break
		}
	}
	if !hasVisibleInput && finalInstruction != "" {
		body.Input = append(body.Input, codexMessageItem("user", finalInstruction, "input_text"))
	}
	for _, tool := range req.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			return nil, codexTerminal(CodeInvalidRequest, "Codex tool name is required")
		}
		parameters := make(map[string]any, len(tool.InputSchema)+1)
		for key, value := range tool.InputSchema {
			parameters[key] = value
		}
		if len(parameters) == 0 {
			parameters["type"] = "object"
		}
		if parameters["type"] == "object" {
			if _, present := parameters["properties"]; !present {
				parameters["properties"] = map[string]any{}
			}
		}
		body.Tools = append(body.Tools, codexTool{
			Type: "function", Name: tool.Name, Description: tool.Description, Parameters: parameters,
		})
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, codexTerminal(CodeInvalidRequest, "Codex request is not JSON encodable")
	}
	return encoded, nil
}

func codexMessageItem(role, text, partType string) codexInputItem {
	return codexInputItem{Type: "message", Role: role, Content: []codexContentPart{{Type: partType, Text: text}}}
}

func (p *codexProvider) codexMessageItems(message llm.Message, authScope string, replay bool) ([]codexInputItem, error) {
	if message.Role == llm.RoleUser {
		var items []codexInputItem
		for _, block := range message.Content {
			switch block.Type {
			case llm.BlockText:
				if block.Text != "" {
					items = append(items, codexMessageItem("user", block.Text, "input_text"))
				}
			case llm.BlockToolResult:
				items = append(items, codexInputItem{
					Type: "function_call_output", CallID: codexSanitizeCallID(block.ToolUseID), Output: codexFlattenText(block.Content),
				})
			}
		}
		return items, nil
	}
	if message.Role != llm.RoleAssistant {
		return nil, nil
	}

	state := codexReplayState{}
	stateValid := false
	if replay && message.ProviderState != nil &&
		message.ProviderState.Provider == ProviderCodex &&
		message.ProviderState.Model == p.config.Model &&
		message.ProviderState.AuthScope == authScope &&
		json.Unmarshal(message.ProviderState.Data, &state) == nil && state.Version == codexReplayStateVersion {
		stateValid = true
	}

	var byPosition map[int][]codexReplayReasoning
	if stateValid {
		byPosition = make(map[int][]codexReplayReasoning)
		for _, reasoning := range state.Reasoning {
			if reasoning.Before >= 0 && reasoning.Before <= len(message.Content) && codexOpaqueJSON(reasoning.EncryptedContent) {
				byPosition[reasoning.Before] = append(byPosition[reasoning.Before], reasoning)
			}
		}
	}
	var consumedThinking map[int]bool
	var items []codexInputItem
	for index := 0; index <= len(message.Content); index++ {
		for _, reasoning := range byPosition[index] {
			summary := []codexContentPart{}
			item := codexInputItem{
				Type:             "reasoning",
				EncryptedContent: append(json.RawMessage(nil), reasoning.EncryptedContent...),
				Summary:          &summary,
			}
			if reasoning.Summary && index < len(message.Content) && message.Content[index].Type == llm.BlockThinking {
				summaryText := message.Content[index].Thinking
				if len(reasoning.SummarySpans) > 0 {
					var selected strings.Builder
					valid := true
					for _, span := range reasoning.SummarySpans {
						if span.Start < 0 || span.End < span.Start || span.End > len(summaryText) {
							valid = false
							break
						}
						selected.WriteString(summaryText[span.Start:span.End])
					}
					if !valid {
						items = append(items, item)
						continue
					}
					summaryText = selected.String()
				}
				if summaryText != "" {
					summary = append(summary, codexContentPart{Type: "summary_text", Text: summaryText})
				}
				if consumedThinking == nil {
					consumedThinking = make(map[int]bool)
				}
				consumedThinking[index] = true
			}
			items = append(items, item)
		}
		if index == len(message.Content) {
			break
		}
		block := message.Content[index]
		switch block.Type {
		case llm.BlockText:
			if block.Text != "" {
				items = append(items, codexMessageItem("assistant", block.Text, "output_text"))
			}
		case llm.BlockThinking:
			// Codex reasoning is replayable only with same-scope encrypted state.
			if consumedThinking[index] {
				continue
			}
		case llm.BlockToolUse:
			arguments := strings.TrimSpace(string(block.Input))
			if arguments == "" {
				arguments = "{}"
			}
			if !codexJSONObject(arguments) {
				return nil, codexTerminal(CodeInvalidRequest, "stored Codex tool arguments are malformed")
			}
			items = append(items, codexInputItem{
				Type: "function_call", CallID: codexSanitizeCallID(block.ID), Name: block.Name, Arguments: arguments,
			})
		}
	}
	return items, nil
}

func codexFlattenText(blocks []llm.ContentBlock) string {
	var text strings.Builder
	for _, block := range blocks {
		if block.Type == llm.BlockText {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}

func codexJSONObject(raw string) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal([]byte(raw), &value) == nil && value != nil
}

func codexOpaqueJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null")) && json.Valid(trimmed)
}

type codexTokenClaims struct {
	accountID string
	residency string
}

func codexAccessTokenClaims(accessToken string) codexTokenClaims {
	firstDot := strings.IndexByte(accessToken, '.')
	if firstDot < 0 {
		return codexTokenClaims{}
	}
	secondDotRelative := strings.IndexByte(accessToken[firstDot+1:], '.')
	if secondDotRelative < 0 {
		return codexTokenClaims{}
	}
	secondDot := firstDot + 1 + secondDotRelative
	if strings.IndexByte(accessToken[secondDot+1:], '.') >= 0 {
		return codexTokenClaims{}
	}
	payloadSegment := accessToken[firstDot+1 : secondDot]
	payload, err := base64.RawURLEncoding.DecodeString(payloadSegment)
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(payloadSegment)
		if err != nil {
			return codexTokenClaims{}
		}
	}
	var claims struct {
		Auth struct {
			AccountID        string `json:"chatgpt_account_id"`
			DataResidency    string `json:"chatgpt_data_residency"`
			ComputeResidency string `json:"chatgpt_compute_residency"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return codexTokenClaims{}
	}
	result := codexTokenClaims{accountID: claims.Auth.AccountID}
	if residency := strings.TrimSpace(claims.Auth.DataResidency); residency != "" {
		if !strings.EqualFold(residency, "no_constraint") {
			result.residency = codexSafeToken(residency)
		}
		return result
	}
	if residency := strings.TrimSpace(claims.Auth.ComputeResidency); residency != "" &&
		!strings.EqualFold(residency, "no_constraint") {
		result.residency = codexSafeToken(residency)
	}
	return result
}

func codexSanitizeCallID(raw string) string {
	base := raw
	if index := strings.IndexAny(base, "|\n"); index > 0 {
		base = base[:index]
	} else if index == 0 {
		base = base[1:]
	}
	var clean strings.Builder
	for _, r := range base {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			clean.WriteRune(r)
		} else {
			clean.WriteByte('_')
		}
	}
	candidate := strings.TrimRight(clean.String(), "_")
	if candidate == base && len(candidate) > 0 && len(candidate) <= 64 {
		return candidate
	}
	hashInput := base
	if hashInput == "" {
		hashInput = raw
	}
	digest := sha256.Sum256([]byte(hashInput))
	suffix := hex.EncodeToString(digest[:6])
	if candidate == "" {
		candidate = "call"
	}
	limit := 63 - len(suffix)
	if len(candidate) > limit {
		candidate = candidate[:limit]
	}
	return candidate + "_" + suffix
}

func (p *codexProvider) codexSetHeaders(request *http.Request, snapshot Snapshot, residency string) {
	request.Header.Set("Authorization", "Bearer "+snapshot.Tokens.AccessToken)
	request.Header.Set("chatgpt-account-id", snapshot.Tokens.AccountID)
	request.Header.Set("originator", "omp")
	request.Header.Set("version", codexClientVersion)
	request.Header.Set("x-codex-routing-hint", "model="+p.config.Model)
	if residency != "" {
		request.Header.Set("x-openai-internal-codex-residency", residency)
	}
	request.Header.Set("User-Agent", codexUserAgent)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
}

const codexReplayStateVersion = 1

type codexReplayState struct {
	Version   int                    `json:"version"`
	Reasoning []codexReplayReasoning `json:"reasoning,omitempty"`
}

type codexReplayReasoning struct {
	Before           int               `json:"before"`
	Summary          bool              `json:"summary,omitempty"`
	SummarySpans     []codexReplaySpan `json:"summary_spans,omitempty"`
	EncryptedContent json.RawMessage   `json:"encrypted_content"`
}

type codexReplaySpan struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type codexStreamProcessor struct {
	model         string
	authScope     string
	showThinking  bool
	requestID     string
	yield         func(llm.StreamEvent, error) bool
	items         []*codexOutputItem
	byID          map[string]*codexOutputItem
	byIndex       map[int]*codexOutputItem
	current       *codexOutputItem
	currentVisual string
	blockCount    int
	visualBytes   int
	completed     bool
	stopped       bool
}

type codexOutputItem struct {
	kind             string
	id               string
	outputIndex      int
	hasOutputIndex   bool
	before           int
	block            int
	callID           string
	name             string
	arguments        strings.Builder
	finalArguments   string
	argumentsDone    bool
	itemDone         bool
	visibleEmitted   bool
	fallbackText     string
	summarySpans     []codexReplaySpan
	encryptedContent json.RawMessage
}

func newCodexStreamProcessor(model, authScope string, showThinking bool, headers http.Header, yield func(llm.StreamEvent, error) bool) *codexStreamProcessor {
	return &codexStreamProcessor{
		model: model, authScope: authScope, showThinking: showThinking, requestID: codexRequestID(headers), yield: yield,
		byID: make(map[string]*codexOutputItem), byIndex: make(map[int]*codexOutputItem),
	}
}

type codexFrame struct {
	Type        string          `json:"type"`
	Item        json.RawMessage `json:"item"`
	ItemID      string          `json:"item_id"`
	OutputIndex *int            `json:"output_index"`
	Delta       string          `json:"delta"`
	Arguments   string          `json:"arguments"`
	Text        string          `json:"text"`
	Response    *codexResponse  `json:"response"`
	Error       json.RawMessage `json:"error"`
	Code        string          `json:"code"`
	Message     string          `json:"message"`
}

type codexResponse struct {
	Status            string            `json:"status"`
	Output            []json.RawMessage `json:"output"`
	Usage             *codexWireUsage   `json:"usage"`
	Error             json.RawMessage   `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

type codexWireUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
}

func (usage *codexWireUsage) normalized() llm.Usage {
	if usage == nil {
		return llm.Usage{}
	}
	return llm.Usage{
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CacheReadTokens: usage.InputTokensDetails.CachedTokens, CacheWriteTokens: usage.InputTokensDetails.CacheWriteTokens,
	}
}

func (p *codexStreamProcessor) visit(event string, data []byte) (bool, error) {
	if bytes.Equal(data, []byte("[DONE]")) {
		return false, codexTerminalWithRequest(codexCodeStreamFailed, "Codex stream ended without response.completed", p.requestID)
	}
	var frame codexFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return false, codexTerminalWithRequest(codexCodeStreamFailed, "Codex stream contained malformed JSON", p.requestID)
	}
	if frame.Type == "" {
		frame.Type = event
	}
	switch frame.Type {
	case "response.output_item.added":
		if err := p.addItem(frame.Item, frame.OutputIndex); err != nil {
			return false, err
		}
	case "response.output_text.delta", "response.refusal.delta":
		item, err := p.itemForFrame(frame, "message")
		if err != nil {
			return false, err
		}
		if item != nil {
			item.visibleEmitted = true
		}
		if frame.Delta != "" && !p.emitVisible(item, "text", llm.StreamEvent{Type: llm.SETextDelta, Text: frame.Delta}) {
			return false, nil
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		item, err := p.itemForFrame(frame, "reasoning")
		if err != nil {
			return false, err
		}
		if !p.showThinking {
			break
		}
		if item != nil {
			item.visibleEmitted = true
		}
		if frame.Delta != "" && !p.emitVisible(item, "thinking", llm.StreamEvent{Type: llm.SEThinkingDelta, Text: frame.Delta}) {
			return false, nil
		}
	case "response.reasoning_summary_text.done":
		item, err := p.itemForFrame(frame, "reasoning")
		if err != nil {
			return false, err
		}
		if !p.showThinking {
			break
		}
		if item != nil && !item.visibleEmitted && frame.Text != "" {
			item.visibleEmitted = true
			if !p.emitVisible(item, "thinking", llm.StreamEvent{Type: llm.SEThinkingDelta, Text: frame.Text}) {
				return false, nil
			}
		}
	case "response.function_call_arguments.delta":
		item, err := p.itemForFrame(frame, "function_call")
		if err != nil {
			return false, err
		}
		if item == nil {
			return false, codexTerminalWithRequest(codexCodeStreamFailed, "Codex tool argument delta has no matching call", p.requestID)
		}
		item.arguments.WriteString(frame.Delta)
	case "response.function_call_arguments.done":
		item, err := p.itemForFrame(frame, "function_call")
		if err != nil {
			return false, err
		}
		if item == nil {
			return false, codexTerminalWithRequest(codexCodeStreamFailed, "Codex tool arguments have no matching call", p.requestID)
		}
		item.finalArguments = frame.Arguments
		item.argumentsDone = true
	case "response.output_item.done":
		item, err := p.finishItem(frame.Item, frame.OutputIndex)
		if err != nil {
			return false, err
		}
		if item != nil && (item.kind == "message" || item.kind == "reasoning") &&
			!(item.kind == "reasoning" && !p.showThinking) && !item.visibleEmitted {
			text := codexOutputItemText(frame.Item, item.kind)
			if text != "" {
				item.visibleEmitted = true
				typeName := "text"
				eventType := llm.SETextDelta
				if item.kind == "reasoning" {
					typeName = "thinking"
					eventType = llm.SEThinkingDelta
				}
				if !p.emitVisible(item, typeName, llm.StreamEvent{Type: eventType, Text: text}) {
					return false, nil
				}
			}
		}
	case "response.completed":
		return p.complete(frame.Response)
	case "response.incomplete":
		return p.failResponse(frame.Response, "Codex response was incomplete")
	case "response.failed", "error":
		return p.failFrame(frame)
	default:
		// Forward-compatible progress/metadata events do not affect the normalized
		// response. Unknown output item kinds are rejected when observed above.
	}
	return !p.stopped, nil
}

func (p *codexStreamProcessor) emit(event llm.StreamEvent) bool {
	if p.stopped {
		return false
	}
	if !p.yield(event, nil) {
		p.stopped = true
		return false
	}
	return true
}

func (p *codexStreamProcessor) emitVisible(item *codexOutputItem, kind string, event llm.StreamEvent) bool {
	if p.currentVisual != kind {
		p.currentVisual = kind
		p.blockCount++
		p.visualBytes = 0
	}
	if item != nil && item.block < 0 {
		item.block = p.blockCount - 1
	}
	start := p.visualBytes
	p.visualBytes += len(event.Text)
	if item != nil && kind == "thinking" && len(event.Text) > 0 {
		if count := len(item.summarySpans); count > 0 && item.summarySpans[count-1].End == start {
			item.summarySpans[count-1].End = p.visualBytes
		} else {
			item.summarySpans = append(item.summarySpans, codexReplaySpan{Start: start, End: p.visualBytes})
		}
	}
	return p.emit(event)
}

func (p *codexStreamProcessor) addItem(raw json.RawMessage, outputIndex *int) error {
	var wire struct {
		Type             string          `json:"type"`
		ID               string          `json:"id"`
		CallID           string          `json:"call_id"`
		Name             string          `json:"name"`
		Arguments        string          `json:"arguments"`
		EncryptedContent json.RawMessage `json:"encrypted_content"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &wire) != nil || wire.Type == "" {
		return codexTerminalWithRequest(codexCodeStreamFailed, "Codex output item is malformed", p.requestID)
	}
	switch wire.Type {
	case "message", "reasoning", "function_call":
	default:
		return codexTerminalWithRequest(codexCodeStreamFailed, "Codex emitted an unsupported hosted or unknown output item", p.requestID)
	}
	if wire.ID != "" {
		if _, exists := p.byID[wire.ID]; exists {
			return codexTerminalWithRequest(codexCodeStreamFailed, "Codex reused an output item identity", p.requestID)
		}
	}
	if outputIndex != nil {
		if *outputIndex < 0 {
			return codexTerminalWithRequest(codexCodeStreamFailed, "Codex emitted a negative output index", p.requestID)
		}
		if _, exists := p.byIndex[*outputIndex]; exists {
			return codexTerminalWithRequest(codexCodeStreamFailed, "Codex reused an output index", p.requestID)
		}
	}
	item := &codexOutputItem{kind: wire.Type, id: wire.ID, before: p.blockCount, block: -1, callID: wire.CallID, name: wire.Name}
	if outputIndex != nil {
		item.outputIndex, item.hasOutputIndex = *outputIndex, true
	}
	if wire.Arguments != "" {
		item.arguments.WriteString(wire.Arguments)
	}
	if codexOpaqueJSON(wire.EncryptedContent) {
		item.encryptedContent = append(json.RawMessage(nil), wire.EncryptedContent...)
	}
	p.items = append(p.items, item)
	p.current = item
	if item.id != "" {
		p.byID[item.id] = item
	}
	if item.hasOutputIndex {
		p.byIndex[item.outputIndex] = item
	}
	return nil
}

func (p *codexStreamProcessor) itemForFrame(frame codexFrame, kind string) (*codexOutputItem, error) {
	if frame.ItemID != "" {
		item := p.byID[frame.ItemID]
		if item == nil || item.kind != kind {
			return nil, codexTerminalWithRequest(codexCodeStreamFailed, "Codex event references an unknown output item", p.requestID)
		}
		return item, nil
	}
	if frame.OutputIndex != nil {
		item := p.byIndex[*frame.OutputIndex]
		if item == nil || item.kind != kind {
			return nil, codexTerminalWithRequest(codexCodeStreamFailed, "Codex event references an unknown output index", p.requestID)
		}
		return item, nil
	}
	if p.current != nil && p.current.kind == kind {
		return p.current, nil
	}
	var candidate *codexOutputItem
	for _, item := range p.items {
		if item.kind != kind || item.itemDone {
			continue
		}
		if candidate != nil {
			return nil, codexTerminalWithRequest(codexCodeStreamFailed, "Codex event is ambiguous between output items", p.requestID)
		}
		candidate = item
	}
	return candidate, nil
}

func (p *codexStreamProcessor) finishItem(raw json.RawMessage, outputIndex *int) (*codexOutputItem, error) {
	var wire struct {
		Type             string          `json:"type"`
		ID               string          `json:"id"`
		CallID           string          `json:"call_id"`
		Name             string          `json:"name"`
		Arguments        string          `json:"arguments"`
		EncryptedContent json.RawMessage `json:"encrypted_content"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &wire) != nil || wire.Type == "" {
		return nil, codexTerminalWithRequest(codexCodeStreamFailed, "Codex completed output item is malformed", p.requestID)
	}
	var item *codexOutputItem
	if wire.ID != "" {
		item = p.byID[wire.ID]
	}
	if item == nil && outputIndex != nil {
		item = p.byIndex[*outputIndex]
	}
	if item == nil && wire.ID == "" && outputIndex == nil {
		var err error
		item, err = p.itemForFrame(codexFrame{}, wire.Type)
		if err != nil {
			return nil, err
		}
	}
	if item != nil && item.kind != wire.Type {
		return nil, codexTerminalWithRequest(codexCodeStreamFailed, "Codex completed item does not match its open output item", p.requestID)
	}
	if item != nil && wire.ID != "" && item.id != "" && item.id != wire.ID {
		return nil, codexTerminalWithRequest(codexCodeStreamFailed, "Codex completed item changed its identity", p.requestID)
	}
	if item != nil && outputIndex != nil && item.hasOutputIndex && item.outputIndex != *outputIndex {
		return nil, codexTerminalWithRequest(codexCodeStreamFailed, "Codex completed item changed its output index", p.requestID)
	}
	if item != nil && wire.CallID != "" && item.callID != "" && item.callID != wire.CallID {
		return nil, codexTerminalWithRequest(codexCodeStreamFailed, "Codex function call changed its identity", p.requestID)
	}
	if item != nil && wire.Name != "" && item.name != "" && item.name != wire.Name {
		return nil, codexTerminalWithRequest(codexCodeStreamFailed, "Codex function call changed its name", p.requestID)
	}
	if item == nil {
		if err := p.addItem(raw, outputIndex); err != nil {
			return nil, err
		}
		item = p.items[len(p.items)-1]
	}
	item.itemDone = true
	if wire.CallID != "" {
		item.callID = wire.CallID
	}
	if wire.Name != "" {
		item.name = wire.Name
	}
	if wire.Type == "function_call" {
		item.finalArguments = wire.Arguments
		item.argumentsDone = true
	}
	if wire.Type == "message" || wire.Type == "reasoning" {
		item.fallbackText = codexOutputItemText(raw, wire.Type)
	}
	if codexOpaqueJSON(wire.EncryptedContent) {
		item.encryptedContent = append(json.RawMessage(nil), wire.EncryptedContent...)
	}
	return item, nil
}

func codexOutputItemText(raw json.RawMessage, kind string) string {
	var wire struct {
		Summary []codexContentPart `json:"summary"`
		Content []codexContentPart `json:"content"`
	}
	if json.Unmarshal(raw, &wire) != nil {
		return ""
	}
	var text strings.Builder
	if kind == "reasoning" {
		for _, part := range wire.Summary {
			if part.Type == "summary_text" {
				text.WriteString(part.Text)
			}
		}
		for _, part := range wire.Content {
			if part.Type == "reasoning_text" {
				text.WriteString(part.Text)
			}
		}
		return text.String()
	}
	for _, part := range wire.Content {
		if part.Type == "output_text" || part.Type == "refusal" {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

func (p *codexStreamProcessor) mergeResponseOutput(output []json.RawMessage) error {
	for index, raw := range output {
		outputIndex := index
		var identity struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal(raw, &identity) != nil || identity.Type == "" {
			return codexTerminalWithRequest(codexCodeStreamFailed, "Codex terminal output item is malformed", p.requestID)
		}
		if _, err := p.finishItem(raw, &outputIndex); err != nil {
			return err
		}
	}
	return nil
}

func (p *codexStreamProcessor) complete(response *codexResponse) (bool, error) {
	if response == nil {
		return false, codexTerminalWithRequest(codexCodeStreamFailed, "Codex response.completed omitted response data", p.requestID)
	}
	if usage := response.Usage.normalized(); usage != (llm.Usage{}) {
		if !p.emit(llm.StreamEvent{Type: llm.SEMessageDelta, Usage: usage}) {
			return false, nil
		}
	}
	if codexHasError(response.Error) {
		return false, codexWireTerminal(response.Error, http.StatusOK, p.requestID, codexCodeStreamFailed, "Codex response contained an error")
	}
	if response.Status != "completed" {
		return false, codexTerminalWithRequest(codexCodeStreamFailed, "Codex response did not complete successfully", p.requestID)
	}
	if err := p.mergeResponseOutput(response.Output); err != nil {
		return false, err
	}
	for _, item := range p.items {
		if item.visibleEmitted || item.fallbackText == "" || (item.kind != "message" && item.kind != "reasoning") ||
			(item.kind == "reasoning" && !p.showThinking) {
			continue
		}
		item.visibleEmitted = true
		visualKind := "text"
		eventType := llm.SETextDelta
		if item.kind == "reasoning" {
			visualKind = "thinking"
			eventType = llm.SEThinkingDelta
		}
		if !p.emitVisible(item, visualKind, llm.StreamEvent{Type: eventType, Text: item.fallbackText}) {
			return false, nil
		}
	}
	callIDs := make(map[string]struct{})
	for _, item := range p.items {
		if item.kind != "function_call" {
			continue
		}
		if !item.itemDone || !item.argumentsDone || item.callID == "" || item.name == "" {
			return false, codexTerminalWithRequest(codexCodeStreamFailed, "Codex function call was incomplete", p.requestID)
		}
		normalizedCallID := codexSanitizeCallID(item.callID)
		if _, duplicate := callIDs[normalizedCallID]; duplicate {
			return false, codexTerminalWithRequest(codexCodeStreamFailed, "Codex reused a function call identity", p.requestID)
		}
		callIDs[normalizedCallID] = struct{}{}
		arguments := item.finalArguments
		if arguments == "" {
			arguments = item.arguments.String()
		}
		if !codexJSONObject(arguments) {
			return false, codexTerminalWithRequest(codexCodeStreamFailed, "Codex function call arguments are malformed", p.requestID)
		}
	}

	hasTools := false
	toolIndex := 0
	for _, item := range p.items {
		if item.kind != "function_call" {
			continue
		}
		hasTools = true
		p.currentVisual = "tool"
		p.blockCount++
		item.block = p.blockCount - 1
		if !p.emit(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: item.callID, ToolName: item.name, ToolIndex: toolIndex}) {
			return false, nil
		}
		arguments := item.finalArguments
		if arguments == "" {
			arguments = item.arguments.String()
		}
		if !p.emit(llm.StreamEvent{Type: llm.SEToolInputJSON, ToolID: item.callID, Text: arguments, ToolIndex: toolIndex}) {
			return false, nil
		}
		toolIndex++
	}

	if state := p.providerState(); state != nil {
		if !p.emit(llm.StreamEvent{Type: llm.SEProviderState, ProviderState: state}) {
			return false, nil
		}
	}
	stopReason := "end_turn"
	if hasTools {
		stopReason = "tool_use"
	}
	if !p.emit(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stopReason}) {
		return false, nil
	}
	if !p.emit(llm.StreamEvent{Type: llm.SEMessageStop}) {
		return false, nil
	}
	p.completed = true
	return false, nil
}

func (p *codexStreamProcessor) providerState() *llm.ProviderState {
	var reasoning []codexReplayReasoning
	for _, item := range p.items {
		if item.kind != "reasoning" || !codexOpaqueJSON(item.encryptedContent) {
			continue
		}
		entry := codexReplayReasoning{
			Before: item.before, EncryptedContent: append(json.RawMessage(nil), item.encryptedContent...),
		}
		if item.block >= 0 {
			entry.Before = item.block
			entry.Summary = true
			entry.SummarySpans = append([]codexReplaySpan(nil), item.summarySpans...)
		}
		reasoning = append(reasoning, entry)
	}
	if len(reasoning) == 0 {
		return nil
	}
	data, err := json.Marshal(codexReplayState{Version: codexReplayStateVersion, Reasoning: reasoning})
	if err != nil {
		return nil
	}
	return &llm.ProviderState{Provider: ProviderCodex, Model: p.model, AuthScope: p.authScope, Data: data}
}

func (p *codexStreamProcessor) failResponse(response *codexResponse, message string) (bool, error) {
	code := codexCodeStreamFailed
	if response != nil {
		if usage := response.Usage.normalized(); usage != (llm.Usage{}) {
			if !p.emit(llm.StreamEvent{Type: llm.SEMessageDelta, Usage: usage}) {
				return false, nil
			}
		}
		if codexHasError(response.Error) {
			return false, codexWireTerminal(response.Error, http.StatusOK, p.requestID, code, message)
		}
		if response.IncompleteDetails != nil {
			if reason := codexSafeToken(response.IncompleteDetails.Reason); reason != "" {
				code = reason
			}
		}
	}
	return false, codexTerminalWithRequest(code, message, p.requestID)
}

func (p *codexStreamProcessor) failFrame(frame codexFrame) (bool, error) {
	if frame.Response != nil {
		if usage := frame.Response.Usage.normalized(); usage != (llm.Usage{}) {
			if !p.emit(llm.StreamEvent{Type: llm.SEMessageDelta, Usage: usage}) {
				return false, nil
			}
		}
		if codexHasError(frame.Response.Error) {
			return false, codexWireTerminal(frame.Response.Error, http.StatusOK, p.requestID, codexCodeStreamFailed, "Codex response failed")
		}
	}
	if codexHasError(frame.Error) {
		return false, codexWireTerminal(frame.Error, http.StatusOK, p.requestID, codexCodeStreamFailed, "Codex response failed")
	}
	wire, _ := json.Marshal(map[string]string{"code": frame.Code, "message": frame.Message})
	return false, codexWireTerminal(wire, http.StatusOK, p.requestID, codexCodeStreamFailed, "Codex response failed")
}

func codexHasError(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null")) && !bytes.Equal(trimmed, []byte("{}"))
}

func codexWireTerminal(raw json.RawMessage, status int, requestID, fallbackCode, message string) error {
	var wire struct {
		Code     string `json:"code"`
		Type     string `json:"type"`
		Status   int    `json:"status"`
		ResetsAt int64  `json:"resets_at"`
	}
	_ = json.Unmarshal(raw, &wire)
	code := codexSafeToken(wire.Code)
	if code == "" {
		code = codexSafeToken(wire.Type)
	}
	if code == "" {
		code = fallbackCode
	}
	if wire.Status >= 400 {
		status = wire.Status
	}
	terminal := &Error{Code: code, Message: message, Status: status, RequestID: requestID}
	if wire.ResetsAt > 0 {
		terminal.ResetAt = time.Unix(wire.ResetsAt, 0)
	}
	return terminal
}

func codexHTTPError(response *http.Response) error {
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		raw = nil
	}
	requestID := codexRequestID(response.Header)
	fallbackCode := codexCodeRequestFailed
	message := "Codex request was rejected"
	switch response.StatusCode {
	case http.StatusUnauthorized:
		fallbackCode = CodeAuthRequired
		message = "Codex authentication was rejected"
	case http.StatusForbidden:
		fallbackCode = CodeConsentRequired
		message = "Codex account is not permitted to make this request"
	case http.StatusTooManyRequests:
		fallbackCode = CodeSubscriptionLimit
		message = "Codex subscription limit was reached"
	}
	err := codexWireTerminal(codexNestedError(raw), response.StatusCode, requestID, fallbackCode, message)
	if terminal, ok := err.(*Error); ok {
		if resetAt := codexResetAt(response.Header, raw); !resetAt.IsZero() {
			terminal.ResetAt = resetAt
		}
	}
	return err
}

func codexNestedError(raw []byte) json.RawMessage {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && codexHasError(envelope.Error) {
		return envelope.Error
	}
	return raw
}

func codexRequestID(headers http.Header) string {
	for _, name := range []string{"x-request-id", "request-id", "x-openai-request-id"} {
		if value := codexSafeToken(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func codexResetAt(headers http.Header, raw []byte) time.Time {
	var envelope struct {
		Error struct {
			ResetsAt int64 `json:"resets_at"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error.ResetsAt > 0 {
		return time.Unix(envelope.Error.ResetsAt, 0)
	}
	var exhausted, fallback int64
	for _, window := range []string{"primary", "secondary"} {
		reset, err := strconv.ParseInt(headers.Get("x-codex-"+window+"-reset-at"), 10, 64)
		if err != nil || reset <= 0 {
			continue
		}
		if fallback == 0 {
			fallback = reset
		}
		used, err := strconv.ParseFloat(headers.Get("x-codex-"+window+"-used-percent"), 64)
		if err == nil && used >= 100 && reset > exhausted {
			exhausted = reset
		}
	}
	if exhausted > 0 {
		return time.Unix(exhausted, 0)
	}
	if fallback > 0 {
		return time.Unix(fallback, 0)
	}
	return time.Time{}
}

func codexSafeToken(value string) string {
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r) {
			continue
		}
		return ""
	}
	return value
}

func codexTransportError(ctx context.Context, _ error) error {
	if ctx.Err() != nil {
		return codexTerminal(codexCodeCanceled, "Codex request was canceled")
	}
	return codexTerminal(codexCodeRequestFailed, "Codex request could not be completed")
}

func codexStreamError(ctx context.Context, err error, requestID string) error {
	if IsTerminal(err) {
		return err
	}
	if ctx.Err() != nil {
		return codexTerminalWithRequest(codexCodeCanceled, "Codex request was canceled", requestID)
	}
	return codexTerminalWithRequest(codexCodeStreamFailed, "Codex response stream failed", requestID)
}

func codexTerminalContext(err error) error {
	if IsTerminal(err) {
		return err
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		return codexTerminal(codexCodeCanceled, "Codex request was canceled")
	}
	return codexTerminal(codexCodeRequestFailed, "Codex request could not be started")
}

func codexTerminal(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func codexTerminalWithRequest(code, message, requestID string) *Error {
	return &Error{Code: code, Message: message, RequestID: requestID}
}

var _ llm.Provider = (*codexProvider)(nil)
