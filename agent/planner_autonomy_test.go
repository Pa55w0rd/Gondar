package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"gondar/db"
)

func autonomyFixture(t *testing.T) (*db.DB, *db.Task, *db.ExplorationStore, PlannerWait, int64) {
	t.Helper()
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTaskWithOptions(t.Name(), "synthetic local fixture only", db.TaskCreateOptions{CompletionMode: db.CompletionContinuous})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTask(task.ID) })
	store := d.Exploration(task.ExplorationID)
	aid, err := store.AppendActivity(db.Activity{Kind: "tool_result", Summary: "synthetic prerequisite", Detail: "local fixture 401"})
	if err != nil {
		t.Fatal(err)
	}
	write, err := store.UpsertVerification(t.Context(), db.VerificationUpdate{Target: db.VerificationTarget{Endpoint: "https://owned.invalid/fixture", Method: "GET", Input: "none", Identity: "anonymous", Technique: "fixture"}, State: db.VerificationMissingPrerequisite, Summary: "synthetic account missing", Observation: "local anonymous observation", MissingPrerequisites: []string{"controlled account"}, Evidence: []db.VerificationEvidence{{Role: "blocker", ActivityID: aid, Observation: "local fixture 401"}}}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return d, task, store, PlannerWait{Kind: "missing_prerequisite", Reason: "synthetic branch needs account", VerificationIDs: []string{write.Record.ID}, RemainingSteps: []string{"check independent saved material before requesting an account"}}, write.Record.NodeID
}

func TestPlannerAutonomyTurnsWaitIntoRealGoalAndIntent(t *testing.T) {
	for _, runningAtStart := range []bool{false, true} {
		t.Run(fmt.Sprint(runningAtStart), func(t *testing.T) {
			_, task, store, wait, basis := autonomyFixture(t)
			oldGoal, err := store.AddGoal(map[string]any{"text": "original preserved goal"}, "human")
			if err != nil {
				t.Fatal(err)
			}
			var previousIntent int64
			if runningAtStart {
				previousIntent, err = store.AddIntent(map[string]any{"summary": "previous local fixture"}, 1, nil, "fixture")
				if err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			provider := plannerStateProvider{complete: func(_ context.Context, request llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				tool := func(name string, raw []byte) (llm.Message, string, llm.Usage, error) {
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprintf("fixture-%d", calls), Name: name, Input: raw}}}, "tool_use", llm.Usage{}, nil
				}
				switch calls {
				case 1:
					if previousIntent > 0 {
						if err := store.SetNodeState(previousIntent, "done"); err != nil {
							t.Fatal(err)
						}
					}
					raw, _ := json.Marshal(wait)
					return tool("record_planner_wait", raw)
				case 3:
					raw, _ := json.Marshal(request.Messages)
					if !strings.Contains(string(raw), "宿主自动回复") {
						t.Error("automatic response not delivered to actual SDK session")
					}
					return tool("add_exploration_goal", []byte(fmt.Sprintf(`{"text":"Verify independent local material","parent_ids":[%d]}`, basis)))
				case 4:
					return tool("add_intent", []byte(fmt.Sprintf(`{"summary":"verify independent synthetic local material","parent_ids":[%d],"priority":2}`, basis)))
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("local round conclusion")}}, "end_turn", llm.Usage{}, nil
			}}
			var automatic atomic.Int32
			p := NewPlanner(provider, "fixture", t.TempDir(), nil, 200000, 8)
			p.SetNonStreaming(func() bool { return true })
			met, _, err := p.Plan(t.Context(), task.ID, nil, store, task.Goal, nil, func(activity db.Activity) {
				if strings.Contains(string(activity.Metadata), "planner_auto_response") {
					automatic.Add(1)
				}
			})
			if met || err != nil || calls != 5 || automatic.Load() != 1 {
				t.Fatalf("automatic dispatch failed: met=%v err=%v calls=%d automatic=%d", met, err, calls, automatic.Load())
			}
			progress, err := store.PlannerProgress(t.Context())
			if err != nil || progress.ActiveIntents != 1 {
				t.Fatalf("no committed intent: %+v %v", progress, err)
			}
			goals, err := store.ListByKind(db.KindGoal, 10)
			if err != nil || len(goals) != 2 {
				t.Fatalf("new goal not saved: %d %v", len(goals), err)
			}
			original, err := store.GetNode(oldGoal)
			if err != nil || original.State != "open" || original.Origin != "human" {
				t.Fatal("original goal changed")
			}
		})
	}
}

func TestPlannerAutonomyKeepsNewEvidenceInsteadOfStaleWait(t *testing.T) {
	_, _, store, wait, _ := autonomyFixture(t)
	before, err := store.PlannerProgress(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	g := plannerProgressGuard{store: store, before: before, continuous: true, wait: func() *PlannerWait { return &wait }}
	if _, err := store.AddNode(db.KindFact, map[string]any{"summary": "new independent observation", "evidence": "actual saved local fixture content", "confidence": "observed"}, 0, "confirmed", "fixture", nil); err != nil {
		t.Fatal(err)
	}
	if err := g.validateResult(t.Context()); err != nil {
		t.Fatalf("new content incorrectly paused by stale wait: %v", err)
	}
}

func TestPlannerAutonomyCannotSkipReviewAtRoundCutoff(t *testing.T) {
	_, _, store, wait, _ := autonomyFixture(t)
	before, err := store.PlannerProgress(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	g := plannerProgressGuard{store: store, before: before, continuous: true, wait: func() *PlannerWait { return &wait }, recoverWait: func(context.Context, PlannerWait) string { return "automatic local review" }}
	if err := g.validateResult(t.Context()); !errors.Is(err, ErrPlannerNoProgress) || errors.Is(err, ErrPlannerWaiting) {
		t.Fatalf("cutoff bypassed review: %v", err)
	}
	stop, feedback, _ := g.Stop(t.Context(), nil)
	if stop || len(feedback) != 1 || !g.waitRecovered {
		t.Fatal("first wait did not get bounded automatic response")
	}
	stop, feedback, _ = g.Stop(t.Context(), nil)
	if !stop || len(feedback) != 0 {
		t.Fatal("automatic response repeated without bound")
	}
}

func TestPlannerAutonomyDraftGetsOneResponseAndStillFailsWithoutAction(t *testing.T) {
	_, task, store, _, _ := autonomyFixture(t)
	calls := 0
	provider := plannerStateProvider{complete: func(_ context.Context, request llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls == 2 {
			raw, _ := json.Marshal(request.Messages)
			if !strings.Contains(string(raw), "宿主自动回复") {
				t.Error("draft-only planner did not receive automatic response")
			}
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("continue without any real action")}}, "end_turn", llm.Usage{}, nil
	}}
	p := NewPlanner(provider, "fixture", t.TempDir(), nil, 200000, 8)
	p.SetNonStreaming(func() bool { return true })
	_, _, err := p.Plan(t.Context(), task.ID, nil, store, task.Goal, nil, nil)
	if !errors.Is(err, ErrPlannerNoProgress) || errors.Is(err, ErrPlannerWaiting) || calls != 2 {
		t.Fatalf("automatic response disabled no-progress protection: err=%v calls=%d", err, calls)
	}
}
