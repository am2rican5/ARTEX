package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/norma/harness"
	"github.com/Autumn-27/artex/internal/norma/llm"
	"github.com/Autumn-27/artex/subscription"
)

type nativeTaskAttemptProbe struct {
	err           error
	failCount     int
	failedUsage   llm.Usage
	successUsage  llm.Usage
	completeCalls int
	streamCalls   int
}

func (p *nativeTaskAttemptProbe) failing(call int) bool {
	return p.failCount < 0 || call <= p.failCount
}

func (p *nativeTaskAttemptProbe) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.completeCalls++
	if p.failing(p.completeCalls) {
		return llm.Message{}, "", p.failedUsage, p.err
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("ok")}}, "end_turn", p.successUsage, nil
}

func (p *nativeTaskAttemptProbe) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		p.streamCalls++
		if p.failing(p.streamCalls) {
			if p.failedUsage != (llm.Usage{}) && !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: p.failedUsage}, nil) {
				return
			}
			yield(llm.StreamEvent{}, p.err)
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: p.successUsage}, nil) {
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "ok"}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func nativeTaskHooks(provider llm.Provider, exhausted *int) taskLLMStreamHooks {
	return taskLLMStreamHooks{
		current: func(context.Context) (taskLLMSelection, error) {
			return taskLLMSelection{profileID: 41, provider: provider}, nil
		},
		exhaust: func(taskLLMSelection, error) (db.TaskLLMTransition, error) {
			(*exhausted)++
			return db.TaskLLMTransition{}, nil
		},
	}
}

func TestTaskRecoveryHoldStopsEveryDispatchMode(t *testing.T) {
	for _, mode := range []string{"complete", "stream"} {
		t.Run(mode, func(t *testing.T) {
			provider := &nativeTaskAttemptProbe{}
			exhausted := 0
			hooks := nativeTaskHooks(provider, &exhausted)
			hooks.admission = func(context.Context) error {
				return subscription.TaskRecoveryError()
			}

			var err error
			if mode == "complete" {
				_, _, _, err = completeTaskLLM(t.Context(), "41", llm.CompletionRequest{}, hooks)
			} else {
				_, err = collectTaskLLMStream(streamTaskLLM(t.Context(), "41", llm.CompletionRequest{}, hooks))
			}
			var recoveryErr *subscription.Error
			if !errors.As(err, &recoveryErr) || recoveryErr.Code != subscription.CodeTaskRecoveryRequired {
				t.Fatalf("error = %T %v, want task recovery required", err, err)
			}
			if !isTaskLLMRuntimeError(err) || retryableWorkerModelError(harness.ReasonModelError, err) {
				t.Fatalf("held dispatch remained replayable: %T %v", err, err)
			}
			if provider.completeCalls != 0 || provider.streamCalls != 0 || exhausted != 0 {
				t.Fatalf("held dispatch reached provider: complete=%d stream=%d exhausted=%d", provider.completeCalls, provider.streamCalls, exhausted)
			}
		})
	}
}

func TestWorkerRetryRejectsWrappedTerminalNativeError(t *testing.T) {
	nativeErr := errors.Join(
		errors.New("compactor failed"),
		&subscription.Error{Code: subscription.CodeAuthRequired, Message: "connection ended before a response"},
	)
	if retryableWorkerModelError(harness.ReasonModelError, nativeErr) {
		t.Fatal("wrapped terminal native error remained eligible for worker replay")
	}
	if !retryableWorkerModelError(harness.ReasonModelError, errors.New("legacy API: status 503")) {
		t.Fatal("legacy API error lost the existing worker retry behavior")
	}
}

func TestTaskAdmissionAllowsOneHealthyDispatch(t *testing.T) {
	provider := &nativeTaskAttemptProbe{}
	exhausted := 0
	checks := 0
	hooks := nativeTaskHooks(provider, &exhausted)
	hooks.admission = func(context.Context) error {
		checks++
		return nil
	}
	msg, _, _, err := completeTaskLLM(t.Context(), "41", llm.CompletionRequest{}, hooks)
	if err != nil || msg.Text() != "ok" {
		t.Fatalf("healthy dispatch = %q / %v", msg.Text(), err)
	}
	if checks != 2 || provider.completeCalls != 1 || exhausted != 0 {
		t.Fatalf("healthy dispatch checks=%d calls=%d exhausted=%d, want 2/1/0", checks, provider.completeCalls, exhausted)
	}
}

