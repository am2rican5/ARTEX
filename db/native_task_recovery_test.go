package db

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"
)

func openNativeRecoveryTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open configured PostgreSQL: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func createNativeRecoveryTestTask(t *testing.T, d *DB) *Task {
	t.Helper()
	task, err := d.CreateTask("native recovery persistence", "preserve uncertain native outcomes", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.DeleteTask(task.ID); err != nil {
			t.Errorf("delete recovery task %d: %v", task.ID, err)
		}
	})
	return task
}

func TestNativeTaskUsagePersistsAcrossTaskProfileMutation(t *testing.T) {
	d := openNativeRecoveryTestDB(t)
	task := createNativeRecoveryTestTask(t, d)

	for _, invalid := range []string{"", "0", "01", "-1", "+1", " 1", "1 ", "1x", "9223372036854775808"} {
		if err := d.MarkNativeTaskUsage(t.Context(), invalid); err == nil {
			t.Errorf("MarkNativeTaskUsage(%q) succeeded", invalid)
		}
	}
	if err := d.MarkNativeTaskUsage(t.Context(), "9223372036854775807"); err == nil {
		t.Fatal("marking a missing registry task succeeded")
	}

	id := strconv.FormatInt(task.ID, 10)
	if err := d.MarkNativeTaskUsage(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := d.ReplaceTaskLLMProfiles(task.ID, nil, 0); err != nil {
		t.Fatalf("clear task profile chain: %v", err)
	}

	other, err := Open(testDSN(t))
	if err != nil {
		t.Fatalf("open second PostgreSQL handle: %v", err)
	}
	defer other.Close()
	state, err := other.NativeTaskRecoveryState(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Used || state.Hold {
		t.Fatalf("recovery state after profile mutation = %+v, want used without hold", state)
	}
	loaded, err := other.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || !loaded.NativeUsed || loaded.NativeRecoveryHold {
		t.Fatalf("task recovery fields were not scanned: %+v", loaded)
	}
}

func TestNativeUsageAloneDoesNotBlockHealthyAdmission(t *testing.T) {
	d := openNativeRecoveryTestDB(t)
	task := createNativeRecoveryTestTask(t, d)
	if err := d.MarkNativeTaskUsage(t.Context(), strconv.FormatInt(task.ID, 10)); err != nil {
		t.Fatal(err)
	}

	_, err := d.ApplyTaskAdmission(t.Context(), task.ID, TaskAdmissionInput{
		ExpectedStatus: "created",
		Status:         "running",
	})
	if err != nil {
		t.Fatalf("healthy marked task admission: %v", err)
	}
	state, err := d.NativeTaskRecoveryState(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Used || state.Hold {
		t.Fatalf("healthy admission changed marker or invented a hold: %+v", state)
	}
	loaded, err := d.GetTask(task.ID)
	if err != nil || loaded == nil || loaded.Status != "running" || loaded.Paused {
		t.Fatalf("healthy admission lifecycle = %+v err=%v", loaded, err)
	}
}

func TestNativeRecoveryExpiredNormalAndLiveClaims(t *testing.T) {
	d := openNativeRecoveryTestDB(t)

	nativeTask := createNativeRecoveryTestTask(t, d)
	nativeStore := d.Exploration(nativeTask.ExplorationID)
	nativeIntent := addClaimIntent(t, nativeStore)
	claimIntentToken(t, nativeStore, nativeIntent, "native-expired")
	if err := d.MarkNativeTaskUsage(t.Context(), strconv.FormatInt(nativeTask.ID, 10)); err != nil {
		t.Fatal(err)
	}
	ageClaimHeartbeat(t, d, nativeIntent, IntentClaimTTL+time.Second)
	count, err := nativeStore.ResetRunningIntents()
	if count != 1 || !errors.Is(err, ErrNativeRecoveryHeld) {
		t.Fatalf("native reset = (%d, %v), want (1, ErrNativeRecoveryHeld)", count, err)
	}
	nativeRow := readClaimRow(t, d, nativeIntent)
	if nativeRow.state != "blocked" || nativeRow.blockedReason.String != "native_outcome_unknown" || nativeRow.token.Valid {
		t.Fatalf("expired native claim = %+v", nativeRow)
	}
	var completed bool
	if err := d.QueryRow(`SELECT completed_at IS NOT NULL FROM exploration_nodes WHERE id=$1`, nativeIntent).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatal("expired native claim did not stamp completion")
	}
	nativeLoaded, err := d.GetTask(nativeTask.ID)
	if err != nil || nativeLoaded == nil || !nativeLoaded.NativeRecoveryHold || !nativeLoaded.Paused {
		t.Fatalf("native task hold = %+v err=%v", nativeLoaded, err)
	}

	normalTask := createNativeRecoveryTestTask(t, d)
	normalStore := d.Exploration(normalTask.ExplorationID)
	normalIntent := addClaimIntent(t, normalStore)
	claimIntentToken(t, normalStore, normalIntent, "normal-expired")
	ageClaimHeartbeat(t, d, normalIntent, IntentClaimTTL+time.Second)
	count, err = normalStore.ResetRunningIntents()
	if err != nil || count != 1 {
		t.Fatalf("normal reset = (%d, %v), want (1, nil)", count, err)
	}
	normalRow := readClaimRow(t, d, normalIntent)
	if normalRow.state != "open" || normalRow.blockedReason.Valid || normalRow.token.Valid {
		t.Fatalf("expired normal claim = %+v", normalRow)
	}
	normalState, err := d.NativeTaskRecoveryState(t.Context(), normalTask.ID)
	if err != nil || normalState != (NativeTaskRecovery{}) {
		t.Fatalf("normal task recovery state = %+v err=%v", normalState, err)
	}

	liveTask := createNativeRecoveryTestTask(t, d)
	liveStore := d.Exploration(liveTask.ExplorationID)
	liveIntent := addClaimIntent(t, liveStore)
	liveToken := claimIntentToken(t, liveStore, liveIntent, "native-live")
	if err := d.MarkNativeTaskUsage(t.Context(), strconv.FormatInt(liveTask.ID, 10)); err != nil {
		t.Fatal(err)
	}
	count, err = liveStore.ResetRunningIntents()
	if err != nil || count != 0 {
		t.Fatalf("live reset = (%d, %v), want (0, nil)", count, err)
	}
	liveRow := readClaimRow(t, d, liveIntent)
	if liveRow.state != "running" || liveRow.token.String != liveToken || liveRow.blockedReason.Valid {
		t.Fatalf("live native claim changed: %+v", liveRow)
	}
	liveLoaded, err := d.GetTask(liveTask.ID)
	if err != nil || liveLoaded == nil || !liveLoaded.NativeUsed || liveLoaded.NativeRecoveryHold || liveLoaded.Paused {
		t.Fatalf("live native task invented a hold: %+v err=%v", liveLoaded, err)
	}
}

func TestNativeRecoveryAdmissionAcknowledgementIsAtomicAndSpecific(t *testing.T) {
	d := openNativeRecoveryTestDB(t)
	task := createNativeRecoveryTestTask(t, d)
	store := d.Exploration(task.ExplorationID)

	if err := d.MarkNativeTaskUsage(t.Context(), strconv.FormatInt(task.ID, 10)); err != nil {
		t.Fatal(err)
	}
	held, err := d.HoldNativeTaskRecovery(t.Context(), task.ID)
	if err != nil || !held {
		t.Fatalf("HoldNativeTaskRecovery = (%t, %v)", held, err)
	}
	held, err = d.HoldNativeTaskRecovery(t.Context(), task.ID)
	if err != nil || !held {
		t.Fatalf("idempotent HoldNativeTaskRecovery = (%t, %v)", held, err)
	}
	if err := d.MarkNativeTaskUsage(t.Context(), strconv.FormatInt(task.ID, 10)); !errors.Is(err, ErrNativeRecoveryHeld) {
		t.Fatalf("mark while held = %v, want ErrNativeRecoveryHeld", err)
	}

	nativeIntent, err := store.AddNode("intent", map[string]any{"summary": "native outcome unknown"}, 1, "blocked", "system", nil)
	if err != nil {
		t.Fatal(err)
	}
	otherIntent, err := store.AddNode("intent", map[string]any{"summary": "other block"}, 1, "blocked", "system", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE exploration_nodes
SET blocked_reason=CASE id WHEN $1 THEN 'native_outcome_unknown' ELSE 'provider_unavailable' END,
    completed_at=now()
WHERE id IN ($1,$2)`, nativeIntent, otherIntent); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.ReopenIntent(nativeIntent)
	if err != nil || reopened {
		t.Fatalf("generic rerun of uncertain native intent = (%t, %v)", reopened, err)
	}

	ack := TaskAdmissionInput{
		ExpectedStatus:            "running",
		Status:                    "running",
		AcknowledgeNativeRecovery: true,
	}
	if _, err := d.ApplyTaskAdmission(t.Context(), task.ID, ack); err == nil {
		t.Fatal("acknowledgement with failed lifecycle CAS succeeded")
	}
	assertRecoveryAndIntentState(t, d, task.ID, nativeIntent, true, true, "blocked", "native_outcome_unknown")

	_, err = d.ApplyTaskAdmission(t.Context(), task.ID, TaskAdmissionInput{
		ExpectedStatus: "created",
		Status:         "running",
	})
	if !errors.Is(err, ErrNativeRecoveryHeld) {
		t.Fatalf("non-acknowledging admission = %v, want ErrNativeRecoveryHeld", err)
	}
	assertRecoveryAndIntentState(t, d, task.ID, nativeIntent, true, true, "blocked", "native_outcome_unknown")

	ack.ExpectedStatus = "created"
	if _, err := d.ApplyTaskAdmission(t.Context(), task.ID, ack); err != nil {
		t.Fatalf("acknowledging admission: %v", err)
	}
	assertRecoveryAndIntentState(t, d, task.ID, nativeIntent, false, false, "open", "")

	var otherState string
	var otherReason sql.NullString
	var otherCompleted bool
	if err := d.QueryRow(`SELECT state, blocked_reason, completed_at IS NOT NULL
FROM exploration_nodes WHERE id=$1`, otherIntent).Scan(&otherState, &otherReason, &otherCompleted); err != nil {
		t.Fatal(err)
	}
	if otherState != "blocked" || otherReason.String != "provider_unavailable" || !otherCompleted {
		t.Fatalf("unrelated blocked intent changed: state=%q reason=%q completed=%t", otherState, otherReason.String, otherCompleted)
	}
	loaded, err := d.GetTask(task.ID)
	if err != nil || loaded == nil || loaded.Status != "running" || loaded.Paused || loaded.NativeUsed || loaded.NativeRecoveryHold {
		t.Fatalf("acknowledged task lifecycle = %+v err=%v", loaded, err)
	}
}

func TestFailedNativeUsageMarkLeavesTaskUnmarked(t *testing.T) {
	d := openNativeRecoveryTestDB(t)
	task := createNativeRecoveryTestTask(t, d)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := d.MarkNativeTaskUsage(ctx, strconv.FormatInt(task.ID, 10)); err == nil {
		t.Fatal("mark with cancelled context succeeded")
	}
	state, err := d.NativeTaskRecoveryState(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state != (NativeTaskRecovery{}) {
		t.Fatalf("failed mark changed task state: %+v", state)
	}
}

func assertRecoveryAndIntentState(t *testing.T, d *DB, taskID, intentID int64, used, held bool, nodeState, reason string) {
	t.Helper()
	state, err := d.NativeTaskRecoveryState(t.Context(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if state != (NativeTaskRecovery{Used: used, Hold: held}) {
		t.Fatalf("recovery state = %+v, want used=%t hold=%t", state, used, held)
	}
	var actualState string
	var actualReason sql.NullString
	if err := d.QueryRow(`SELECT state, blocked_reason FROM exploration_nodes WHERE id=$1`, intentID).Scan(&actualState, &actualReason); err != nil {
		t.Fatal(err)
	}
	if actualState != nodeState || actualReason.String != reason || actualReason.Valid != (reason != "") {
		t.Fatalf("intent state = %q reason=%+v, want %q/%q", actualState, actualReason, nodeState, reason)
	}
}
