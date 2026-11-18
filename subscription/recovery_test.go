package subscription

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/internal/norma/llm"
)

type recoveryTestStore struct {
	*testStore
	mark func(context.Context, string) error
}

func (s *recoveryTestStore) MarkNativeTaskUsage(ctx context.Context, taskID string) error {
	return s.mark(ctx, taskID)
}

type recoveryTestTransport struct {
	calls  atomic.Int64
	before func(*http.Request)
}

func (transport *recoveryTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	if transport.before != nil {
		transport.before(request)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(codexTextSuccess("ok"))),
		Request:    request,
	}, nil
}

func newRecoveryTestModel(t *testing.T, recorder bool, mark func(context.Context, string) error) (llm.Provider, *recoveryTestTransport) {
	t.Helper()
	base := newTestStore()
	var store Store = base
	if recorder {
		store = &recoveryTestStore{testStore: base, mark: mark}
	}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	credentialID := base.seed(ProviderCodex, CredentialReady, TokenSet{
		AccessToken:  "synthetic-recovery-access",
		RefreshToken: "synthetic-recovery-refresh",
		ExpiresAt:    now.Add(time.Hour),
		AccountID:    "account-recovery",
		Subject:      "subject-recovery",
	})
	binding := Binding{ProfileID: 181, CredentialID: credentialID, Generation: 7}
	base.bind(binding.ProfileID, binding.CredentialID, binding.Generation)
	manager := NewManager(store, Options{
		Drivers: []OAuthDriver{&testDriver{kind: ProviderCodex}},
		Now:     func() time.Time { return now },
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	transport := &recoveryTestTransport{}
	provider, err := NewCodexProvider(manager, ProviderConfig{
		Binding:         binding,
		Model:           "gpt-5.6-sol",
		ThinkingType:    "enabled",
		ReasoningEffort: "high",
		HTTPClient:      &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	return provider, transport
}

type recoveryContextValueKey struct{}

func TestNativeTaskUsageCheckpointCommitsBeforeCodexDispatch(t *testing.T) {
	var marked atomic.Bool
	var markCalls atomic.Int64
	var markedTask atomic.Value
	var checkpointSawValue atomic.Bool
	provider, transport := newRecoveryTestModel(t, true, func(ctx context.Context, taskID string) error {
		markCalls.Add(1)
		markedTask.Store(taskID)
		checkpointSawValue.Store(ctx.Value(recoveryContextValueKey{}) == "preserved")
		marked.Store(true)
		return nil
	})
	var dispatchedBeforeCheckpoint atomic.Bool
	transport.before = func(*http.Request) {
		if !marked.Load() {
			dispatchedBeforeCheckpoint.Store(true)
		}
	}

	ctx := context.WithValue(context.Background(), recoveryContextValueKey{}, "preserved")
	ctx = WithTaskScope(WithTaskScope(ctx, " 42 "), "   ")
	ctx = context.WithoutCancel(ctx)
	message, _, _, err := provider.Complete(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hello")}})
	if err != nil || message.Text() != "ok" {
		t.Fatalf("Complete = text %q, error %v", message.Text(), err)
	}
	if markCalls.Load() != 1 || markedTask.Load() != "42" || !checkpointSawValue.Load() {
		t.Fatalf("checkpoint = calls %d task %v preserved-value %v", markCalls.Load(), markedTask.Load(), checkpointSawValue.Load())
	}
	if transport.calls.Load() != 1 || dispatchedBeforeCheckpoint.Load() {
		t.Fatalf("dispatch = calls %d before-checkpoint %v", transport.calls.Load(), dispatchedBeforeCheckpoint.Load())
	}
}

func TestNativeTaskUsageCheckpointFailurePreventsCodexDispatch(t *testing.T) {
	tests := []struct {
		name     string
		recorder bool
		mark     func(context.Context, string) error
	}{
		{name: "missing recorder"},
		{name: "failed checkpoint", recorder: true, mark: func(context.Context, string) error {
			return errors.New("database detail containing secret material")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, transport := newRecoveryTestModel(t, test.recorder, test.mark)
			_, _, _, err := provider.Complete(WithTaskScope(context.Background(), "42"), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hello")}})
			var terminal *Error
			if !errors.As(err, &terminal) || terminal.Code != codeTaskUsagePersistence || !IsTerminal(err) {
				t.Fatalf("Complete error = %#v, want terminal task checkpoint failure", err)
			}
			if strings.Contains(err.Error(), "database detail") || strings.Contains(err.Error(), "secret material") {
				t.Fatalf("checkpoint error leaked store detail: %v", err)
			}
			if transport.calls.Load() != 0 {
				t.Fatalf("physical requests = %d, want 0", transport.calls.Load())
			}
		})
	}
}

func TestNativeTaskRecoveryHoldIsTerminalBeforeCodexDispatch(t *testing.T) {
	provider, transport := newRecoveryTestModel(t, true, func(context.Context, string) error {
		return db.ErrNativeRecoveryHeld
	})
	_, _, _, err := provider.Complete(WithTaskScope(context.Background(), "42"), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hello")}})
	var terminal *Error
	if !errors.As(err, &terminal) || terminal.Code != CodeTaskRecoveryRequired || terminal.Status != http.StatusConflict || !IsTerminal(err) {
		t.Fatalf("Complete error = %#v, want terminal recovery hold", err)
	}
	if transport.calls.Load() != 0 {
		t.Fatalf("physical requests = %d, want 0", transport.calls.Load())
	}
}

func TestUnscopedCodexModelDoesNotRequireTaskRecorder(t *testing.T) {
	provider, transport := newRecoveryTestModel(t, false, nil)
	message, _, _, err := provider.Complete(context.Background(), llm.CompletionRequest{Messages: []llm.Message{llm.UserText("hello")}})
	if err != nil || message.Text() != "ok" {
		t.Fatalf("Complete = text %q, error %v", message.Text(), err)
	}
	if transport.calls.Load() != 1 {
		t.Fatalf("physical requests = %d, want 1", transport.calls.Load())
	}
}
