package subscription

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/db"
)

const codeTaskUsagePersistence = "subscription_task_usage_persistence_failed"

type taskScopeContextKey struct{}

// WithTaskScope attaches the owning task registry ID to native model requests.
// A blank ID is a no-op so optional nested annotations retain their parent task.
// The persistence layer validates every non-empty value as a positive decimal
// task ID.
func WithTaskScope(ctx context.Context, taskID string) context.Context {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return ctx
	}
	current, _ := ctx.Value(taskScopeContextKey{}).(string)
	if current == taskID {
		return ctx
	}
	return context.WithValue(ctx, taskScopeContextKey{}, taskID)
}

// TaskUsageRecorder durably records that a task is about to use a native model.
// It remains separate from Store so credential-only stores need no task support.
type TaskUsageRecorder interface {
	MarkNativeTaskUsage(context.Context, string) error
}

// TaskRecoveryError reports that an explicit task resume is required before
// another native model request may be sent.
func TaskRecoveryError() error {
	return &Error{
		Code:    CodeTaskRecoveryRequired,
		Message: "task recovery requires explicit resume before another native model request",
		Status:  http.StatusConflict,
	}
}

func taskScopeFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	taskID, _ := ctx.Value(taskScopeContextKey{}).(string)
	return strings.TrimSpace(taskID)
}

func recordTaskUsage(ctx context.Context, store Store) error {
	taskID := taskScopeFromContext(ctx)
	if taskID == "" {
		return nil
	}
	recorder, ok := store.(TaskUsageRecorder)
	if !ok {
		return newError(codeTaskUsagePersistence, "native task usage checkpoint is unavailable")
	}
	if err := recorder.MarkNativeTaskUsage(ctx, taskID); err != nil {
		if errors.Is(err, db.ErrNativeRecoveryHeld) {
			return TaskRecoveryError()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return newError(codeTaskUsagePersistence, "native task usage checkpoint failed")
	}
	return nil
}
