package llmrec

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/norma/llm"
	"github.com/Autumn-27/artex/internal/norma/transcript"
	"github.com/google/uuid"
)

type completeProvider struct{}

func (completeProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(func(llm.StreamEvent, error) bool) {}
}

func (completeProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			{Type: llm.BlockThinking, Thinking: "reasoning"},
			llm.TextBlock("answer"),
		},
	}, "stop", llm.Usage{InputTokens: 7, OutputTokens: 3}, nil
}

func TestTaskIDContextUsesExplicitRegistryID(t *testing.T) {
	ctx := WithTaskID(context.Background(), " 42 ")
	if got := TaskIDFrom(ctx); got != "42" {
		t.Fatalf("TaskIDFrom()=%q want 42", got)
	}
	if got := TaskIDFrom(WithTaskID(ctx, "   ")); got != "42" {
		t.Fatalf("blank task id should preserve parent context, got %q", got)
	}
	if got := TaskIDFrom(nil); got != "" {
		t.Fatalf("nil context returned %q", got)
	}
}

func TestParseSessionFallbackIsExplorationScoped(t *testing.T) {
	taskID, worker := parseSession("exp12-worker-i99")
	if taskID != "12" || worker != "worker" {
		t.Fatalf("parseSession()=(%q,%q)", taskID, worker)
	}
	if taskID, worker := parseSession("not-a-task"); taskID != "" || worker != "" {
		t.Fatalf("unexpected non-session parse: (%q,%q)", taskID, worker)
	}
}

func TestCompleteForwardsAtomicResponse(t *testing.T) {
	recorder := Wrap(completeProvider{}, nil, "model", "profile", "", "", func() bool { return false })
	msg, stopReason, usage, err := recorder.Complete(context.Background(), llm.CompletionRequest{})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	if got := msg.Text(); got != "answer" {
		t.Fatalf("Complete() text=%q want answer", got)
	}
	if stopReason != "stop" {
		t.Fatalf("Complete() stop reason=%q want stop", stopReason)
	}
	if usage.InputTokens != 7 || usage.OutputTokens != 3 {
		t.Fatalf("Complete() usage=%+v", usage)
	}
}

func TestRecorderSerializesProviderStateAndCompletionTruth(t *testing.T) {
	state := &llm.ProviderState{
		Provider:  "openai-codex-oauth",
		Model:     "codex-native",
		AuthScope: "account:workspace",
		Data:      json.RawMessage(`{"version":1,"reasoning":[{"id":"r1"}]}`),
	}
	msg := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("answer")}, ProviderState: state}
	recorder := Wrap(completeProvider{}, nil, "codex-native", "native", "", "", nil)
	requestBody := recorder.serializeRequest(llm.CompletionRequest{Messages: []llm.Message{msg}})
	var request struct {
		Messages []llm.Message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(requestBody), &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 1 || request.Messages[0].ProviderState == nil || string(request.Messages[0].ProviderState.Data) != string(state.Data) {
		t.Fatalf("normalized request lost provider state: %s", requestBody)
	}

	responseBody := serializeResponse(msg, llm.Usage{InputTokens: 8, OutputTokens: 3}, "", false)
	var response struct {
		Message   llm.Message `json:"message"`
		Completed bool        `json:"completed"`
	}
	if err := json.Unmarshal([]byte(responseBody), &response); err != nil {
		t.Fatal(err)
	}
	if response.Completed || response.Message.ProviderState == nil || string(response.Message.ProviderState.Data) != string(state.Data) {
		t.Fatalf("partial normalized response changed completion/state: %s", responseBody)
	}
	response.Message.ProviderState.Data[0] = '['
	if state.Data[0] != '{' {
		t.Fatal("serialized response aliases the provider-owned state")
	}
}

type nativeErrorProvider struct {
	completeProvider
	err error
}

func (p nativeErrorProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("partial")}}, "", llm.Usage{InputTokens: 13, OutputTokens: 5}, p.err
}

func TestCompletePreservesNativeErrorUsage(t *testing.T) {
	wantErr := errors.New("native terminal failure")
	var metered []llm.Usage
	ctx := WithUsageSink(t.Context(), func(usage llm.Usage) { metered = append(metered, usage) })
	recorder := Wrap(nativeErrorProvider{err: wantErr}, nil, "native-model", "native", "", "", func() bool { return false })
	msg, stop, usage, err := recorder.Complete(ctx, llm.CompletionRequest{})
	if !errors.Is(err, wantErr) || stop != "" || msg.Text() != "partial" || usage != (llm.Usage{InputTokens: 13, OutputTokens: 5}) {
		t.Fatalf("Complete()=(%+v,%q,%+v,%v)", msg, stop, usage, err)
	}
	if len(metered) != 1 || metered[0] != usage {
		t.Fatalf("metered usage=%+v want one %+v", metered, usage)
	}
}

type nativeStreamMetadataProvider struct {
	completeProvider
	state *llm.ProviderState
	fail  bool
}