func TestTaskNativeTerminalCompleteDoesNotRetryOrAdvance(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "quota",
			err: &subscription.Error{
				Code:    subscription.CodeSubscriptionLimit,
				Message: "status 429 insufficient_quota",
				Status:  429,
			},
		},
		{
			name: "non_quota_no_status",
			err: &subscription.Error{
				Code:    subscription.CodeAuthRequired,
				Message: "connection ended before a response",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLLMFailStreaksForTest()
			wantUsage := llm.Usage{InputTokens: 19, OutputTokens: 3, CacheReadTokens: 4}
			provider := &nativeTaskAttemptProbe{err: tc.err, failCount: -1, failedUsage: wantUsage}
			exhausted := 0
			hooks := nativeTaskHooks(provider, &exhausted)

			for call := 1; call <= llmFailStreakThreshold+1; call++ {
				_, _, usage, err := completeTaskLLM(context.Background(), "native-terminal", llm.CompletionRequest{}, hooks)
				var nativeErr *subscription.Error
				if !errors.As(err, &nativeErr) {
					t.Fatalf("call %d error = %T %v, want native terminal error", call, err, err)
				}
				if !isTaskLLMRuntimeError(err) || retryableWorkerModelError(harness.ReasonModelError, err) {
					t.Fatalf("call %d was not terminal at the task/worker boundary: %T %v", call, err, err)
				}
				if usage != wantUsage {
					t.Fatalf("call %d usage = %+v, want %+v", call, usage, wantUsage)
				}
			}
			if provider.completeCalls != llmFailStreakThreshold+1 {
				t.Fatalf("physical calls = %d, want %d", provider.completeCalls, llmFailStreakThreshold+1)
			}
			if exhausted != 0 {
				t.Fatalf("native terminal failures advanced the task chain %d times", exhausted)
			}
		})
	}
}

func TestTaskNativeTerminalStreamDoesNotReplay(t *testing.T) {
	resetLLMFailStreaksForTest()
	wantUsage := llm.Usage{InputTokens: 23, OutputTokens: 2}
	nativeErr := &subscription.Error{
		Code:    subscription.CodeAuthRequired,
		Message: "connection ended before a response",
	}
	provider := &nativeTaskAttemptProbe{err: nativeErr, failCount: -1, failedUsage: wantUsage}
	exhausted := 0

	events, err := collectTaskLLMStream(streamTaskLLM(context.Background(), "native-stream", llm.CompletionRequest{}, nativeTaskHooks(provider, &exhausted)))
	var gotNative *subscription.Error
	if !errors.As(err, &gotNative) {
		t.Fatalf("error = %T %v, want native terminal error", err, err)
	}
	if !isTaskLLMRuntimeError(err) || retryableWorkerModelError(harness.ReasonModelError, err) {
		t.Fatalf("native stream error was replayable at the worker boundary: %T %v", err, err)
	}
	if len(events) != 1 || events[0].Type != llm.SEMessageStart || events[0].Usage != wantUsage {
		t.Fatalf("preserved stream usage events = %+v, want one message_start with %+v", events, wantUsage)
	}
	if provider.streamCalls != 1 || exhausted != 0 {
		t.Fatalf("physical calls = %d, chain advances = %d; want 1/0", provider.streamCalls, exhausted)
	}
}

func TestTaskLegacy503RetainsSameProviderRetry(t *testing.T) {
	defer withZeroRetryBackoff()()
	resetLLMFailStreaksForTest()
	failedUsage := llm.Usage{InputTokens: 7}
	successUsage := llm.Usage{InputTokens: 11, OutputTokens: 4}
	provider := &nativeTaskAttemptProbe{
		err:          errors.New("legacy API: status 503: unavailable"),
		failCount:    1,
		failedUsage:  failedUsage,
		successUsage: successUsage,
	}
	exhausted := 0

	msg, _, usage, err := completeTaskLLM(context.Background(), "legacy-503", llm.CompletionRequest{}, nativeTaskHooks(provider, &exhausted))
	if err != nil || msg.Text() != "ok" {
		t.Fatalf("legacy retry result = %q / %v", msg.Text(), err)
	}
	wantUsage := failedUsage
	wantUsage.Add(successUsage)
	if usage != wantUsage {
		t.Fatalf("usage = %+v, want %+v", usage, wantUsage)
	}
	if provider.completeCalls != 2 || exhausted != 0 {
		t.Fatalf("physical calls = %d, chain advances = %d; want 2/0", provider.completeCalls, exhausted)
	}
}

