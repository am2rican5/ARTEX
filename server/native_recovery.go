package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/artex/subscription"
)

const nativeRecoveryWarning = "Automatic recovery paused this task because a native subscription request may have been sent before the previous execution stopped, and its outcome is unknown. Manual Resume is required before model calls can continue."

func nativeTaskRecoveryHeld(ctx context.Context, store *db.DB, taskID string) (bool, error) {
	if store == nil {
		return false, errors.New("native task recovery store is unavailable")
	}
	id, err := parseTaskID(taskID)
	if err != nil {
		return false, err
	}
	state, err := store.NativeTaskRecoveryState(ctx, id)
	if err != nil {
		return false, err
	}
	return state.Hold, nil
}

func nativeRecoveryBarrierSet(task *Task) bool {
	if task == nil {
		return false
	}
	task.lifecycleMu.RLock()
	held := task.NativeRecoveryHold
	task.lifecycleMu.RUnlock()
	return held
}

// ensureNativeTaskAdmission is the final task-scoped dispatch barrier. It reads
// the durable state on every physical attempt, so changing the selected profile
// cannot bypass a hold established for earlier native use.
func (s *Server) ensureNativeTaskAdmission(ctx context.Context, taskID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.m == nil {
		return subscription.TaskRecoveryError()
	}
	if task, ok := s.m.Task(taskID); ok && nativeRecoveryBarrierSet(task) {
		return subscription.TaskRecoveryError()
	}
	held, err := nativeTaskRecoveryHeld(ctx, s.m.pg, taskID)
	if err == nil && !held {
		return nil
	}
	if task, ok := s.m.Task(taskID); ok && s.engine != nil {
		if barrierErr := s.engine.applyNativeRecoveryBarrier(task, err == nil && held); barrierErr != nil {
			log.Printf("[native-recovery] task %s warning persistence failed: %v", taskID, barrierErr)
		}
	}
	if err != nil {
		log.Printf("[native-recovery] task %s admission check failed closed: %v", taskID, err)
	}
	return subscription.TaskRecoveryError()
}

// prepareNativeTaskRecovery establishes the restart barrier before reset,
// deadline coordination, or Engine.Run can make progress. A failure to read,
// commit, or expose the hold leaves an in-memory pause in place and is returned
// to the startup caller, which must skip automatic execution for this task.
func (s *Server) prepareNativeTaskRecovery(h *Task) (bool, error) {
	if h == nil {
		return true, errors.New("native task recovery requires a task")
	}
	if s == nil || s.m == nil || s.m.pg == nil || s.engine == nil {
		if s != nil && s.engine != nil {
			_ = s.engine.applyNativeRecoveryBarrier(h, false)
		}
		return true, errors.New("native task recovery dependencies are unavailable")
	}
	id, err := parseTaskID(h.ID)
	if err != nil {
		_ = s.engine.applyNativeRecoveryBarrier(h, false)
		return true, err
	}
	recoveryCtx := s.ctx
	if recoveryCtx == nil {
		recoveryCtx = context.Background()
	}
	if recoveryCtx.Err() != nil {
		s.engine.draining.Store(true)
		return true, nil
	}
	state, err := s.m.pg.NativeTaskRecoveryState(recoveryCtx, id)
	if recoveryCtx.Err() != nil {
		s.engine.draining.Store(true)
		return true, nil
	}
	if err != nil {
		_ = s.engine.applyNativeRecoveryBarrier(h, false)
		return true, fmt.Errorf("read native recovery state: %w", err)
	}
	if !state.Used && !state.Hold {
		return false, nil
	}
	held, err := s.m.pg.HoldNativeTaskRecovery(recoveryCtx, id)
	if recoveryCtx.Err() != nil {
		s.engine.draining.Store(true)
		return true, nil
	}
	if err != nil {
		_ = s.engine.applyNativeRecoveryBarrier(h, false)
		return true, fmt.Errorf("commit native recovery hold: %w", err)
	}
	if !held {
		return false, nil
	}
	if recoveryCtx.Err() != nil {
		s.engine.draining.Store(true)
		return true, nil
	}
	if err := s.engine.applyNativeRecoveryBarrier(h, true); err != nil {
		return true, fmt.Errorf("persist native recovery warning: %w", err)
	}
	return true, nil
}