func (p nativeStreamMetadataProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for _, event := range []llm.StreamEvent{
			{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 31}},
			{Type: llm.SEToolUseStart, ToolID: "a", ToolName: "read"},
			{Type: llm.SEToolUseStart, ToolID: "b", ToolName: "read"},
			{Type: llm.SEToolInputJSON, ToolID: "b", Text: `{"n":2}`},
			{Type: llm.SEToolInputJSON, ToolID: "a", Text: `{"n":1}`},
			{Type: llm.SEProviderState, ProviderState: p.state},
			{Type: llm.SEMessageDelta, StopReason: "tool_use", Usage: llm.Usage{OutputTokens: 7}},
		} {
			if !yield(event, nil) {
				return
			}
		}
		if p.fail {
			yield(llm.StreamEvent{}, errors.New("native stream failed"))
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func TestStreamForwardsNativeMetadataWithoutFalseCompletion(t *testing.T) {
	state := &llm.ProviderState{
		Provider: "openai-codex-oauth", Model: "codex-native", AuthScope: "account:workspace",
		Data: json.RawMessage(`{"version":1,"reasoning":[{"id":"r1"}]}`),
	}
	recorder := Wrap(nativeStreamMetadataProvider{state: state}, nil, "native-model", "native", "", "", func() bool { return false })
	acc := llm.NewAccumulator()
	for event, err := range recorder.Stream(t.Context(), llm.CompletionRequest{}) {
		if err != nil {
			t.Fatal(err)
		}
		acc.Add(event)
	}
	msg := acc.Message()
	uses := msg.ToolUses()
	if len(uses) != 2 || string(uses[0].Input) != `{"n":1}` || string(uses[1].Input) != `{"n":2}` || msg.ProviderState == nil {
		t.Fatalf("recorder changed native stream metadata: message=%+v", msg)
	}

	var metered []llm.Usage
	ctx := WithUsageSink(t.Context(), func(usage llm.Usage) { metered = append(metered, usage) })
	failed := Wrap(nativeStreamMetadataProvider{state: state, fail: true}, nil, "native-model", "native", "", "", func() bool { return false })
	stops := 0
	var streamErr error
	for event, err := range failed.Stream(ctx, llm.CompletionRequest{}) {
		if event.Type == llm.SEMessageStop {
			stops++
		}
		if err != nil {
			streamErr = err
		}
	}
	if streamErr == nil || stops != 0 {
		t.Fatalf("partial native stream err=%v stops=%d", streamErr, stops)
	}
	wantUsage := llm.Usage{InputTokens: 31, OutputTokens: 7}
	if len(metered) != 1 || metered[0] != wantUsage {
		t.Fatalf("partial native usage=%+v want one %+v", metered, wantUsage)
	}
}

// The usage sink sees each metered call once, with the usage the llm_usage row gets.
func TestUsageSinkSeesMeteredCalls(t *testing.T) {
	var got []llm.Usage
	ctx := WithUsageSink(context.Background(), func(u llm.Usage) { got = append(got, u) })
	recorder := Wrap(meteredStreamProvider{}, nil, "model", "profile", "", "", func() bool { return false })
	for _, err := range recorder.Stream(ctx, llm.CompletionRequest{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := recorder.Complete(ctx, llm.CompletionRequest{}); err != nil {
		t.Fatal(err)
	}
	want := []llm.Usage{{InputTokens: 23, OutputTokens: 5}, {InputTokens: 7, OutputTokens: 3}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("sink saw %+v, want %+v", got, want)
	}
	// A call that gets no llm_usage row (no model, zero usage) is not seen either.
	got = nil
	empty := Wrap(completeProvider{}, nil, "", "", "", "", nil)
	empty.recordUsage(ctx, "", 0, "", llm.Usage{}, 0, "ok")
	if len(got) != 0 {
		t.Fatalf("sink saw an unmetered call: %+v", got)
	}
}

type meteredStreamProvider struct{ completeProvider }

func (meteredStreamProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 23}}, nil) {
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, Usage: llm.Usage{OutputTokens: 5}}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func TestSideUsageRecordedOnceOnConsumerCancellation(t *testing.T) {
	t.Skip("timing-sensitive: passes locally, flaky on CI runners; tracked in BaaRai/ARTEX#9")
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	for _, early := range []bool{false, true} {
		profile := "btw-metering-" + uuid.NewString()
		ctx, cancel := context.WithCancel(transcript.WithSessionID(t.Context(), "exp0-btw-test"))
		recorder := Wrap(meteredStreamProvider{}, pg, "fixture", profile, "", "", func() bool { return false })
		for event, err := range recorder.Stream(ctx, llm.CompletionRequest{}) {
			if err != nil {
				t.Fatal(err)
			}
			if early && event.Type == llm.SEMessageDelta {
				cancel()
				break // Consumer exits before the provider can return a cancellation event.
			}
		}
		cancel()
		var count, input, output int
		var worker, status string
		err = pg.QueryRow(`SELECT count(*),max(input_tokens),max(output_tokens),max(worker),max(status)
FROM llm_usage WHERE profile_name=$1`, profile).Scan(&count, &input, &output, &worker, &status)
		_, _ = pg.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile)
		if err != nil {
			t.Fatal(err)
		}
		wantStatus := "ok"
		if early {
			wantStatus = "error"
		}
		if count != 1 || input != 23 || output != 5 || worker != "btw" || status != wantStatus {
			t.Fatalf("early=%v: count=%d usage=%d/%d worker=%q status=%q", early, count, input, output, worker, status)
		}
	}
}
