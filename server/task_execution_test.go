package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"gondar/agent"
	"gondar/db"
)

func TestContinueContinuousTaskPreservesCompletedGoalsAndRejectsDuplicate(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTaskWithOptions("continue fixture", "synthetic only", db.TaskCreateOptions{CompletionMode: db.CompletionContinuous})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	goal, _ := task.Store.AddGoal(map[string]any{"text": "old delivery"}, "fixture")
	task.Store.SetNodeState(goal, "met")
	fact, _ := task.Store.AddNode(db.KindFact, map[string]any{"summary": "retained evidence"}, 0, "confirmed", "fixture", nil)
	if err := m.SetTaskStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	s := newAdmissionTestServer(m, nil)
	request := httptest.NewRequest("POST", "/api/tasks/"+task.ID+"/continue", nil)
	request.SetPathValue("id", task.ID)
	w := httptest.NewRecorder()
	s.continueTask(w, request)
	if w.Code != 200 {
		t.Fatalf("continue: %d %s", w.Code, w.Body.String())
	}
	stored, _ := m.pg.GetTask(id)
	if isTerminalStatus(stored.Status) || stored.Paused || stored.CompletedAt != nil || stored.FirstRunAt != nil {
		t.Fatalf("not admitted fresh: %+v", stored)
	}
	g, _ := task.Store.GetNode(goal)
	f, _ := task.Store.GetNode(fact)
	if g == nil || g.State != "met" || f == nil {
		t.Fatal("continuation destroyed previous proof/evidence")
	}
	w = httptest.NewRecorder()
	s.continueTask(w, request)
	if w.Code != 409 {
		t.Fatalf("duplicate continue accepted: %d", w.Code)
	}
}

