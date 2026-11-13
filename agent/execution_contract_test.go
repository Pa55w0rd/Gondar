package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"gondar/db"
)

func TestWorkerVerificationDeliveryUsesRealReceipts(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(fmt.Sprint(repair), func(t *testing.T) {
			d := testDB(t)
			defer d.Close()
			task, err := d.CreateTask(t.Name(), "local synthetic observations only", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer d.DeleteTask(task.ID)
			store := d.Exploration(task.ExplorationID)
			node, err := store.AddNode(db.KindFact, map[string]any{"summary": "受控观察", "evidence": "本地合成响应，尚缺基线对照", "confidence": "observed"}, 0, "confirmed", "fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			intentID, err := store.AddIntent(map[string]any{"summary": "仅核对已经保存的合成观察，不请求目标"}, 1, nil, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			intent, _ := store.GetNode(intentID)
			input := db.VerificationUpdate{Target: db.VerificationTarget{Endpoint: "https://owned.invalid/items", Method: "GET", Input: "none", Identity: "anonymous", Technique: "fixture"}, State: db.VerificationNegative, Summary: "受控验证", Observation: "合成材料尚不足", PrerequisitesMet: true, Evidence: []db.VerificationEvidence{{Role: "test", NodeID: node, Observation: "已有合成观察"}}}
			calls := 0
			provider := plannerStateProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if calls == 1 || (repair && calls == 3) {
					if calls == 3 {
						input.State = db.VerificationInconclusive
					}
					raw, _ := json.Marshal(input)
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint(calls), Name: "record_verification", Input: raw}}}, "tool_use", llm.Usage{}, nil
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("已记录阴性并完成")}}, "end_turn", llm.Usage{}, nil
			}}
			w := NewWorker(provider, "fixture", t.TempDir(), nil, 200000, 8)
			w.SetNonStreaming(func() bool { return true })
			var events []db.Activity
			_, writes, runErr := w.Execute(t.Context(), "fixture", task.ID, nil, store, intent, nil, func(a db.Activity) { events = append(events, a) }, nil, nil)
			page, err := store.ListVerifications(t.Context(), db.VerificationFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if repair {
				if runErr != nil || len(page.Cases) != 1 || page.Cases[0].State != db.VerificationInconclusive || writes.Facts != 1 {
					t.Fatalf("bounded correction failed: err=%v cases=%+v writes=%+v", runErr, page.Cases, writes)
				}
			} else {
				if !errors.Is(runErr, ErrVerificationDelivery) || len(page.Cases) != 0 || writes.Total() != 0 {
					t.Fatalf("failed delivery accepted: err=%v cases=%d writes=%+v", runErr, len(page.Cases), writes)
				}
				last := events[len(events)-1]
				if !last.IsError || !strings.Contains(last.Detail, "unresolved_verification_writes") {
					t.Fatal("failed delivery looked successful in terminal receipt")
				}
			}
			if calls > 4 {
				t.Fatalf("unbounded correction: %d", calls)
			}
		})
	}
}

func TestPlannerDistinctLookupsCanReachRealDispatchWithinConfiguredBudget(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask(t.Name(), "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	store := d.Exploration(task.ExplorationID)
	if _, err := store.AddGoal(map[string]any{"text": "verify independent saved material"}, "fixture"); err != nil {
		t.Fatal(err)
	}
	var facts []int64
	for i := 0; i < 18; i++ {
		id, err := store.AddNode(db.KindFact, map[string]any{"summary": fmt.Sprintf("local fixture %d", i), "evidence": fmt.Sprintf("saved distinct observation %d", i)}, 0, "confirmed", "fixture", nil)
		if err != nil {
			t.Fatal(err)
		}
		facts = append(facts, id)
	}
	calls := 0
	provider := plannerStateProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		name := "node_detail"
		var raw json.RawMessage
		if calls <= 18 {
			raw = json.RawMessage(fmt.Sprintf(`{"id":%d}`, facts[calls-1]))
		} else if calls == 19 {
			name = "add_intent"
			raw = json.RawMessage(fmt.Sprintf(`{"summary":"verify independent saved synthetic observation","parent_ids":[%d]}`, facts[0]))
		} else {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("committed local dispatch")}}, "end_turn", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprintf("fixture-%d", calls), Name: name, Input: raw}}}, "tool_use", llm.Usage{}, nil
	}}
	p := NewPlanner(provider, "fixture", t.TempDir(), nil, 200000, 24)
	p.SetNonStreaming(func() bool { return true })
	_, _, err = p.Plan(t.Context(), task.ID, nil, store, task.Goal, nil, nil)
	progress, readErr := store.PlannerProgress(t.Context())
	if err != nil || readErr != nil || calls != 20 || progress.ActiveIntents != 1 {
		t.Fatalf("legitimate reads blocked dispatch: calls=%d progress=%+v errors=%v %v", calls, progress, err, readErr)
	}
}

