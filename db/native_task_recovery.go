package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ErrNativeRecoveryHeld means a task cannot dispatch or be admitted until an
// explicit resume acknowledges its uncertain native outcome.
var ErrNativeRecoveryHeld = errors.New("native task recovery acknowledgement required")

// NativeTaskRecovery is the durable native-use and recovery-hold state of a
// task. Used alone is not a live hold; startup or expired-claim recovery sets
// Hold when unfinished native work may have an unobserved outcome.
type NativeTaskRecovery struct {
	Used bool
	Hold bool
}

// TaskAdmissionInput is the lifecycle compare-and-set performed by the task
// scheduler. AcknowledgeNativeRecovery is true only for an explicit user resume.
type TaskAdmissionInput struct {
	ExpectedStatus            string
	Status                    string
	Queued                    bool
	Mode                      string
	PreservePosition          bool
	AcknowledgeNativeRecovery bool
}

// TaskAdmissionResult is the lifecycle state committed by ApplyTaskAdmission.
type TaskAdmissionResult struct {
	QueuedAt    *time.Time
	QueueMode   string
	CompletedAt *time.Time
	FirstRunAt  *time.Time
	DeadlineAt  *time.Time
}

func parseTaskRegistryID(raw string) (int64, error) {
	if raw == "" || raw[0] == '0' {
		return 0, errors.New("task registry id must be a positive decimal")
	}
	for i := range len(raw) {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, fmt.Errorf("task registry id %q must be a positive decimal", raw)
		}
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("task registry id %q must be a positive decimal", raw)
	}
	return id, nil
}

