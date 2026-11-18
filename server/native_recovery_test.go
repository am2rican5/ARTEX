package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/norma/llm"
	"github.com/Autumn-27/artex/subscription"
)

func newNativeRecoveryFixture(t *testing.T) (*Manager, *Server, *Task) {
	t.Helper()
	skipWithoutDSN(t)
	manager, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	task, err := manager.CreateTask("native recovery fixture", "do not replay uncertain work", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.pg.DeleteTask(taskID) })
	engine := NewEngine(manager)
	return manager, &Server{m: manager, engine: engine, ctx: t.Context()}, task
}

func requireNativeRecoveryError(t *testing.T, err error) {
	t.Helper()
	var nativeErr *subscription.Error
	if !errors.As(err, &nativeErr) || nativeErr.Code != subscription.CodeTaskRecoveryRequired {
		t.Fatalf("error = %T %v, want %s", err, err, subscription.CodeTaskRecoveryRequired)
	}
}

func requireRecoveryWarning(t *testing.T, task *Task) {
	t.Helper()
	activities, _, err := task.Store.ActivityList(nil, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, activity := range activities {
		if activity.Worker == "system" && activity.Kind == "text" && activity.IsError && activity.Summary == nativeRecoveryWarning {
			return
		}
	}
	t.Fatalf("visible native recovery warning not found in %+v", activities)
}

type plannerRecoveryProbe struct {
	calls    atomic.Int64
	firstErr error
}

func (p *plannerRecoveryProbe) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	call := p.calls.Add(1)
	return func(yield func(llm.StreamEvent, error) bool) {
		if call == 1 && p.firstErr != nil {
			yield(llm.StreamEvent{}, p.firstErr)
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "done"}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
	}
}

func (p *plannerRecoveryProbe) Complete(ctx context.Context, request llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return accumulateStreamForTest(ctx, p.Stream, request)
}

type scopedRecoveryProvider struct {
	taskID string
	inner  llm.Provider
}

func (p scopedRecoveryProvider) Stream(ctx context.Context, request llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return p.inner.Stream(subscription.WithTaskScope(ctx, p.taskID), request)
}

func (p scopedRecoveryProvider) Complete(ctx context.Context, request llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.inner.Complete(subscription.WithTaskScope(ctx, p.taskID), request)
}

func waitForNativeRecovery(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runRecoveryPlanner(t *testing.T, server *Server, task *Task, provider llm.Provider) {
	t.Helper()
	if _, err := task.Store.AddGoal(map[string]any{"summary": "recovery probe"}, "seed"); err != nil {
		t.Fatal(err)
	}
	planner := agent.NewPlanner(provider, "recovery-probe", server.m.dir, nil, 0, 1)
	server.engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) {
		return planner, nil
	})
	server.engine.debounce = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.engine.plannerLoop(ctx, task)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	task.Notify()
}

func TestPlannerTerminalNativeFailureRequiresExplicitResume(t *testing.T) {
	manager, server, task := newNativeRecoveryFixture(t)
	nativeProvider, physicalRequests := newSyntheticNativeProviderWithFirstFailure(t, manager, http.StatusTooManyRequests)
	provider := scopedRecoveryProvider{taskID: task.ID, inner: nativeProvider}
	runRecoveryPlanner(t, server, task, provider)

	waitForNativeRecovery(t, "terminal native planner hold", func() bool {
		return physicalRequests.Load() == 1 && task.lifecycleSnapshot().NativeRecoveryHold
	})
	waitForNativeRecovery(t, "terminal native planner round settlement", func() bool {
		return server.engine.inflightCount(task.ID) == 0
	})
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	state, err := manager.pg.NativeTaskRecoveryState(t.Context(), taskID)
	if err != nil || !state.Used || !state.Hold {
		t.Fatalf("terminal planner recovery state = %+v / %v", state, err)
	}
	if !server.engine.IsPaused(task.ID) || !task.lifecycleSnapshot().Paused {
		t.Fatal("terminal native planner error did not pause execution")
	}
	requireRecoveryWarning(t, task)

	task.Notify()
	task.Notify()
	time.Sleep(4*server.engine.debounce + 20*time.Millisecond)
	if calls := physicalRequests.Load(); calls != 1 {
		t.Fatalf("duplicate planner notifications dispatched %d physical requests, want 1", calls)
	}

	status := task.lifecycleSnapshot().Status
	if err := manager.ApplyTaskAdmission(task.ID, status, status, false, "", false, true); err != nil {
		t.Fatal(err)
	}
	server.engine.Resume(task)
	waitForNativeRecovery(t, "explicitly resumed planner request", func() bool {
		return physicalRequests.Load() == 2
	})
	time.Sleep(4*server.engine.debounce + 20*time.Millisecond)
	if calls := physicalRequests.Load(); calls != 2 {
		t.Fatalf("explicit Resume dispatched %d total physical requests, want 2", calls)
	}
	if state, err = manager.pg.NativeTaskRecoveryState(t.Context(), taskID); err != nil || !state.Used || state.Hold {
		t.Fatalf("resumed native planner recovery state = %+v / %v", state, err)
	}
}

