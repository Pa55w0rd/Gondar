package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"gondar/db"
)

func TestFinalPlannerUsesLiveBudgetAfterTaskDeadline(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("expired final budget fixture", "synthetic only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	for _, configured := range []time.Duration{0, 30 * time.Second} {
		t.Run(configured.String(), func(t *testing.T) {
			calls := 0
			provider := &modelCallTimeoutProvider{timeout: 10 * time.Minute, inner: plannerStateProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				deadline, ok := ctx.Deadline()
				remaining := time.Until(deadline)
				ceiling := 2 * time.Minute
				if configured > 0 {
					ceiling = configured
				}
				if !ok || remaining < ceiling-5*time.Second || remaining > ceiling {
					t.Errorf("final budget collapsed/extended: %v", remaining)
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("synthetic settlement retained")}}, "end_turn", llm.Usage{}, nil
			}}}
			p := NewPlanner(provider, "fixture", t.TempDir(), nil, 10000, 0)
			p.SetRunTimeout(configured)
			p.SetNonStreaming(func() bool { return true })
			ctx := WithTaskClock(t.Context(), TaskClock{DeadlineUnix: 1, Final: true, FinalReason: "timeout"})
			if _, _, err := p.Plan(ctx, task.ID, nil, d.Exploration(task.ExplorationID), task.Goal, nil, nil); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("unexpected final replay: %d", calls)
			}
		})
	}
}

func TestTaskBudgetExpiryIsNotProviderFailure(t *testing.T) {
	provider := &modelCallTimeoutProvider{timeout: time.Minute, inner: plannerStateProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		<-ctx.Done()
		return llm.Message{}, "", llm.Usage{}, ctx.Err()
	}}}
	ctx := WithTaskClock(t.Context(), TaskClock{DeadlineUnix: 1})
	var rows []db.Activity
	_, reason, err := captureRun(ctx, agentcore.Options{Provider: provider, NonStreaming: true, MaxTurns: 3, MaxDuration: time.Minute, WorkingDir: t.TempDir()}, "synthetic only", func(a db.Activity) { rows = append(rows, a) })
	if !errors.Is(err, ErrTaskBudgetExpired) || errors.Is(err, ErrModelCallTimeout) || reason != harness.ReasonTimeout || ctx.Err() != nil {
		t.Fatalf("task budget mislabeled: %s %v parent=%v", reason, err, ctx.Err())
	}
	terminal := 0
	for _, row := range rows {
		if row.Kind == "result" {
			terminal++
			if row.IsError {
				t.Fatalf("budget marked as provider error: %+v", row)
			}
		}
	}
	if terminal != 1 {
		t.Fatalf("terminal count=%d", terminal)
	}
}

func TestPlannerFailedDraftPreservesPreviousHandoff(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("handoff draft protection", "synthetic only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	if _, err := s.AddGoal(map[string]any{"text": "unfinished fixture"}, "fixture"); err != nil {
		t.Fatal(err)
	}
	state, err := s.BeginPlannerRound()
	if err != nil {
		t.Fatal(err)
	}
	state.Summary = "retained valid handoff"
	state.Outcome = "completed"
	if ok, err := s.SavePlannerState(state); err != nil || !ok {
		t.Fatalf("save: %v %v", ok, err)
	}
	calls := 0
	reply := "No answer requested."
	provider := plannerStateProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(reply)}}, "end_turn", llm.Usage{}, nil
	}}
	p := NewPlanner(provider, "fixture", t.TempDir(), nil, 10000, 0)
	p.SetNonStreaming(func() bool { return true })
	if _, _, err := p.Plan(t.Context(), task.ID, nil, s, task.Goal, nil, nil); !errors.Is(err, ErrPlannerNoProgress) {
		t.Fatalf("err=%v", err)
	}
	got, err := s.BeginPlannerRound()
	if err != nil || got.Summary != state.Summary || got.Outcome != "error" || calls != 2 {
		t.Fatalf("valid handoff lost: %+v calls=%d err=%v", got, calls, err)
	}
	reply = "缺少受控会话，身份验证尚未完成，需要补充前置条件。"
	if _, _, err := p.Plan(t.Context(), task.ID, nil, s, task.Goal, nil, nil); !errors.Is(err, ErrPlannerNoProgress) {
		t.Fatalf("blocker prose incorrectly counted as execution: %v", err)
	}
	got, err = s.BeginPlannerRound()
	if err != nil || !strings.Contains(got.Summary, state.Summary) || !strings.Contains(got.Summary, reply) || !strings.Contains(got.Summary, "本轮未完成") || len([]rune(got.Summary)) > 2000 {
		t.Fatalf("old context or new blocker lost: %+v %v", got, err)
	}
}

type evidenceReviewHook struct {
	blocked bool
	calls   int
}

func (h *evidenceReviewHook) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	h.calls++
	return h.blocked, "inner policy", nil
}
func (*evidenceReviewHook) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (*evidenceReviewHook) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return false, nil, ""
}

func TestIdleStopKeepsEvidenceAndResourceWritesBehindInnerGuard(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprint(denied), func(t *testing.T) {
			inner := &evidenceReviewHook{blocked: denied}
			h := wrapIdleStop(inner, (&fakeWrites{}).get, nil)
			ageStopper(h.stop, time.Hour)
			for i := 0; i < idleStopRounds; i++ {
				h.stop.advance()
			}
			for _, name := range []string{"save_evidence", "register_resource", "update_resource"} {
				block, _, _ := h.PreToolUse(t.Context(), name, []byte(`{}`))
				if block != denied {
					t.Fatalf("%s: blocked=%v inner=%v", name, block, denied)
				}
				h.PostToolUse(t.Context(), name, nil, nil, false)
			}
			if inner.calls != 3 {
				t.Fatalf("inner guard bypassed: %d", inner.calls)
			}
			if block, _, _ := h.PreToolUse(t.Context(), "Bash", []byte(`{}`)); !block {
				t.Fatal("idle stop allowed new execution")
			}
		})
	}
}

func TestPlannerRecallAndNodeReadsStopThroughSDK(t *testing.T) {
	for _, name := range []string{"recall_lessons", "node_detail", "list_untested_assets", "list_resources"} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			p := plannerStateProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint(calls), Name: name, Input: json.RawMessage(`{}`)}}}, "tool_use", llm.Usage{}, nil
			}}
			read := readTool(name, "fixture", obj(nil), func(context.Context, json.RawMessage) (actool.Result, error) {
				return actool.Text(`{"unchanged":true}`), nil
			})
			_, _, err := captureRun(WithRunInfo(t.Context(), RunInfo{Kind: KindPlanner}), agentcore.Options{Provider: p, NonStreaming: true, Tools: []actool.CoreTool{read}, PermissionMode: permission.ModeBypass, WorkingDir: t.TempDir(), MaxTurns: 24}, "synthetic only", nil)
			if !errors.Is(err, ErrPlannerReadLoop) || calls != 6 {
				t.Fatalf("read loop unbounded: calls=%d err=%v", calls, err)
			}
		})
	}
}