func TestPlannerWaitIsExplicitAndNotNoProgress(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTaskWithOptions(t.Name(), "synthetic", db.TaskCreateOptions{CompletionMode: db.CompletionContinuous})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	store := d.Exploration(task.ExplorationID)
	aid, err := store.AppendActivity(db.Activity{Kind: "tool_result", Summary: "合成登录态缺失", Detail: "synthetic 401"})
	if err != nil {
		t.Fatal(err)
	}
	write, err := store.UpsertVerification(t.Context(), db.VerificationUpdate{Target: db.VerificationTarget{Endpoint: "https://owned.invalid/items", Method: "GET", Input: "none", Identity: "anonymous", Technique: "fixture"}, State: db.VerificationMissingPrerequisite, Summary: "等待受控账号", Observation: "只有匿名观察", MissingPrerequisites: []string{"有效测试账号"}, Evidence: []db.VerificationEvidence{{Role: "blocker", ActivityID: aid, Observation: "synthetic 401"}}}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	wait := PlannerWait{Kind: "missing_prerequisite", Reason: "缺有效测试账号，剩余步骤依赖该条件", VerificationIDs: []string{write.Record.ID}, RemainingSteps: []string{"受控账号到位后核对该资源"}}
	calls := 0
	provider := plannerStateProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls == 1 || calls == 3 {
			raw, _ := json.Marshal(wait)
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "wait", Name: "record_planner_wait", Input: raw}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("等待条件")}}, "end_turn", llm.Usage{}, nil
	}}
	p := NewPlanner(provider, "fixture", t.TempDir(), nil, 200000, 5)
	p.SetNonStreaming(func() bool { return true })
	met, _, err := p.Plan(t.Context(), task.ID, nil, store, task.Goal, nil, nil)
	var waiting *PlannerWaitError
	if met || !errors.As(err, &waiting) || errors.Is(err, ErrPlannerNoProgress) || calls != 4 {
		t.Fatalf("waiting became failure or completed: met=%v err=%v calls=%d", met, err, calls)
	}
	if _, err := store.AddIntent(map[string]any{"summary": "new executable fixture"}, 1, nil, "fixture"); err != nil {
		t.Fatal(err)
	}
	if wait.Validate(t.Context(), store) == nil {
		t.Fatal("wait accepted despite new executable intent")
	}
}

func TestExecutionPromptAndCheckpointContracts(t *testing.T) {
	if strings.Contains(plannerDefaultTmpl, "最多接 3 次") || strings.Contains(plannerDefaultTmpl, "已证明没东西") || strings.Contains(plannerDefaultTmpl, "派生自某上游意图/发现也传") {
		t.Fatal("stale contract remains in default prompt")
	}
	if !idleStopWriteTools["record_verification"] || !plannerCheckpointTools["record_verification"] || !plannerCheckpointTools["list_verifications"] {
		t.Fatal("checkpoint cannot preserve verification evidence")
	}
	for _, seed := range BuiltinToolSeeds() {
		if seed.Key == "record_planner_wait" {
			if len(seed.Agents) != 1 || seed.Agents[0] != "planner" {
				t.Fatal("wait tool bound to execution roles")
			}
			return
		}
	}
	t.Fatal("wait tool missing from real catalog")
}