func (e *Engine) applyNativeRecoveryBarrier(t *Task, persistWarning bool) error {
	if t == nil {
		return errors.New("native recovery barrier requires a task")
	}
	t.lifecycleMu.Lock()
	t.Paused = true
	t.NativeRecoveryHold = true
	t.lifecycleMu.Unlock()
	e.Pause(t.ID, subscription.TaskRecoveryError())
	if !persistWarning {
		return nil
	}
	activity := db.Activity{
		Worker:  "system",
		Kind:    "text",
		IsError: true,
		Summary: nativeRecoveryWarning,
		Detail:  nativeRecoveryWarning,
	}
	id, err := e.appendActivity(t, activity)
	if err != nil {
		return err
	}
	activity.ID = id
	activity.CreatedAt = time.Now()
	if e.bc != nil {
		e.bc.Publish(t.ID, activity)
	}
	e.touch(t.ID)
	return nil
}

// holdNativeTaskAfterTerminalError converts an in-process native terminal
// failure into the same durable, user-acknowledged recovery barrier used after
// restart. The in-memory barrier is reserved before persistence so concurrent
// planner/worker paths cannot race another model call or duplicate the warning.
// Wrapped errors retain the terminal marker through subscription.IsTerminal.
func (e *Engine) holdNativeTaskAfterTerminalError(ctx context.Context, task *Task, callErr error) bool {
	if !subscription.IsTerminal(callErr) {
		return false
	}
	if e == nil || e.m == nil || e.m.pg == nil || task == nil {
		return true
	}

	task.lifecycleMu.Lock()
	if task.NativeRecoveryHold || isTerminalStatus(task.Status) {
		task.lifecycleMu.Unlock()
		return true
	}
	wasPaused := task.Paused
	task.NativeRecoveryHold = true
	task.Paused = true
	task.lifecycleMu.Unlock()
	e.Pause(task.ID, subscription.TaskRecoveryError())

	if ctx == nil {
		ctx = context.Background()
	}
	persistCtx := context.WithoutCancel(ctx)
	if err := e.m.pg.MarkNativeTaskUsage(persistCtx, task.ID); err != nil {
		if errors.Is(err, db.ErrNativeRecoveryHeld) {
			return true
		}
		if warningErr := e.applyNativeRecoveryBarrier(task, true); warningErr != nil {
			log.Printf("[native-recovery] task %s terminal native warning persistence failed: %v", task.ID, warningErr)
		}
		log.Printf("[native-recovery] task %s terminal native usage checkpoint failed closed: %v", task.ID, err)
		return true
	}
	task.lifecycleMu.Lock()
	task.NativeUsed = true
	task.lifecycleMu.Unlock()

	taskID, err := parseTaskID(task.ID)
	if err != nil {
		if warningErr := e.applyNativeRecoveryBarrier(task, true); warningErr != nil {
			log.Printf("[native-recovery] task %s terminal native warning persistence failed: %v", task.ID, warningErr)
		}
		log.Printf("[native-recovery] task %s terminal native hold failed closed: %v", task.ID, err)
		return true
	}
	held, err := e.m.pg.HoldNativeTaskRecovery(persistCtx, taskID)
	if err != nil {
		if warningErr := e.applyNativeRecoveryBarrier(task, true); warningErr != nil {
			log.Printf("[native-recovery] task %s terminal native warning persistence failed: %v", task.ID, warningErr)
		}
		log.Printf("[native-recovery] task %s terminal native hold persistence failed closed: %v", task.ID, err)
		return true
	}
	if !held {
		// HoldNativeTaskRecovery returns false only when the task became terminal
		// (usage was just committed above). Do not turn that terminal task back
		// into a resumable recovery task or leave our reservation mirrored.
		task.lifecycleMu.Lock()
		task.NativeRecoveryHold = false
		task.Paused = wasPaused
		task.lifecycleMu.Unlock()
		if !wasPaused {
			e.paused.Delete(task.ID)
		}
		return true
	}
	if warningErr := e.applyNativeRecoveryBarrier(task, true); warningErr != nil {
		log.Printf("[native-recovery] task %s terminal native warning persistence failed: %v", task.ID, warningErr)
	}
	return true
}