// MarkNativeTaskUsage durably marks a registry task before task-scoped native
// HTTP dispatch. A held task is never marked-and-dispatched again.
func (d *DB) MarkNativeTaskUsage(ctx context.Context, taskID string) error {
	id, err := parseTaskRegistryID(taskID)
	if err != nil {
		return err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var held bool
	if err := tx.QueryRowContext(ctx, `SELECT native_recovery_hold
FROM tasks WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&held); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("task registry id %d does not exist", id)
		}
		return err
	}
	if held {
		return ErrNativeRecoveryHeld
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET native_used=true
WHERE id=$1 AND native_recovery_hold=false`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// NativeTaskRecoveryState reads the dedicated recovery fields without deriving
// task identity from exploration or node ids.
func (d *DB) NativeTaskRecoveryState(ctx context.Context, taskID int64) (NativeTaskRecovery, error) {
	var state NativeTaskRecovery
	if taskID <= 0 {
		return state, errors.New("task registry id must be positive")
	}
	err := d.QueryRowContext(ctx, `SELECT native_used, native_recovery_hold
FROM tasks WHERE id=$1 AND deleted_at IS NULL`, taskID).Scan(&state.Used, &state.Hold)
	if errors.Is(err, sql.ErrNoRows) {
		return state, fmt.Errorf("task registry id %d does not exist", taskID)
	}
	return state, err
}

// HoldNativeTaskRecovery establishes the durable pause barrier for unfinished
// marked work. It is idempotent for an existing hold. Terminal marked tasks are
// not converted into held resumable tasks, but an existing terminal hold remains
// visible until explicit acknowledgement.
func (d *DB) HoldNativeTaskRecovery(ctx context.Context, taskID int64) (bool, error) {
	if taskID <= 0 {
		return false, errors.New("task registry id must be positive")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var used, held bool
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT native_used, native_recovery_hold, status
FROM tasks WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, taskID).Scan(&used, &held, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("task registry id %d does not exist", taskID)
		}
		return false, err
	}
	terminal := IsTerminal(status)
	if !held && (!used || terminal) {
		return false, nil
	}
	if !terminal {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks
SET native_recovery_hold=true,
    paused=true,
    queued=false,
    queued_at=NULL,
    paused_at=COALESCE(paused_at, now())
WHERE id=$1`, taskID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ApplyTaskAdmission atomically commits scheduler lifecycle state. Held tasks
// reject ordinary admission. Explicit acknowledgement clears both dedicated
// recovery bits and reopens only intents blocked by uncertain native outcome in
// this same transaction.
func (d *DB) ApplyTaskAdmission(ctx context.Context, taskID int64, input TaskAdmissionInput) (TaskAdmissionResult, error) {
	var result TaskAdmissionResult
	if taskID <= 0 {
		return result, errors.New("task registry id must be positive")
	}
	if input.Queued {
		if input.Mode != "bootstrap" && input.Mode != "resume" {
			return result, fmt.Errorf("invalid queue mode %q", input.Mode)
		}
	} else {
		input.Mode = ""
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	var currentStatus string
	var explorationID int64
	var held bool
	err = tx.QueryRowContext(ctx, `SELECT status, exploration_id, native_recovery_hold
FROM tasks WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, taskID).Scan(&currentStatus, &explorationID, &held)
	if errors.Is(err, sql.ErrNoRows) || err == nil && currentStatus != input.ExpectedStatus {
		return result, fmt.Errorf("task %d lifecycle changed before admission (expected status %q)", taskID, input.ExpectedStatus)
	}
	if err != nil {
		return result, err
	}
	if held && !input.AcknowledgeNativeRecovery {
		return result, ErrNativeRecoveryHeld
	}

	var queuedAt, completedAt, firstRunAt, deadlineAt sql.NullTime
	err = tx.QueryRowContext(ctx, `UPDATE tasks
SET status=$2,
    completed_at=CASE
        WHEN $2 IN ('done','failed','timeout') THEN COALESCE(completed_at, now())
        ELSE NULL
    END,
    paused=false,
    queued=$3,
    queued_at=CASE
        WHEN NOT $3 THEN NULL
        WHEN $5 AND queued THEN COALESCE(queued_at, now())
        ELSE now()
    END,
    queue_mode=CASE
        WHEN NOT $3 THEN ''
        WHEN ($5 AND queued AND queue_mode='bootstrap') OR $4='bootstrap' THEN 'bootstrap'
        ELSE 'resume'
    END,
    first_run_at=CASE
        WHEN $6='timeout' AND $2 NOT IN ('done','failed','timeout') THEN NULL
        ELSE first_run_at
    END,
    deadline_at=CASE
        WHEN $6='timeout' AND $2 NOT IN ('done','failed','timeout') THEN NULL
        WHEN paused_at IS NOT NULL AND deadline_at IS NOT NULL THEN deadline_at + (now() - paused_at)
        ELSE deadline_at
    END,
    paused_at=NULL,
    end_reason=CASE
        WHEN $2 NOT IN ('done','failed','timeout') THEN ''
        ELSE end_reason
    END,
    native_used=CASE WHEN $7 THEN false ELSE native_used END,
    native_recovery_hold=CASE WHEN $7 THEN false ELSE native_recovery_hold END
WHERE id=$1 AND deleted_at IS NULL AND status=$6
RETURNING queued_at, queue_mode, completed_at, first_run_at, deadline_at`,
		taskID, input.Status, input.Queued, input.Mode, input.PreservePosition, input.ExpectedStatus, input.AcknowledgeNativeRecovery).
		Scan(&queuedAt, &result.QueueMode, &completedAt, &firstRunAt, &deadlineAt)
	if errors.Is(err, sql.ErrNoRows) {
		return result, fmt.Errorf("task %d lifecycle changed before admission (expected status %q)", taskID, input.ExpectedStatus)
	}
	if err != nil {
		return result, err
	}

	if input.AcknowledgeNativeRecovery {
		if _, err := tx.ExecContext(ctx, `UPDATE exploration_nodes
SET state='open', completed_at=NULL, blocked_reason=NULL
WHERE exploration_id=$1 AND kind='intent' AND state='blocked'
  AND blocked_reason='native_outcome_unknown'`, explorationID); err != nil {
			return result, err
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	result.QueuedAt = nullTimePointer(queuedAt)
	result.CompletedAt = nullTimePointer(completedAt)
	result.FirstRunAt = nullTimePointer(firstRunAt)
	result.DeadlineAt = nullTimePointer(deadlineAt)
	return result, nil
}

func nullTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time
	return &result
}
