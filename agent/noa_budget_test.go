package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/internal/norma/agentcore"
	"github.com/Autumn-27/artex/internal/norma/harness"
	"github.com/Autumn-27/artex/internal/norma/llm"
	"github.com/Autumn-27/artex/internal/norma/noaadapter"
)

// A worker whose protected context exceeds a tiny NOA window ends with
// model_error and an error that still wraps ErrNoaBudget after the Norma harness
// and the capture layer: the engine's retry check relies on that.
func TestWorkerNoaBudgetRefusalKeepsSentinelThroughHarness(t *testing.T) {
	f := newSessionFixture(t)
	inner := &countingProvider{}
	w := NewWorker(inner, "test-model", t.TempDir(), nil, 2000, 3)
	w.SetNoaEnabled(func() bool { return true })
	reason, _, err := w.Execute(t.Context(), "work#1", f.expID, nil, f.ts, f.intent, passHooks{}, nil, nil, nil)
	if reason != harness.ReasonModelError || !errors.Is(err, ErrNoaBudget) || inner.calls != 0 {
		t.Fatalf("reason=%q err=%v provider calls=%d; want model_error wrapping ErrNoaBudget and no call", reason, err, inner.calls)
	}
}

// countingProvider answers with one text turn and counts the calls it receives.
type countingProvider struct{ calls int }

func (p *countingProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.calls++
	return func(yield func(llm.StreamEvent, error) bool) {
		if yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "ok"}, nil) {
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}
}

func (p *countingProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ok")}}, "end_turn", llm.Usage{}, nil
}

// withNoaEnable swaps the noaadapter.Enable seam for the duration of a test.
func withNoaEnable(t *testing.T, fn func(*agentcore.Options, noaadapter.Options) error) {
	t.Helper()
	prev := noaEnable
	noaEnable = fn
	t.Cleanup(func() { noaEnable = prev })
}

func TestEnableNoaUsesConfiguredWindow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		compaction func() *agentcore.Options
		want       int
	}{
		{"configured", func() *agentcore.Options { return &agentcore.Options{Compaction: compactionConfig(32000)} }, 32000},
		{"default", func() *agentcore.Options { return &agentcore.Options{} }, defaultWindowK * 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got noaadapter.Options
			withNoaEnable(t, func(opts *agentcore.Options, o noaadapter.Options) error {
				got = o
				return noaadapter.Enable(opts, o)
			})
			inner := &countingProvider{}
			opts := tc.compaction()
			opts.Provider = inner
			enableNoa(opts, func() bool { return true }, t.TempDir(), "noa-window", nil)

			if got.ModelContextLimit != tc.want {
				t.Fatalf("noaadapter.Options.ModelContextLimit=%d, want %d", got.ModelContextLimit, tc.want)
			}
			bp, ok := opts.Provider.(*noaBudgetProvider)
			if !ok || bp.window != tc.want || bp.Provider != inner {
				t.Fatalf("provider %T %+v, want *noaBudgetProvider{window %d} around the original", opts.Provider, opts.Provider, tc.want)
			}
			if opts.Compaction != nil || opts.Compactor == nil {
				t.Fatalf("Compaction=%v Compactor=%v; noa must replace the built-in compaction", opts.Compaction, opts.Compactor)
			}
		})
	}
}

func irreducibleRequest() llm.CompletionRequest {
	return llm.CompletionRequest{System: []string{strings.Repeat("protected system prompt ", 8000)}, MaxTokens: 8000}
}

func TestNoaBudgetRefusesIrreducibleRequestBeforeTransport(t *testing.T) {
	t.Run("atomic", func(t *testing.T) {
		inner := &countingProvider{}
		p := &noaBudgetProvider{Provider: inner, window: 32000}
		_, _, _, err := p.Complete(t.Context(), irreducibleRequest())
		if err == nil || !strings.Contains(err.Error(), "exceeds configured budget 32000") || inner.calls != 0 {
			t.Fatalf("err=%v provider calls=%d", err, inner.calls)
		}
		if !errors.Is(err, ErrNoaBudget) {
			t.Fatalf("atomic refusal does not wrap ErrNoaBudget: %v", err)
		}
		if _, _, _, err := p.Complete(t.Context(), llm.CompletionRequest{System: []string{"short"}, MaxTokens: 1000}); err != nil || inner.calls != 1 {
			t.Fatalf("a request that fits was refused or not sent: err=%v calls=%d", err, inner.calls)
		}
	})
	t.Run("stream", func(t *testing.T) {
		inner := &countingProvider{}
		p := &noaBudgetProvider{Provider: inner, window: 32000}
		var events int
		var err error
		for ev, e := range p.Stream(t.Context(), irreducibleRequest()) {
			if e != nil {
				err = e
				break
			}
			_ = ev
			events++
		}
		if err == nil || !strings.Contains(err.Error(), "exceeds configured budget 32000") || events != 0 || inner.calls != 0 {
			t.Fatalf("err=%v events=%d provider calls=%d", err, events, inner.calls)
		}
		if !errors.Is(err, ErrNoaBudget) {
			t.Fatalf("stream refusal does not wrap ErrNoaBudget: %v", err)
		}
		for _, e := range p.Stream(t.Context(), llm.CompletionRequest{System: []string{"short"}, MaxTokens: 1000}) {
			if e != nil {
				t.Fatal(e)
			}
		}
		if inner.calls != 1 {
			t.Fatalf("a request that fits was not streamed: calls=%d", inner.calls)
		}
	})
}

