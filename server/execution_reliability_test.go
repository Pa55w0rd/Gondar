package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"gondar/db"
)

func TestRepeatTrackerAcceptsChangedOutputAndBackgroundWait(t *testing.T) {
	rt := newRepeatTracker()
	input := []byte(`{"operation":"status"}`)
	for i := 0; i < 12; i++ {
		if _, block := rt.observe("read_state", input); block {
			t.Fatal("changing observation was blocked")
		}
		rt.observeResult("read_state", input, []byte(fmt.Sprint(i)), false)
	}
	h := steerHooks{repeats: newRepeatTracker(), drain: func() (string, bool) { return "", false }}
	for i := 0; i < 6; i++ {
		if blocked, _, _ := h.PreToolUse(t.Context(), "TaskOutput", input); blocked {
			t.Fatal("legitimate background wait was blocked")
		}
	}
}

func TestHealthPersistenceCoalescesAndOrdersManualReset(t *testing.T) {
	first := make(chan struct{})
	release := make(chan struct{})
	stored := make(chan *db.LLMHealth, 4)
	var once sync.Once
	w := &healthWriter{store: func(ctx context.Context, _ int64, h *db.LLMHealth) error {
		once.Do(func() {
			close(first)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
		stored <- h
		return nil
	}}
	w.enqueue(1, &db.LLMHealth{Fails: 1})
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not start")
	}
	for i := 0; i < 100; i++ {
		w.enqueue(1, &db.LLMHealth{Fails: i + 2})
	}
	w.enqueue(1, nil)
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case h := <-stored:
			if i == 1 && h != nil {
				t.Fatal("reset was overwritten by old queued state")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("writer did not drain")
		}
	}
}

func TestHealthPersistencePendingSetIsBounded(t *testing.T) {
	w := &healthWriter{running: true}
	for i := 0; i < 1000; i++ {
		w.enqueue(int64(i), &db.LLMHealth{Fails: i})
	}
	if len(w.pending) != maxPendingHealthWrites || w.dropped != 1000-maxPendingHealthWrites {
		t.Fatalf("unbounded queue: %d dropped %d", len(w.pending), w.dropped)
	}
	if _, ok := w.pending[999]; !ok {
		t.Fatal("latest state was dropped")
	}
}

func TestContinuousMixedFailuresPauseWithActualSequence(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTaskWithOptions("mixed failures", "synthetic only", db.TaskCreateOptions{CompletionMode: db.CompletionContinuous})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	e := NewEngine(m)
	want := []string{"planner_no_progress", "planner_read_loop", "planner_no_progress"}
	for i, code := range want {
		e.recordPlannerOutcome(t.Context(), task, int64(i+1), code)
	}
	pt, err := m.pg.GetTask(id)
	if err != nil || !pt.Paused || pt.Status == "done" || pt.Status == "failed" {
		t.Fatalf("unrecoverable mixed stop: %+v %v", pt, err)
	}
	var raw []byte
	if err := m.pg.QueryRow(`SELECT metadata FROM activity WHERE exploration_id=$1 AND metadata ? 'continuous_pause' ORDER BY id DESC LIMIT 1`, task.ExpID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Pause struct {
			Reasons []string `json:"consecutive_reasons"`
		} `json:"continuous_pause"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(meta.Pause.Reasons) != fmt.Sprint(want) {
		t.Fatalf("failure causes lost: %v", meta.Pause.Reasons)
	}
}
