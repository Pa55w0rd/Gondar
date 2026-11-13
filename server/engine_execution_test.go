package server

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/harness"
	"gondar/agent"
	"gondar/db"
)

func TestExecutionEvidenceUsesHostInvocationAndKeepsUnknown(t *testing.T) {
	x := &executionEvidence{runID: "owned"}
	activity := func(invocation, phase, state string, use bool) db.Activity {
		id := agent.ToolCallIdentity{Version: 1, RunID: "owned", PromptID: "prompt", InvocationID: invocation, ToolUseID: "same-provider-id"}
		meta := map[string]any{"tool_call_identity": id}
		kind := "tool_use"
		if !use {
			kind = "tool_result"
			// Marshal plain trusted event values, without relying on target text.
			meta["tool_outcome"] = map[string]any{"version": 1, "run_id": "owned", "prompt_id": "prompt", "invocation_id": invocation, "tool_use_id": "same-provider-id", "phase": phase, "execution_state": state}
		}
		raw, _ := json.Marshal(meta)
		return db.Activity{Kind: kind, ToolUseID: id.ToolUseID, Metadata: raw, IsError: state == "unknown"}
	}
	x.observe(activity("one", "", "", true))
	x.observe(activity("two", "", "", true))
	x.observe(activity("one", "result", "executed", false))
	if !x.uncertain() {
		t.Fatal("one receipt cleared another concurrent invocation")
	}
	x.observe(activity("two", "result", "not_executed", false))
	if x.uncertain() {
		t.Fatal("both exact receipts were ignored")
	}
	x.observe(activity("three", "", "", true))
	x.observe(activity("three", "result", "unknown", false))
	x.observe(activity("three", "late_receipt", "executed", false))
	if !x.uncertain() {
		t.Fatal("late receipt rewrote an interrupted decision")
	}
}

func TestToolLoopStopCannotBecomeWholeRunModelRetry(t *testing.T) {
	loop := errors.Join(agent.ErrToolFailureLoop, context.Canceled)
	if retryableWorkerModelError(harness.ReasonModelError, loop) || isTransientContextCanceled(loop) {
		t.Fatal("host loop protection retried as transient model failure")
	}
	if !isTransientContextCanceled(context.Canceled) {
		t.Fatal("unrelated transient cancellation policy changed")
	}
}

func TestClosingWorkRetainsOwnershipUntilReceiptAndRejectsNewControl(t *testing.T) {
	e := NewEngine(nil)
	ctx, cancel := context.WithCancelCause(t.Context())
	e.registerWork(42, cancel)
	_, complete := e.detachWork(42)
	e.workMu.Lock()
	retained := e.work[42] != nil && e.work[42].closing
	e.workMu.Unlock()
	if !retained || ctx.Err() == nil {
		t.Fatal("closing work lost ownership or leaked execution context")
	}
	if err := e.ControlWork(t.Context(), 42, "pause"); !errors.Is(err, errWorkControlConflict) {
		t.Fatalf("closing control=%v", err)
	}
	complete(nil)
	e.workMu.Lock()
	gone := e.work[42] == nil
	e.workMu.Unlock()
	if !gone {
		t.Fatal("committed work handle retained")
	}
}

func TestExecutionStorageGateCoversDetachedAndPoolClaims(t *testing.T) {
	e := NewEngine(nil)
	flag := &atomic.Int64{}
	flag.Store(1)
	e.executionSaving.Store("synthetic", flag)
	task := &Task{ID: "synthetic"}
	if n := e.claimNext(t.Context(), task, "worker"); n != nil {
		t.Fatal("pool bypassed storage gate")
	}
	if err := e.runDetachedIntent(t.Context(), task, 12, "request", "message", "message"); !errors.Is(err, db.ErrIntentStateConflict) {
		t.Fatalf("detached bypassed storage gate: %v", err)
	}
}

func TestExecutionStorageRetryPreservesHumanStateAndDoesNotReplay(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	task, err := m.CreateTask("storage fixture", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	t.Cleanup(func() { m.pg.DeleteTask(taskID) })
	iid, err := task.Store.AddIntent(map[string]any{"summary": "synthetic"}, 5, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(m)
	var calls atomic.Int64
	saved, err := e.persistExecution(t.Context(), task, iid, func(ctx context.Context) (bool, error) {
		n := calls.Add(1)
		if n < 3 {
			return false, errors.New("synthetic database unavailable")
		}
		if !e.executionStoragePending(task.ID) || e.IsPaused(task.ID) {
			t.Error("storage wait confused with manual pause")
		}
		return true, nil
	})
	if err != nil || !saved || calls.Load() != 3 || e.executionStoragePending(task.ID) {
		t.Fatalf("storage retry saved=%v calls=%d err=%v", saved, calls.Load(), err)
	}
}

func TestExecutionStorageFailureHonorsShutdownWithoutFalseAcknowledgement(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	task, err := m.CreateTask("storage shutdown fixture", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	t.Cleanup(func() { m.pg.DeleteTask(taskID) })
	iid, _ := task.Store.AddIntent(map[string]any{"summary": "synthetic"}, 5, nil, "test")
	e := NewEngine(m)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		saved, err := e.persistExecution(ctx, task, iid, func(context.Context) (bool, error) { return false, errors.New("synthetic persistent storage failure") })
		if saved {
			err = errors.New("false storage acknowledgement")
		}
		done <- err
	}()
	deadline := time.After(10 * time.Second)
	for !e.executionStoragePending(task.ID) {
		select {
		case <-deadline:
			t.Fatal("storage gate not installed")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || strings.Contains(err.Error(), "false storage") {
			t.Fatalf("shutdown err=%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown blocked on storage retry")
	}
	if e.executionStoragePending(task.ID) || e.IsPaused(task.ID) {
		t.Fatal("shutdown leaked pending gate or manual pause")
	}
}