func TestEnableNoaFailureKeepsBuiltInCompaction(t *testing.T) {
	withNoaEnable(t, func(*agentcore.Options, noaadapter.Options) error { return errors.New("archive dir unusable") })
	inner := &countingProvider{}
	compaction := compactionConfig(32000)
	opts := &agentcore.Options{Compaction: compaction, Provider: inner}
	var warns []string
	enableNoa(opts, func() bool { return true }, t.TempDir(), "noa-fail", func(m string) { warns = append(warns, m) })

	if len(warns) != 1 || !strings.Contains(warns[0], "falling back to built-in compaction") || !strings.Contains(warns[0], "archive dir unusable") {
		t.Fatalf("warnings %q", warns)
	}
	if opts.Compaction != compaction || opts.Compactor != nil {
		t.Fatalf("Compaction=%v Compactor=%v; the built-in compaction must stay", opts.Compaction, opts.Compactor)
	}
	if opts.Provider != llm.Provider(inner) {
		t.Fatalf("provider %T was wrapped although noa is off", opts.Provider)
	}
}

type nativeContextProvider struct {
	state *llm.ProviderState
	err   error
}

func (p nativeContextProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for _, event := range []llm.StreamEvent{
			{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 21}},
			{Type: llm.SEToolUseStart, ToolID: "a", ToolName: "read"},
			{Type: llm.SEToolUseStart, ToolID: "b", ToolName: "read"},
			{Type: llm.SEToolInputJSON, ToolID: "b", Text: `{"n":2}`},
			{Type: llm.SEToolInputJSON, ToolID: "a", Text: `{"n":1}`},
			{Type: llm.SEProviderState, ProviderState: p.state},
			{Type: llm.SEMessageDelta, StopReason: "tool_use", Usage: llm.Usage{OutputTokens: 6}},
			{Type: llm.SEMessageStop},
		} {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (p nativeContextProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("partial")}, ProviderState: p.state}, "", llm.Usage{InputTokens: 9, OutputTokens: 2}, p.err
}

func TestNativeContextSurvivesNoaBudgetAndProjection(t *testing.T) {
	state := &llm.ProviderState{
		Provider:  "openai-codex-oauth",
		Model:     "codex-native",
		AuthScope: "account:workspace",
		Data:      json.RawMessage(`{"version":1,"reasoning":[{"id":"r1"}]}`),
	}
	wantErr := errors.New("native terminal failure")
	p := &noaBudgetProvider{Provider: nativeContextProvider{state: state, err: wantErr}, window: 32000}
	acc := llm.NewAccumulator()
	for event, err := range p.Stream(t.Context(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("short")}}) {
		if err != nil {
			t.Fatal(err)
		}
		acc.Add(event)
	}
	streamed := acc.Message()
	uses := streamed.ToolUses()
	if len(uses) != 2 || string(uses[0].Input) != `{"n":1}` || string(uses[1].Input) != `{"n":2}` {
		t.Fatalf("NOA budget wrapper lost explicit tool IDs: %+v", uses)
	}
	if streamed.ProviderState == nil || string(streamed.ProviderState.Data) != string(state.Data) || acc.Usage != (llm.Usage{InputTokens: 21, OutputTokens: 6}) {
		t.Fatalf("NOA budget wrapper lost state/usage: message=%+v usage=%+v", streamed, acc.Usage)
	}
	atomic, stop, usage, err := p.Complete(t.Context(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("short")}})
	if !errors.Is(err, wantErr) || stop != "" || usage != (llm.Usage{InputTokens: 9, OutputTokens: 2}) || atomic.ProviderState == nil {
		t.Fatalf("atomic metadata changed: message=%+v stop=%q usage=%+v err=%v", atomic, stop, usage, err)
	}

	original := []llm.Message{
		llm.UserText("question"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("original answer")}, ProviderState: state},
	}
	cores, sidecar := noaadapter.Project(original)
	roundTrip := noaadapter.Reassemble(cores, original, sidecar, noaadapter.ReassembleOptions{})
	if len(roundTrip) != 2 || roundTrip[1].ProviderState == nil || string(roundTrip[1].ProviderState.Data) != string(state.Data) {
		t.Fatalf("unchanged NOA projection lost provider state: %+v", roundTrip)
	}
	roundTrip[1].ProviderState.Data[0] = '['
	if state.Data[0] != '{' {
		t.Fatal("NOA projection aliases original provider state")
	}
	cores[1].Text = "compressed answer"
	transformed := noaadapter.Reassemble(cores, original, sidecar, noaadapter.ReassembleOptions{})
	if len(transformed) != 2 || transformed[1].Text() != "compressed answer" || transformed[1].ProviderState != nil {
		t.Fatalf("transformed NOA message retained stale provider state: %+v", transformed)
	}
}
