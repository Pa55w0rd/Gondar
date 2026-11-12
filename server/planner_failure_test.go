package server

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"

	"gondar/agent"
)

func TestPlannerFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		err     error
		timeout bool
		code    string
	}{
		{nil, false, ""}, {errors.Join(agent.ErrPlannerModelError, errors.New("synthetic 503")), false, "planner_model_unavailable"},
		{agent.ErrPlannerNoProgress, false, "planner_no_progress"}, {agent.ErrToolFailureLoop, false, "planner_tool_failure_loop"},
		{agent.ErrPlannerReadLoop, false, "planner_read_loop"}, {agent.ErrTaskBudgetExpired, true, "task_deadline"},
		{errors.Join(agent.ErrPlannerModelError, &agent.ModelCallTimeoutError{Limit: 20 * time.Minute}), false, "planner_model_timeout"},
		{context.DeadlineExceeded, true, "planner_round_timeout"}, {context.Canceled, false, "other"},
	} {
		if got := plannerFailureCode(tc.err, tc.timeout); got != tc.code {
			t.Fatalf("got %s want %s", got, tc.code)
		}
	}
}

func TestPlannerLoopStopsAfterThreeProviderFailures(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask("planner loop failure fixture", "synthetic only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	if _, err := task.Store.AddGoal(map[string]any{"text": "unfinished fixture"}, "fixture"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	provider := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls.Add(1)
		return llm.Message{}, "", llm.Usage{}, errors.New("HTTP status 503: backend_unavailable")
	}}
	planner := agent.NewPlanner(provider, "fixture", t.TempDir(), nil, 10000, 4)
	planner.SetNonStreaming(func() bool { return true })
	e := NewEngine(m)
	e.debounce = 5 * time.Millisecond
	e.plannerRetryDelay = 30 * time.Millisecond
	e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return planner, nil })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); e.plannerLoop(ctx, task) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("planner loop did not stop")
		}
	}()
	for round := 1; round <= 3; round++ {
		if round == 1 {
			task.Notify() // later rounds must wake without another edge or the long heartbeat
		}
		until := time.NewTimer(10 * time.Second)
		poll := time.NewTicker(20 * time.Millisecond)
		for {
			_, streak, err := task.Store.PlannerFailureStreak(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if streak >= round {
				break
			}
			select {
			case <-until.C:
				t.Fatalf("round %d did not persist an outcome", round)
			case <-poll.C:
			}
		}
		until.Stop()
		poll.Stop()
	}
	for deadline := time.Now().Add(10 * time.Second); task.lifecycleSnapshot().Status != "failed"; {
		if time.Now().After(deadline) {
			t.Fatal("planner outcomes did not stop task")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if calls.Load() != 3 {
		t.Fatalf("provider called %d times, expected 3 bounded rounds", calls.Load())
	}
}

func TestPlannerRepeatedFaultStopsWithoutCompletingOrErasingProgress(t *testing.T) {
	for _, code := range []string{"planner_model_unavailable", "planner_model_timeout", "planner_no_progress", "planner_tool_failure_loop", "planner_round_timeout"} {
		t.Run(code, func(t *testing.T) {
			m, err := NewManager(t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			task, err := m.CreateTask("planner protection fixture", "synthetic only", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			id, _ := strconv.ParseInt(task.ID, 10, 64)
			defer m.pg.DeleteTask(id)
			if _, err := task.Store.AddGoal(map[string]any{"text": "unfinished synthetic goal"}, "fixture"); err != nil {
				t.Fatal(err)
			}
			state, err := task.Store.BeginPlannerRound()
			if err != nil {
				t.Fatal(err)
			}
			state.Todos = json.RawMessage(`[{"content":"retained pending work","status":"pending"}]`)
			state.Outcome = "running"
			if ok, err := task.Store.SavePlannerState(state); err != nil || !ok {
				t.Fatalf("checkpoint: %v %v", ok, err)
			}
			e := NewEngine(m)
			for i := 1; i <= 3; i++ {
				e.recordPlannerOutcome(t.Context(), task, int64(i), code)
				if i < 3 && task.lifecycleSnapshot().Status == "failed" {
					t.Fatal("stopped before third failed round")
				}
			}
			stored, err := m.pg.GetTask(id)
			if err != nil || stored.Status != "failed" {
				t.Fatalf("task not stopped: %+v %v", stored, err)
			}
			var raw []byte
			if err := m.pg.QueryRow(`SELECT todos FROM exploration_planner_state WHERE exploration_id=$1`, task.ExpID).Scan(&raw); err != nil || len(raw) < 5 {
				t.Fatalf("progress erased: %s %v", raw, err)
			}
			if open, err := task.Store.HasOpenGoal(); err != nil || !open {
				t.Fatal("failure marked unfinished goal as met")
			}
		})
	}
}

func TestCanceledPlannerRoundAddsNoFaultOutcome(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask("canceled planner", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	NewEngine(m).recordPlannerOutcome(ctx, task, 1, "planner_model_unavailable")
	_, streak, err := task.Store.PlannerFailureStreak(t.Context())
	if err != nil || streak != 0 {
		t.Fatalf("user cancellation counted: %d %v", streak, err)
	}
}