// guardNativeRecovery prevents timeout/budget settlement from issuing a final
// model request or terminalizing a held task. Store errors fail closed as an
// in-process pause; explicit Resume remains the only reopening path.
func (e *Engine) guardNativeRecovery(ctx context.Context, t *Task) error {
	if e == nil || e.m == nil || t == nil {
		return subscription.TaskRecoveryError()
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if nativeRecoveryBarrierSet(t) {
		e.settling.Delete(t.ID)
		_ = e.applyNativeRecoveryBarrier(t, false)
		return subscription.TaskRecoveryError()
	}
	held, err := nativeTaskRecoveryHeld(ctx, e.m.pg, t.ID)
	if err == nil && !held {
		return nil
	}
	e.settling.Delete(t.ID)
	_ = e.applyNativeRecoveryBarrier(t, false)
	if err != nil {
		log.Printf("[native-recovery] task %s settlement check failed closed: %v", t.ID, err)
	}
	return subscription.TaskRecoveryError()
}

type nativeDrainResult struct {
	name string
	err  error
}

// DrainTaskRuntimes cancels model admission first, then joins task execution,
// main-agent/conversation turns, side-question final persistence, and the archive
// worker while PostgreSQL remains open. A caller deadline reports the components
// that are still live; it does not claim their external work was committed.
func (s *Server) DrainTaskRuntimes(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil {
		return nil
	}
	if s.m != nil {
		s.m.BeginNativeShutdown()
	}

	components := make(map[string]struct{}, 3)
	results := make(chan nativeDrainResult, 3)
	start := func(name string, wait func() error) {
		components[name] = struct{}{}
		go func() { results <- nativeDrainResult{name: name, err: wait()} }()
	}

	start("task runtimes and archive worker", func() error { return s.Wait(ctx) })

	chatDone := s.beginChatDrain()
	start("main-agent runtimes", func() error { <-chatDone; return nil })

	if s.side != nil {
		sideDone := s.beginSideQuestionDrain()
		start("side-question persistence", func() error { return <-sideDone })
	}

	var joined []string
	var drainErrs []error
	record := func(result nativeDrainResult) {
		if _, pending := components[result.name]; !pending {
			return
		}
		delete(components, result.name)
		joined = append(joined, result.name)
		if result.err != nil {
			drainErrs = append(drainErrs, fmt.Errorf("%s: %w", result.name, result.err))
		}
	}
	for len(components) > 0 {
		select {
		case result := <-results:
			record(result)
		case <-ctx.Done():
		drainReady:
			for {
				select {
				case result := <-results:
					record(result)
				default:
					break drainReady
				}
			}
			left := make([]string, 0, len(components))
			for name := range components {
				left = append(left, name)
			}
			sort.Strings(joined)
			sort.Strings(left)
			log.Printf("[shutdown] runtime drain deadline reached; joined=%s leftovers=%s", strings.Join(joined, ", "), strings.Join(left, ", "))
			deadlineErr := fmt.Errorf("runtime drain deadline: %w (joined: %s; leftovers: %s)", ctx.Err(), strings.Join(joined, ", "), strings.Join(left, ", "))
			return errors.Join(deadlineErr, errors.Join(drainErrs...))
		}
	}
	sort.Strings(joined)
	if err := errors.Join(drainErrs...); err != nil {
		log.Printf("[shutdown] runtime joins finished with persistence errors; joined=%s error=%v", strings.Join(joined, ", "), err)
		return err
	}
	log.Printf("[shutdown] runtime drain complete; joined=%s", strings.Join(joined, ", "))
	return nil
}

func (s *Server) beginChatDrain() <-chan struct{} {
	done := make(chan struct{})
	s.chatMu.Lock()
	for _, cancel := range s.chatCancel {
		cancel(agent.AbortShutdown)
	}
	s.chatMu.Unlock()
	s.queueMu.Lock()
	for key := range s.triggerQ {
		delete(s.triggerQ, key)
	}
	s.queueMu.Unlock()
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			s.chatMu.Lock()
			chatCount := len(s.chatBusy)
			s.chatMu.Unlock()
			s.queueMu.Lock()
			activeCount := 0
			for _, count := range s.triggerActive {
				activeCount += count
			}
			s.queueMu.Unlock()
			if chatCount == 0 && activeCount == 0 {
				return
			}
			<-ticker.C
		}
	}()
	return done
}

func (s *Server) beginSideQuestionDrain() <-chan error {
	done := make(chan error, 1)
	s.side.commands.Lock()
	runs := s.cancelSideWhere(func(_ sidequestion.Parent) bool { return true })
	s.side.commands.Unlock()
	go func() {
		for _, runDone := range runs {
			<-runDone
		}
		if s.side.done != nil {
			<-s.side.done
		}
		s.flushSideSnapshots()
		s.side.mu.Lock()
		pending := len(s.side.pending)
		s.side.mu.Unlock()
		if pending != 0 {
			done <- fmt.Errorf("%d side-question checkpoints remain unsaved", pending)
			return
		}
		done <- nil
	}()
	return done
}
