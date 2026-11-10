package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"gondar/db"
)

func TestPlannerBudgetsAreBoundedAndHonorShorterConfiguration(t *testing.T) {
	p := NewPlanner(nil, "", "", nil, 0, 0)
	if plannerTurnBudget(0) != 24 || plannerTurnBudget(7) != 7 || p.runDuration() != 5*time.Minute || p.RoundLimit() != 6*time.Minute {
		t.Fatal("unconfigured planner has no finite defaults")
	}
	p.SetRunTimeout(30 * time.Second)
	if p.runDuration() != 30*time.Second || p.RoundLimit() != 90*time.Second {
		t.Fatal("shorter budget was ignored")
	}
	p.SetRunTimeout(time.Hour)
	if p.runDuration() != 5*time.Minute {
		t.Fatal("configured budget bypassed the main exploration ceiling")
	}
}

func TestPlannerDraftDoesNotCountAsDispatchAndGetsOneCorrection(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("draft-only planner", "synthetic only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	if _, err := s.AddGoal(map[string]any{"text": "synthetic goal"}, "fixture"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	provider := plannerStateProvider{complete: func(_ context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("已经写好草稿，已派发")}}, "end_turn", llm.Usage{}, nil
	}}
	p := NewPlanner(provider, "fixture", t.TempDir(), nil, 10000, 0)
	p.SetNonStreaming(func() bool { return true })
	var rows []db.Activity
	met, _, err := p.Plan(t.Context(), task.ID, nil, s, task.Goal, nil, func(a db.Activity) { rows = append(rows, a) })
	if !errors.Is(err, ErrPlannerNoProgress) || met || calls != 2 {
		t.Fatalf("draft accepted or correction unbounded: met=%v calls=%d err=%v", met, calls, err)
	}
	if len(rows) == 0 || !rows[len(rows)-1].IsError {
		t.Fatal("terminal draft looked successful")
	}
	progress, err := s.PlannerProgress(t.Context())
	if err != nil || progress.ActiveIntents != 0 || progress.OpenGoals != 1 {
		t.Fatalf("changed actual state: %+v %v", progress, err)
	}
}

func TestPlannerCorrectionCanSubmitRealIntent(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("planner actual dispatch", "synthetic only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	if _, err := s.AddGoal(map[string]any{"text": "synthetic goal"}, "fixture"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	provider := plannerStateProvider{complete: func(_ context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls == 2 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "real-dispatch", Name: "add_intent", Input: json.RawMessage(`{"intents":[{"summary":"synthetic local verification", "priority":1}]}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("本轮结束")}}, "end_turn", llm.Usage{}, nil
	}}
	p := NewPlanner(provider, "fixture", t.TempDir(), nil, 10000, 6)
	p.SetNonStreaming(func() bool { return true })
	if _, _, err := p.Plan(t.Context(), task.ID, nil, s, task.Goal, nil, nil); err != nil {
		t.Fatal(err)
	}
	progress, err := s.PlannerProgress(t.Context())
	if err != nil || progress.ActiveIntents != 1 || calls != 3 {
		t.Fatalf("no actual dispatch: %+v calls=%d err=%v", progress, calls, err)
	}
}

func TestPlannerModelFailureHasTypedCause(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("planner failed provider", "synthetic only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	provider := plannerStateProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return llm.Message{}, "", llm.Usage{}, fmt.Errorf("HTTP status 503: backend_unavailable")
	}}
	p := NewPlanner(provider, "fixture", t.TempDir(), nil, 10000, 3)
	p.SetNonStreaming(func() bool { return true })
	_, _, err = p.Plan(t.Context(), task.ID, nil, d.Exploration(task.ExplorationID), task.Goal, nil, nil)
	if !errors.Is(err, ErrPlannerModelError) {
		t.Fatalf("provider failure unclassified: %v", err)
	}
}

func TestPlannerToolErrorsCannotBeHiddenByDraftsOrChangedInputs(t *testing.T) {
	g := toolFailureLoop{planner: true}
	for i := 0; i < 3; i++ {
		id := fmt.Sprint(i)
		g.start(id, "Bash", []byte(fmt.Sprintf(`{"command":"draft-%d"}`, i)))
		err := g.result(id, true, "Unknown tool: Bash")
		if (i == 2) != errors.Is(err, ErrToolFailureLoop) {
			t.Fatalf("failure %d: %v", i, err)
		}
		g.start("draft", "Write", []byte(`{}`))
		_ = g.result("draft", false, "draft written")
	}
}
