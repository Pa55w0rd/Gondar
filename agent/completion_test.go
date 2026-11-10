package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"gondar/db"
)

func TestCompletionToolsHonorTrustedModeAndRequireProof(t *testing.T) {
	for _, mode := range []string{db.CompletionObjective, db.CompletionContinuous} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			defer d.Close()
			task, err := d.CreateTaskWithOptions("completion fixture", "synthetic", db.TaskCreateOptions{CompletionMode: mode})
			if err != nil {
				t.Fatal(err)
			}
			defer d.DeleteTask(task.ID)
			s := d.Exploration(task.ExplorationID)
			goal, _ := s.AddGoal(map[string]any{"text": "synthetic delivery"}, "fixture")
			fact, _ := s.AddNode(db.KindFact, map[string]any{"summary": "synthetic proof"}, 0, "confirmed", "fixture", nil)
			ts := NewToolSet(s, "planner")
			result, err := ts.goalMet().Call(t.Context(), json.RawMessage(`{"reason":"暂不收官，结束本轮"}`), nil)
			if err != nil || !result.IsError || ts.GoalMet {
				t.Fatalf("unproven shortcut accepted: %+v %v", result, err)
			}
			result, err = ts.proveGoal().Call(t.Context(), json.RawMessage(fmt.Sprintf(`{"goal_id":%d,"evidence_id":%d,"reason":"synthetic proof"}`, goal, fact)), nil)
			if err != nil || result.IsError || ts.GoalMet != (mode == db.CompletionObjective) {
				t.Fatalf("wrong completion transition: mode=%s met=%v result=%+v err=%v", mode, ts.GoalMet, result, err)
			}
			g, _ := s.GetNode(goal)
			if g.State != "met" {
				t.Fatal("individual proof discarded")
			}
			if mode == db.CompletionContinuous {
				result, _ = ts.goalMet().Call(t.Context(), json.RawMessage(`{"reason":"所有旧目标已经met"}`), nil)
				if !result.IsError || ts.GoalMet {
					t.Fatal("all old goals bypassed continuous policy")
				}
			}
		})
	}
}

func TestContinuousPlannerWithMetGoalsMustDispatchRealWork(t *testing.T) {
	for _, dispatch := range []bool{true, false} {
		t.Run(fmt.Sprint(dispatch), func(t *testing.T) {
			d := testDB(t)
			defer d.Close()
			task, err := d.CreateTaskWithOptions("continuous checkpoint", "synthetic only", db.TaskCreateOptions{CompletionMode: db.CompletionContinuous})
			if err != nil {
				t.Fatal(err)
			}
			defer d.DeleteTask(task.ID)
			s := d.Exploration(task.ExplorationID)
			goal, _ := s.AddGoal(map[string]any{"text": "old delivery"}, "fixture")
			s.SetNodeState(goal, "met")
			calls := 0
			provider := plannerStateProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if dispatch && calls == 1 {
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "new-direction", Name: "add_intent", Input: json.RawMessage(`{"intents":[{"summary":"synthetic new direction","priority":1}]}`)}}}, "tool_use", llm.Usage{}, nil
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("all previous goals met")}}, "end_turn", llm.Usage{}, nil
			}}
			p := NewPlanner(provider, "fixture", t.TempDir(), nil, 10000, 0)
			p.SetNonStreaming(func() bool { return true })
			met, _, err := p.Plan(t.Context(), task.ID, nil, s, task.Goal, nil, nil)
			if met {
				t.Fatal("continuous task completed from old goals")
			}
			if dispatch && err != nil {
				t.Fatal(err)
			}
			if !dispatch && !errors.Is(err, ErrPlannerNoProgress) {
				t.Fatalf("draft accepted: %v", err)
			}
			progress, _ := s.PlannerProgress(t.Context())
			if (progress.ActiveIntents > 0) != dispatch {
				t.Fatalf("dispatch mismatch: %+v", progress)
			}
		})
	}
}

func TestFinalPlannerCheckpointCannotStartTargetWork(t *testing.T) {
	inner := &evidenceReviewHook{}
	h := &plannerCheckpointHooks{inner: inner}
	for _, name := range []string{"Bash", "WebFetch", "add_intent", "unlocked_external_tool"} {
		if block, _, _ := h.PreToolUse(t.Context(), name, nil); !block {
			t.Fatalf("final exploration allowed: %s", name)
		}
	}
	for _, name := range []string{"save_evidence", "record_fact", "TodoWrite"} {
		if block, _, _ := h.PreToolUse(t.Context(), name, nil); block {
			t.Fatalf("checkpoint blocked: %s", name)
		}
	}
	inner.blocked = true
	if block, _, _ := h.PreToolUse(t.Context(), "save_evidence", nil); !block {
		t.Fatal("checkpoint bypassed inner guard")
	}
}