func TestPlannerLegacyErrorRetainsOrdinaryNextRound(t *testing.T) {
	manager, server, task := newNativeRecoveryFixture(t)
	provider := &plannerRecoveryProbe{firstErr: errors.New("legacy API: status 503: unavailable")}
	runRecoveryPlanner(t, server, task, provider)

	waitForNativeRecovery(t, "legacy planner error", func() bool {
		return provider.calls.Load() == 1
	})
	waitForNativeRecovery(t, "legacy planner round settlement", func() bool {
		return server.engine.inflightCount(task.ID) == 0
	})
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	state, err := manager.pg.NativeTaskRecoveryState(t.Context(), taskID)
	if err != nil || state.Used || state.Hold {
		t.Fatalf("legacy planner error changed native recovery state = %+v / %v", state, err)
	}
	if server.engine.IsPaused(task.ID) || task.lifecycleSnapshot().NativeRecoveryHold {
		t.Fatal("legacy planner error installed a native recovery barrier")
	}

	task.Notify()
	waitForNativeRecovery(t, "ordinary legacy retry round", func() bool {
		return provider.calls.Load() == 2
	})
}

func TestStartupNativeRecoveryBlocksUntilCommittedAcknowledgement(t *testing.T) {
	manager, server, task := newNativeRecoveryFixture(t)
	if err := manager.pg.MarkNativeTaskUsage(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}

	held, err := server.prepareNativeTaskRecovery(task)
	if err != nil || !held {
		t.Fatalf("prepare recovery = held %v, err %v", held, err)
	}
	if !task.Paused || !task.NativeRecoveryHold || !server.engine.IsPaused(task.ID) {
		t.Fatalf("startup barrier missing: paused=%v hold=%v enginePaused=%v", task.Paused, task.NativeRecoveryHold, server.engine.IsPaused(task.ID))
	}
	requireRecoveryWarning(t, task)

	provider := &nativeTaskAttemptProbe{}
	exhausted := 0
	hooks := nativeTaskHooks(provider, &exhausted)
	hooks.admission = func(ctx context.Context) error { return server.ensureNativeTaskAdmission(ctx, task.ID) }
	_, _, _, err = completeTaskLLM(t.Context(), task.ID, llm.CompletionRequest{}, hooks)
	requireNativeRecoveryError(t, err)
	if provider.completeCalls != 0 {
		t.Fatalf("startup recovery dispatched %d physical requests", provider.completeCalls)
	}

	status := task.lifecycleSnapshot().Status
	if err := manager.ApplyTaskAdmission(task.ID, "wrong-status", status, false, "", false, true); err == nil {
		t.Fatal("acknowledgement with a failed lifecycle CAS unexpectedly committed")
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	state, err := manager.pg.NativeTaskRecoveryState(t.Context(), taskID)
	if err != nil || !state.Used || !state.Hold {
		t.Fatalf("failed acknowledgement changed recovery state: %+v / %v", state, err)
	}

	if err := manager.ApplyTaskAdmission(task.ID, status, status, false, "", false, true); err != nil {
		t.Fatal(err)
	}
	server.engine.Resume(task)
	_, _, _, err = completeTaskLLM(t.Context(), task.ID, llm.CompletionRequest{}, hooks)
	if err != nil {
		t.Fatalf("explicitly resumed dispatch: %v", err)
	}
	if provider.completeCalls != 1 {
		t.Fatalf("explicit Resume dispatched %d physical requests, want 1", provider.completeCalls)
	}
}

type nativeRecoveryRewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t nativeRecoveryRewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	rewritten := *request.URL
	rewritten.Scheme = t.target.Scheme
	rewritten.Host = t.target.Host
	cloned.URL = &rewritten
	cloned.Host = t.target.Host
	return t.base.RoundTrip(cloned)
}

