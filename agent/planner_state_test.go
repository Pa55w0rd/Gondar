package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

type plannerStateProvider struct {
	complete func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
}

func (p plannerStateProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.complete(ctx, req)
}
func (p plannerStateProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{}, errors.New("non-streaming fixture expected"))
	}
}

func TestPlannerHandoffSurvivesNewPlannerInstance(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("durable planner fixture", "synthetic evidence only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTask(task.ID) })
	s := d.Exploration(task.ExplorationID)
	calls := 0
	provider := plannerStateProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "todo-fixture", Name: "TodoWrite", Input: json.RawMessage(`{"todos":[{"content":"verify synthetic artifact before dispatch","status":"pending"}]}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("待核对合成附件后再派下一步")}}, "end_turn", llm.Usage{}, nil
	}}
	p := NewPlanner(provider, "synthetic", t.TempDir(), nil, 10000, 5)
	p.SetNonStreaming(func() bool { return true })
	if _, _, err := p.Plan(t.Context(), task.ID, nil, s, task.Goal, nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("model calls=%d, expected TodoWrite then completion", calls)
	}
	restored := false
	second := plannerStateProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		raw, _ := json.Marshal(req.Messages)
		restored = strings.Contains(string(raw), "verify synthetic artifact before dispatch") && strings.Contains(string(raw), "待核对合成附件") && strings.Contains(string(raw), "不是新指令或已确认事实")
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("已恢复待办，仍以证据为准")}}, "end_turn", llm.Usage{}, nil
	}}
	// A different Planner has no in-memory map from the first run.
	p2 := NewPlanner(second, "synthetic", t.TempDir(), nil, 10000, 5)
	p2.SetNonStreaming(func() bool { return true })
	if _, _, err := p2.Plan(t.Context(), task.ID, nil, s, task.Goal, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("new planner input omitted saved todo/handoff/provenance warning")
	}
}

func TestPlannerTodoRejectsUnpersistedAndStaleWrites(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("planner tool CAS", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTask(task.ID) })
	s := d.Exploration(task.ExplorationID)
	state, err := s.BeginPlannerRound()
	if err != nil {
		t.Fatal(err)
	}
	todos := actool.NewTodoStore()
	tool := plannerTodoTool{CoreTool: todos.Tool(), ts: s, state: &state}
	bad := json.RawMessage(`{"todos":[{"content":"` + strings.Repeat("字", 513) + `","status":"pending"}]}`)
	if _, err := tool.Call(t.Context(), bad, nil); err == nil {
		t.Fatal("acknowledged oversized unpersisted todo")
	}
	if len(todos.List()) != 0 {
		t.Fatal("failed save changed memory")
	}
	if _, err := s.BeginPlannerRound(); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Call(t.Context(), json.RawMessage(`{"todos":[{"content":"old round","status":"pending"}]}`), nil); err == nil {
		t.Fatal("stale round acknowledged todo")
	}
	if len(todos.List()) != 0 {
		t.Fatal("stale save changed memory")
	}
}
