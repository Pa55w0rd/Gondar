package server

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"gondar/agent"
)

func TestIdlePlannerRetryRespectsLifecycleWorkAndBudget(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, condition := range []string{"idle", "worker", "paused", "settling", "deadline", "terminal", "canceled", "success"} {
		t.Run(condition, func(t *testing.T) {
			task, err := m.CreateTask("retry boundary "+condition, "synthetic only", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			id, _ := strconv.ParseInt(task.ID, 10, 64)
			defer m.pg.DeleteTask(id)
			if _, err := task.Store.AddGoal(map[string]any{"text": "unfinished fixture"}, "fixture"); err != nil {
				t.Fatal(err)
			}
			e := NewEngine(m)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			code := "planner_no_progress"
			switch condition {
			case "worker":
				if _, err := task.Store.AddIntent(map[string]any{"summary": "queued synthetic work"}, 1, nil, "fixture"); err != nil {
					t.Fatal(err)
				}
			case "paused":
				e.Pause(task.ID, agent.AbortPausedByUser)
			case "settling":
				e.markSettling(task.ID, "timeout")
			case "deadline":
				e.deadline.Store(task.ID, time.Now().Add(5*time.Second).Unix())
			case "terminal":
				task.updateLifecycle(func(s *taskLifecycleState) { s.Status = "timeout" })
			case "canceled":
				cancel()
			case "success":
				code = ""
			}
			if got := e.shouldRetryIdlePlanner(ctx, task, code); got != (condition == "idle") {
				t.Fatalf("unexpected retry under %s: %v", condition, got)
			}
		})
	}
}

func TestFinalEnginePlannerKeepsExpiredTaskStateAndIndependentBudget(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask("final engine fixture", "synthetic only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	calls := 0
	provider := retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < time.Minute || time.Until(deadline) > 3*time.Minute {
			t.Errorf("invalid final hard budget: %v", time.Until(deadline))
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("synthetic only; goal remains unfinished")}}, "end_turn", llm.Usage{}, nil
	}}
	planner := agent.NewPlanner(provider, "fixture", t.TempDir(), nil, 10000, 0)
	planner.SetNonStreaming(func() bool { return true })
	e := NewEngine(m)
	e.deadline.Store(task.ID, int64(1))
	e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return planner, nil })
	if e.runFinalPlannerRound(t.Context(), task, "timeout") || calls != 1 {
		t.Fatalf("settlement marked success or replayed: calls=%d", calls)
	}
	if retryableWorkerModelError(harness.ReasonModelError, agent.ErrTaskBudgetExpired) {
		t.Fatal("task expiry permits replay")
	}
	if isRetryableStreamError(agent.ErrTaskBudgetExpired) {
		t.Fatal("task expiry permits stream replay")
	}
	e.recordPlannerOutcome(t.Context(), task, 1, "task_deadline")
	_, count, err := task.Store.PlannerFailureStreak(t.Context())
	if err != nil || count != 0 {
		t.Fatalf("budget counted as planner fault: %d %v", count, err)
	}
}

func TestTaskDeadlineStopsTaskRouterBeforeReplay(t *testing.T) {
	p := &scriptedLLMProvider{events: []llm.StreamEvent{{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 17}}}, err: agent.ErrTaskBudgetExpired}
	var returned error
	for _, err := range streamTaskLLM(t.Context(), "synthetic", llm.CompletionRequest{}, taskLLMStreamHooks{current: func() (taskLLMSelection, error) {
		return taskLLMSelection{provider: p, profileID: 5, retry: agent.RetryConfig{StreamAttempts: 5, StreamInterval: time.Nanosecond}}, nil
	}}) {
		if err != nil {
			returned = err
		}
	}
	if returned != agent.ErrTaskBudgetExpired || p.calls != 1 {
		t.Fatalf("task expiry replayed: calls=%d err=%v", p.calls, returned)
	}
}