func TestSelectedRevokedNativeProfileDoesNotUseGlobalProvider(t *testing.T) {
	skipWithoutDSN(t)
	manager, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	credential, err := manager.pg.CreateLLMCredential(ctx, "anthropic-oauth", fmt.Sprintf("revoked-routing-%d", stamp), json.RawMessage(`{}`))
	if err != nil {
		manager.Close()
		t.Fatal(err)
	}
	credential.Status = "revoked"
	if err := manager.pg.UpdateLLMCredential(ctx, credential, credential.Revision); err != nil {
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		manager.Close()
		t.Fatal(err)
	}
	profileID, err := manager.pg.SaveProfile(&db.LLMProfile{
		Name:         fmt.Sprintf("revoked-native-routing-%d", stamp),
		Format:       "anthropic",
		Model:        "claude-native-routing-fixture",
		AuthMode:     "oauth",
		CredentialID: &credential.ID,
		PoolExclude:  true,
	})
	if err != nil {
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		manager.Close()
		t.Fatal(err)
	}
	task, err := manager.CreateTaskWithOptions("revoked native routing", "stay on the selected account", db.TaskCreateOptions{})
	if err != nil {
		_ = manager.pg.DeleteProfile(profileID)
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		manager.Close()
		t.Fatal(err)
	}
	worker, err := manager.pg.GetAgentByKey("worker")
	if err != nil {
		_, _ = manager.DeleteTask(task.ID, DeleteTaskOptions{})
		_ = manager.pg.DeleteProfile(profileID)
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		manager.Close()
		t.Fatal(err)
	}
	var previousWorkerProfileID *int64
	if worker != nil && worker.LLMProfileID != nil {
		previous := *worker.LLMProfileID
		previousWorkerProfileID = &previous
	}
	t.Cleanup(func() {
		_ = manager.pg.SetAgentLLMProfile("worker", previousWorkerProfileID)
		_, _ = manager.DeleteTask(task.ID, DeleteTaskOptions{})
		_ = manager.pg.DeleteProfile(profileID)
		_, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID)
		manager.Close()
	})
	if err := manager.pg.SetAgentLLMProfile("worker", &profileID); err != nil {
		t.Fatal(err)
	}
	poolWasEnabled := manager.LLMPoolEnabled()
	boundFallbackWasEnabled := manager.LLMPoolBindFallback()
	t.Cleanup(func() {
		_ = manager.SetLLMPoolEnabled(poolWasEnabled)
		_ = manager.SetLLMPoolBindFallback(boundFallbackWasEnabled)
	})
	if err := manager.SetLLMPoolEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetLLMPoolBindFallback(false); err != nil {
		t.Fatal(err)
	}

	global := &nativeTaskAttemptProbe{successUsage: llm.Usage{InputTokens: 1}}
	server := &Server{m: manager, llmOn: true, llmProv: global, provByProfile: map[int64]*provEntry{}}
	runtime := &taskLLMRuntime{s: server, taskID: task.ID, agentKey: "worker"}
	_, _, _, err = runtime.Complete(ctx, llm.CompletionRequest{})
	if !subscription.IsTerminal(err) || !isTaskLLMRuntimeError(err) ||
		retryableWorkerModelError(harness.ReasonModelError, err) {
		t.Fatalf("selected revoked profile error = %T %v, want non-replayable terminal native error", err, err)
	}
	if global.completeCalls != 0 || global.streamCalls != 0 {
		t.Fatalf("global API provider received complete=%d stream=%d physical calls", global.completeCalls, global.streamCalls)
	}
}