func nativeRecoveryAuthScope(provider string, tokens subscription.TokenSet) string {
	identity := struct {
		Provider  string `json:"provider"`
		Issuer    string `json:"issuer"`
		ClientID  string `json:"client_id"`
		AccountID string `json:"account_id"`
		OrgID     string `json:"org_id"`
		Subject   string `json:"subject"`
	}{provider, tokens.Issuer, tokens.ClientID, tokens.AccountID, tokens.OrgID, tokens.Subject}
	encoded, _ := json.Marshal(identity)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func newSyntheticNativeProvider(t *testing.T, manager *Manager) (llm.Provider, *atomic.Int64) {
	t.Helper()
	return newSyntheticNativeProviderWithFirstFailure(t, manager, 0)
}

func newSyntheticNativeProviderWithFirstFailure(t *testing.T, manager *Manager, firstFailureStatus int) (llm.Provider, *atomic.Int64) {
	t.Helper()
	name := "native recovery synthetic " + strconv.FormatInt(time.Now().UnixNano(), 10)
	hits := &atomic.Int64{}
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		call := hits.Add(1)
		if call == 1 && firstFailureStatus != 0 {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(firstFailureStatus)
			_, _ = io.WriteString(writer, `{"error":{"message":"synthetic native request failed","type":"insufficient_quota","code":"insufficient_quota"}}`)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		const response = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"continued"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"continued"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}

`
		_, _ = io.WriteString(writer, response)
	}))
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)

	tokens := subscription.TokenSet{
		AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(time.Hour),
		AccountID: "acct-native-recovery", Subject: "subject-native-recovery",
	}
	payload, err := json.Marshal(map[string]any{
		"version": 1, "provider": subscription.ProviderCodex,
		"auth_scope": nativeRecoveryAuthScope(subscription.ProviderCodex, tokens), "tokens": tokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := manager.pg.CreateLLMCredential(t.Context(), subscription.ProviderCodex, name, payload)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = manager.pg.Exec(`DELETE FROM llm_credentials WHERE id=$1`, credential.ID) })
	profileID, err := manager.pg.SaveProfile(&db.LLMProfile{
		Name: name, Format: "openai-responses", Model: "gpt-5.6-sol",
		AuthMode: "oauth", CredentialID: &credential.ID, PoolExclude: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.pg.DeleteProfile(profileID) })
	profile, err := manager.pg.ProfileByID(profileID)
	if err != nil || profile == nil {
		t.Fatalf("load synthetic native profile: %v", err)
	}
	provider, err := subscription.NewCodexProvider(manager.Subscriptions(), subscription.ProviderConfig{
		Binding: subscription.Binding{ProfileID: profileID, CredentialID: credential.ID, Generation: profile.AuthBindingGeneration},
		Model:   "gpt-5.6-sol", ThinkingType: "enabled", ReasoningEffort: "high",
		HTTPClient: &http.Client{Transport: nativeRecoveryRewriteTransport{target: target, base: transport}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider, hits
}

func TestRecoveryHoldCountsPhysicalNativeHTTPDispatches(t *testing.T) {
	manager, server, task := newNativeRecoveryFixture(t)
	provider, hits := newSyntheticNativeProvider(t, manager)
	if err := manager.pg.MarkNativeTaskUsage(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	if held, err := server.prepareNativeTaskRecovery(task); err != nil || !held {
		t.Fatalf("prepare recovery = held %v, err %v", held, err)
	}
	exhausted := 0
	hooks := nativeTaskHooks(provider, &exhausted)
	hooks.admission = func(ctx context.Context) error { return server.ensureNativeTaskAdmission(ctx, task.ID) }
	callCtx := subscription.WithTaskScope(t.Context(), task.ID)
	_, _, _, err := completeTaskLLM(callCtx, task.ID, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("continue")}}, hooks)
	requireNativeRecoveryError(t, err)
	if hits.Load() != 0 {
		t.Fatalf("held startup recovery sent %d native HTTP requests", hits.Load())
	}

	status := task.lifecycleSnapshot().Status
	if err := manager.ApplyTaskAdmission(task.ID, status, status, false, "", false, true); err != nil {
		t.Fatal(err)
	}
	server.engine.Resume(task)
	message, _, _, err := completeTaskLLM(callCtx, task.ID, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("continue")}}, hooks)
	if err != nil || message.Text() != "continued" {
		t.Fatalf("resumed native completion = %q / %v", message.Text(), err)
	}
	if hits.Load() != 1 {
		t.Fatalf("explicit Resume sent %d native HTTP requests, want 1", hits.Load())
	}
}

func TestPeriodicExpiredNativeClaimHoldsAndRemainsResumable(t *testing.T) {
	manager, server, task := newNativeRecoveryFixture(t)
	provider, hits := newSyntheticNativeProvider(t, manager)
	intentID, err := task.Store.AddIntent(map[string]any{"summary": "native request in flight"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if token, err := task.Store.ClaimIntentToken(intentID, "work#1"); err != nil || token == "" {
		t.Fatalf("claim token=%q err=%v", token, err)
	}
	blockedID, err := task.Store.AddIntent(map[string]any{"summary": "existing provider failure"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.pg.Exec(`UPDATE exploration_nodes SET state='blocked',blocked_reason='provider_unavailable' WHERE id=$1`, blockedID); err != nil {
		t.Fatal(err)
	}
	if err := manager.pg.MarkNativeTaskUsage(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.pg.Exec(`UPDATE exploration_nodes SET last_heartbeat_at=now()-$2*INTERVAL '1 second' WHERE id=$1`, intentID, (db.IntentClaimTTL + time.Second).Seconds()); err != nil {
		t.Fatal(err)
	}

	server.engine.sweepExpiredClaims(task)
	if !task.NativeRecoveryHold || !task.Paused || !server.engine.IsPaused(task.ID) {
		t.Fatalf("periodic barrier missing: paused=%v hold=%v enginePaused=%v", task.Paused, task.NativeRecoveryHold, server.engine.IsPaused(task.ID))
	}
	intent, err := task.Store.GetNode(intentID)
	if err != nil || intent == nil || intent.State != "blocked" || intent.BlockedReason != "native_outcome_unknown" {
		t.Fatalf("expired native intent = %+v / %v", intent, err)
	}
	blocked, err := task.Store.GetNode(blockedID)
	if err != nil || blocked == nil || blocked.State != "blocked" || blocked.BlockedReason != "provider_unavailable" {
		t.Fatalf("existing blocked failure changed: %+v / %v", blocked, err)
	}
	requireRecoveryWarning(t, task)

	// The durable admission barrier fires before the actual native adapter can
	// register or write a physical HTTP request.
	exhausted := 0
	hooks := nativeTaskHooks(provider, &exhausted)
	hooks.admission = func(ctx context.Context) error { return server.ensureNativeTaskAdmission(ctx, task.ID) }
	_, _, _, dispatchErr := completeTaskLLM(subscription.WithTaskScope(t.Context(), task.ID), task.ID, llm.CompletionRequest{}, hooks)
	requireNativeRecoveryError(t, dispatchErr)
	if hits.Load() != 0 {
		t.Fatalf("periodic recovery dispatched %d physical native HTTP requests", hits.Load())
	}

	before := task.lifecycleSnapshot().Status
	server.engine.settleTask(t.Context(), task, false)
	after := task.lifecycleSnapshot().Status
	if after != before || after == "timeout" || after == "done" {
		t.Fatalf("held timeout settlement changed status %q -> %q", before, after)
	}
}

type recoveryRequestProbe struct {
	calls int
	req   llm.CompletionRequest
}

func (p *recoveryRequestProbe) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	p.req = req
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("continued")}}, "end_turn", llm.Usage{}, nil
}

func (p *recoveryRequestProbe) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func TestNativeUsedWithoutHoldPreservesOpaqueToolContinuation(t *testing.T) {
	manager, server, task := newNativeRecoveryFixture(t)
	if err := manager.pg.MarkNativeTaskUsage(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	task.updateLifecycle(func(state *taskLifecycleState) {
		state.NativeUsed = true
	})
	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancelRun()
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelDrain()
		if err := server.engine.Wait(drainCtx); err != nil {
			t.Errorf("drain native continuation test: %v", err)
		}
	})
	for range 3 {
		server.engine.Run(runCtx, task)
		task.Notify()
		server.engine.sweepExpiredClaims(task)
	}
	lifecycle := task.lifecycleSnapshot()
	if server.engine.IsPaused(task.ID) || !lifecycle.NativeUsed || lifecycle.NativeRecoveryHold {
		t.Fatalf("healthy native-used task changed by Run, Notify, or an empty periodic sweep: %+v", lifecycle)
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	state, err := manager.pg.NativeTaskRecoveryState(t.Context(), taskID)
	if err != nil || !state.Used || state.Hold {
		t.Fatalf("healthy native-used recovery state = %+v / %v", state, err)
	}
	probe := &recoveryRequestProbe{}
	exhausted := 0
	hooks := nativeTaskHooks(probe, &exhausted)
	hooks.admission = func(ctx context.Context) error { return server.ensureNativeTaskAdmission(ctx, task.ID) }
	opaque := json.RawMessage(`{"response_id":"resp-1","encrypted_content":"opaque-json-must-survive"}`)
	req := llm.CompletionRequest{Messages: []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "call-1", Name: "lookup", Input: json.RawMessage(`{"q":"alpha"}`)}}, ProviderState: &llm.ProviderState{Provider: "openai-responses", Model: "gpt-5.6-sol", AuthScope: "credential:1", Data: opaque}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("call-1", "result", false)}},
	}}
	if _, _, _, err := completeTaskLLM(t.Context(), task.ID, req, hooks); err != nil {
		t.Fatal(err)
	}
	if probe.calls != 1 || len(probe.req.Messages) != 2 || probe.req.Messages[0].ProviderState == nil {
		t.Fatalf("healthy native continuation dispatch = calls %d, request %+v", probe.calls, probe.req)
	}
	if !bytes.Equal(probe.req.Messages[0].ProviderState.Data, opaque) {
		t.Fatalf("opaque provider state changed: got %s want %s", probe.req.Messages[0].ProviderState.Data, opaque)
	}
	encoded, err := json.Marshal(probe.req.Messages)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"opaque-json-must-survive", "call-1", "tool_result", "result"} {
		if !strings.Contains(string(encoded), required) {
			t.Fatalf("continuation lost %q: %s", required, encoded)
		}
	}
}

func TestDrainTaskRuntimesKeepsDatabaseOpenThroughFinalWorkerWrite(t *testing.T) {
	_, server, task := newNativeRecoveryFixture(t)
	runtime := server.engine.registerTaskRoutines(context.Background(), task.ID, 1)
	writeDone := make(chan error, 1)
	runTaskRoutine(runtime, func(ctx context.Context) {
		<-ctx.Done()
		_, err := task.Store.AppendActivity(db.Activity{Worker: "system", Kind: "text", Summary: "final worker persistence"})
		writeDone <- err
	})

	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.DrainTaskRuntimes(drainCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("final worker write while draining: %v", err)
	}
	activities, _, err := task.Store.ActivityList(nil, 0, 100)
	if err != nil {
		t.Fatalf("database closed before settlement: %v", err)
	}
	found := false
	for _, activity := range activities {
		found = found || activity.Summary == "final worker persistence"
	}
	if !found {
		t.Fatal("final worker persistence is missing after drain")
	}
}