func TestExecutionAPIChangesOnlyStoppedTaskAndDoesNotStartIt(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask("execution API fixture", "synthetic", nil, 120, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	s := newAdmissionTestServer(m, nil)
	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PATCH", "/api/tasks/"+task.ID+"/execution", strings.NewReader(body))
		req.SetPathValue("id", task.ID)
		w := httptest.NewRecorder()
		s.updateTaskExecution(w, req)
		return w
	}
	if w := call(`{"completion_mode":"continuous"}`); w.Code != 409 {
		t.Fatalf("live config mutation: %d %s", w.Code, w.Body.String())
	}
	if err := m.SetTaskStatus(task.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	if w := call(`{"completion_mode":"continuous","timeout_seconds":0}`); w.Code != 200 {
		t.Fatalf("config failed: %d %s", w.Code, w.Body.String())
	}
	state := task.lifecycleSnapshot()
	stored, _ := m.pg.GetTask(id)
	if state.Status != "timeout" || stored.Status != "timeout" || state.TimeoutSeconds != 0 || state.CompletionMode != db.CompletionContinuous || s.engine.Started(task.ID) {
		t.Fatal("config change started task or failed to update trusted mode")
	}
	for _, body := range []string{`{"completion_mode":"bad"}`, `{"timeout_seconds":-1}`, `{}`} {
		if w := call(body); w.Code != 400 {
			t.Fatalf("invalid config accepted: %s %d", body, w.Code)
		}
	}
}

func TestContinuousAutoStopPausesWithoutClaimingCompletion(t *testing.T) {
	for _, reason := range []string{"auto_idle", "auto_coverage", "auto_rounds"} {
		t.Run(reason, func(t *testing.T) {
			m, err := NewManager(t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			task, err := m.CreateTaskWithOptions("continuous stop fixture", "synthetic", db.TaskCreateOptions{CompletionMode: db.CompletionContinuous})
			if err != nil {
				t.Fatal(err)
			}
			id, _ := strconv.ParseInt(task.ID, 10, 64)
			defer m.pg.DeleteTask(id)
			goal, _ := task.Store.AddGoal(map[string]any{"text": "retained delivery"}, "fixture")
			task.Store.SetNodeState(goal, "met")
			e := NewEngine(m)
			e.settleTaskWithReason(t.Context(), task, reason, "synthetic protection")
			stored, _ := m.pg.GetTask(id)
			g, _ := task.Store.GetNode(goal)
			if !stored.Paused || stored.Status == "done" || stored.CompletedAt != nil || !e.IsPaused(task.ID) || g.State != "met" {
				t.Fatalf("protection misreported completion: %+v", stored)
			}
		})
	}
}

func TestEngineContinuesAfterAllOldGoalsMet(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTaskWithOptions("continuous planner loop", "synthetic", db.TaskCreateOptions{CompletionMode: db.CompletionContinuous})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	goal, _ := task.Store.AddGoal(map[string]any{"text": "already met"}, "fixture")
	task.Store.SetNodeState(goal, "met")
	calls := 0
	provider := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "continued", Name: "add_intent", Input: json.RawMessage(`{"intents":[{"summary":"synthetic next direction","priority":1}]}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("new direction queued")}}, "end_turn", llm.Usage{}, nil
	}}
	p := agent.NewPlanner(provider, "fixture", t.TempDir(), nil, 10000, 0)
	p.SetNonStreaming(func() bool { return true })
	e := NewEngine(m)
	e.debounce = 5 * time.Millisecond
	e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return p, nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); e.plannerLoop(ctx, task) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("planner did not stop")
		}
	}()
	task.Notify()
	limit := time.NewTimer(10 * time.Second)
	defer limit.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for {
		active, err := task.Store.HasActiveIntent()
		if err != nil {
			t.Fatal(err)
		}
		if active {
			break
		}
		select {
		case <-limit.C:
			t.Fatal("met goals short-circuited real continuation")
		case <-poll.C:
		}
	}
	if task.lifecycleSnapshot().Status == "done" {
		t.Fatal("continuous task marked done")
	}
}

func TestInterruptedPlannerResponseStillCountsTowardFaultProtection(t *testing.T) {
	err := errors.Join(agent.ErrPlannerModelError, io.ErrUnexpectedEOF)
	if plannerFailureCode(err, false) != "planner_response_interrupted" {
		t.Fatal("transport interruption not distinguished")
	}
	m, createErr := NewManager(t.TempDir(), "")
	if createErr != nil {
		t.Fatal(createErr)
	}
	defer m.Close()
	task, createErr := m.CreateTaskWithOptions("interrupted provider protection", "synthetic", db.TaskCreateOptions{CompletionMode: db.CompletionContinuous})
	if createErr != nil {
		t.Fatal(createErr)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	goal, _ := task.Store.AddGoal(map[string]any{"text": "unfinished synthetic goal"}, "fixture")
	e := NewEngine(m)
	for round := int64(1); round <= 3; round++ {
		e.recordPlannerOutcome(t.Context(), task, round, plannerFailureCode(err, false))
	}
	stored, _ := m.pg.GetTask(id)
	g, _ := task.Store.GetNode(goal)
	if stored.Status != "failed" || g == nil || g.State != "open" {
		t.Fatalf("interruption bypassed protection or erased progress: %+v", stored)
	}
}

func TestContinueAfterRoundBudgetPauseGetsFreshBoundedRun(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTaskWithOptions("budget follow-up", "synthetic", db.TaskCreateOptions{CompletionMode: db.CompletionContinuous, TimeoutSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	goal, _ := task.Store.AddGoal(map[string]any{"text": "retained delivery"}, "fixture")
	task.Store.SetNodeState(goal, "met")
	m.StampTaskFirstRun(task.ID)
	if _, err := m.pg.RecordTaskProgress(task.ID, 80, 1, 0, 0); err != nil {
		t.Fatal(err)
	}
	s := newAdmissionTestServer(m, nil)
	if n := s.engine.nextPlannerRound(task.ID); n != 81 {
		t.Fatalf("restart lost persisted round budget: %d", n)
	}
	s.engine.pauseContinuousRun(task, "auto_rounds", "synthetic round cap")
	req := httptest.NewRequest("POST", "/api/tasks/"+task.ID+"/continue", nil)
	req.SetPathValue("id", task.ID)
	w := httptest.NewRecorder()
	s.continueTask(w, req)
	if w.Code != 200 {
		t.Fatalf("follow-up: %d %s", w.Code, w.Body.String())
	}
	state := task.lifecycleSnapshot()
	progress, err := m.pg.GetTaskProgress(task.ID)
	if err != nil || progress.RoundNo != 0 || progress.IdleRounds != 0 || state.FirstRunAt != 0 || state.DeadlineAt != 0 {
		t.Fatalf("stale budget after follow-up: %+v %+v %v", state, progress, err)
	}
	if n := s.engine.nextPlannerRound(task.ID); n != 1 {
		t.Fatalf("immediate re-pause from stale cap: %d", n)
	}
	if !s.engine.shouldRetryIdlePlanner(t.Context(), task, "planner_no_progress") {
		t.Fatal("met delivery goals disabled short bounded retry")
	}
	g, _ := task.Store.GetNode(goal)
	if g.State != "met" {
		t.Fatal("fresh run erased prior proof")
	}
}

func TestStaleDeadlineCoordinatorCannotSettleNewRun(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask("stale coordinator", "synthetic", nil, 60, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	e := NewEngine(m)
	old := make(chan struct{})
	e.coordStarted.Store(task.ID, make(chan struct{}))
	e.deadline.Store(task.ID, int64(1))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	e.deadlineCoordinator(ctx, task, old)
	if e.isSettling(task.ID) || isTerminalStatus(task.lifecycleSnapshot().Status) {
		t.Fatal("old run coordinator modified current execution")
	}
}

func TestContinuousUnproductivePlanningPausesWithProgress(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTaskWithOptions("no progress pause", "synthetic", db.TaskCreateOptions{CompletionMode: db.CompletionContinuous})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	goal, _ := task.Store.AddGoal(map[string]any{"text": "retained goal"}, "fixture")
	task.Store.SetNodeState(goal, "met")
	e := NewEngine(m)
	for round := int64(1); round <= 3; round++ {
		e.recordPlannerOutcome(t.Context(), task, round, "planner_no_progress")
	}
	stored, _ := m.pg.GetTask(id)
	g, _ := task.Store.GetNode(goal)
	if !stored.Paused || isTerminalStatus(stored.Status) || g.State != "met" {
		t.Fatalf("no progress was claimed complete or erased: %+v", stored)
	}
}
