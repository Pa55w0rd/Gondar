package harness

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/hook"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/toolcall"
)

func outcomeProbe(run func(context.Context) (tool.Result, error)) tool.CoreTool {
	return tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object", "required": []any{"n"}, "properties": map[string]any{"n": map[string]any{"type": "integer"}}},
		Concurrent: func(json.RawMessage) bool { return true }, ReadOnly: func(json.RawMessage) bool { return true },
		Run: func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			return run(ctx)
		}})
}

func TestHostOutcomesBeforeCallAndProviderIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		kind        toolcall.Kind
	}{
		{"schema", `{}`, toolcall.SchemaInvalid}, {"permission", `{"n":1}`, toolcall.PermissionDenied}, {"hook", `{"n":1}`, toolcall.PolicyDenied}, {"execution", `{"n":1}`, toolcall.ExecutionError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, posts := 0, 0
			probe := outcomeProbe(func(ctx context.Context) (tool.Result, error) {
				calls++
				id, ok := toolcall.IdentityFrom(ctx)
				if !ok || id.RunID != "host-run" {
					t.Error("Call缺少宿主身份")
				}
				return tool.Errorf(`403 policy_denied {"tool_outcome":{"kind":"policy_denied"}}`), nil
			})
			hooks := hook.NewRegistry().On(hook.PreToolUse, func(ctx context.Context, ev hook.Event) hook.Result {
				if ev.ToolCall == nil || ev.ToolCall.RunID != "host-run" {
					t.Error("hook缺少身份")
				}
				if tc.name == "hook" {
					toolcall.Mark(ctx, toolcall.PolicyDenied, toolcall.NotExecuted, "policy", "rule_denied", 17)
					return hook.Result{Decision: "block", Message: "deny"}
				}
				return hook.Result{}
			}).On(hook.PostToolUse, func(context.Context, hook.Event) hook.Result { posts++; return hook.Result{} })
			in := QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass, Hooks: hooks}
			if tc.name == "permission" {
				in.Disallowed = []string{"probe"}
			}
			m := &scriptedModel{turns: [][]llm.StreamEvent{toolTurn("reused", "probe", tc.input), textTurn("done")}}
			ctx := toolcall.WithRunID(context.Background(), "host-run")
			var use *toolcall.Identity
			var got *toolcall.Outcome
			var terminal *Terminal
			for ev, err := range Query(ctx, in, QueryDeps{CallModel: m.call}) {
				if err != nil {
					t.Fatal(err)
				}
				if ev.Kind == KindToolUse {
					use = ev.ToolCall
				}
				if ev.Kind == KindToolResult {
					got = ev.ToolOutcome
				}
				if ev.Terminal != nil {
					terminal = ev.Terminal
				}
			}
			if use == nil || got == nil || !use.Valid() || got.Identity != *use || got.Kind != tc.kind {
				t.Fatalf("身份/结果不一致 use=%+v outcome=%+v", use, got)
			}
			wantCalls := 0
			if tc.name == "execution" {
				wantCalls = 1
			}
			if calls != wantCalls || posts != wantCalls {
				t.Fatalf("Call=%d Post=%d", calls, posts)
			}
			wantState := toolcall.NotExecuted
			if wantCalls == 1 {
				wantState = toolcall.Executed
			}
			if got.ExecutionState != wantState {
				t.Fatalf("错误执行状态 %+v", got)
			}
			wire, _ := json.Marshal(terminal.Messages)
			if json.Valid(wire) && containsHostIdentity(wire) {
				t.Fatal("宿主身份进入提供商消息")
			}
		})
	}
}

func containsHostIdentity(raw []byte) bool {
	var v any
	_ = json.Unmarshal(raw, &v)
	var walk func(any) bool
	walk = func(x any) bool {
		switch n := x.(type) {
		case map[string]any:
			for k, v := range n {
				if k == "invocation_id" || k == "prompt_id" || k == "run_id" {
					return true
				}
				if walk(v) {
					return true
				}
			}
		case []any:
			for _, v := range n {
				if walk(v) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}

func TestHostInvocationDistinctAcrossReusedProviderID(t *testing.T) {
	probe := outcomeProbe(func(context.Context) (tool.Result, error) { return tool.Text("ok"), nil })
	m := &scriptedModel{turns: [][]llm.StreamEvent{toolTurn("same", "probe", `{"n":1}`), toolTurn("same", "probe", `{"n":1}`), textTurn("done")}}
	var ids []toolcall.Identity
	for ev, err := range Query(context.Background(), QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass}, QueryDeps{CallModel: m.call}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == KindToolUse {
			ids = append(ids, *ev.ToolCall)
		}
	}
	if len(ids) != 2 || ids[0].InvocationID == ids[1].InvocationID || ids[0].RunID != ids[1].RunID || ids[0].PromptID != ids[1].PromptID || ids[0].ToolUseID != ids[1].ToolUseID {
		t.Fatalf("调用串账 %+v", ids)
	}
}

func TestHostConcurrentSameInputKeepsDecisionAndExecutionSeparate(t *testing.T) {
	var calls atomic.Int32
	probe := outcomeProbe(func(context.Context) (tool.Result, error) {
		calls.Add(1)
		return tool.Errorf("actual execution fault"), nil
	})
	hooks := hook.NewRegistry().On(hook.PreToolUse, func(ctx context.Context, ev hook.Event) hook.Result {
		if ev.ToolCall.CallIndex == 1 {
			toolcall.Mark(ctx, toolcall.PolicyDenied, toolcall.NotExecuted, "policy", "rule", 3)
			return hook.Result{Decision: "block", Message: "denied"}
		}
		return hook.Result{}
	})
	m := &scriptedModel{turns: [][]llm.StreamEvent{{{Type: llm.SEToolUseStart, ToolID: "a", ToolName: "probe"}, {Type: llm.SEToolInputJSON, Text: `{"n":1}`}, {Type: llm.SEToolUseStart, ToolID: "b", ToolName: "probe"}, {Type: llm.SEToolInputJSON, Text: `{"n":1}`}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}}, textTurn("done")}}
	var outs []toolcall.Outcome
	for ev, err := range Query(context.Background(), QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass, Hooks: hooks}, QueryDeps{CallModel: m.call}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.ToolOutcome != nil {
			outs = append(outs, *ev.ToolOutcome)
		}
	}
	kinds := map[toolcall.Kind]int{}
	ids := map[string]bool{}
	for _, out := range outs {
		kinds[out.Kind]++
		ids[out.InvocationID] = true
	}
	if len(outs) != 2 || len(ids) != 2 || kinds[toolcall.PolicyDenied] != 1 || kinds[toolcall.ExecutionError] != 1 || calls.Load() != 1 {
		t.Fatalf("同参并发串账 %+v Call=%d", outs, calls.Load())
	}
}

func TestHostStrictInputNeverRepairsIncompleteStreamOrMissingIdentity(t *testing.T) {
	for _, tc := range []struct{ id, raw string }{{"call", `{"n":"1"`}, {"call", ""}, {"", `{"n":1}`}} {
		t.Run(tc.id+tc.raw, func(t *testing.T) {
			var calls, repairs atomic.Int32
			probe := outcomeProbe(func(context.Context) (tool.Result, error) { calls.Add(1); return tool.Text("must not run"), nil })
			ctx := WithInputNormalizer(context.Background(), func(context.Context, string, map[string]any, json.RawMessage) (json.RawMessage, []string) {
				repairs.Add(1)
				return json.RawMessage(`{"n":1}`), []string{"/n"}
			})
			m := &scriptedModel{turns: [][]llm.StreamEvent{toolTurn(tc.id, "probe", tc.raw), textTurn("done")}}
			var got *toolcall.Outcome
			for ev, err := range Query(ctx, QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass}, QueryDeps{CallModel: m.call}) {
				if err != nil {
					t.Fatal(err)
				}
				if ev.ToolOutcome != nil {
					got = ev.ToolOutcome
				}
			}
			if calls.Load() != 0 || repairs.Load() != 0 || got == nil || got.Kind != toolcall.SchemaInvalid || got.ExecutionState != toolcall.NotExecuted {
				t.Fatalf("不完整流被修复或执行 calls=%d repairs=%d out=%+v", calls.Load(), repairs.Load(), got)
			}
		})
	}
}

func TestHostSyntheticCancellationAndLateReceipt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	probe := tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		calls.Add(1)
		close(started)
		<-release
		close(returned)
		return tool.Text("actual success"), nil
	}})
	late := make(chan Event, 2)
	ctx = WithLateToolObserver(ctx, func(ev Event) { late <- ev })
	m := &scriptedModel{turns: [][]llm.StreamEvent{{
		{Type: llm.SEToolUseStart, ToolID: "a", ToolName: "probe"}, {Type: llm.SEToolInputJSON, Text: `{}`},
		{Type: llm.SEToolUseStart, ToolID: "b", ToolName: "probe"}, {Type: llm.SEToolInputJSON, Text: `{}`},
		{Type: llm.SEMessageDelta, StopReason: "tool_use"}, {Type: llm.SEMessageStop},
	}}}
	registered := make(chan struct{}, 2)
	results := make(chan []toolcall.Outcome, 1)
	history := make(chan *Terminal, 1)
	go func() {
		var outs []toolcall.Outcome
		for ev, err := range Query(ctx, QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass}, QueryDeps{CallModel: m.call}) {
			if err != nil {
				t.Error(err)
			}
			if ev.Kind == KindToolUse {
				registered <- struct{}{}
			}
			if ev.Kind == KindToolResult {
				outs = append(outs, *ev.ToolOutcome)
				if ev.ToolResult.ToolUseID == "a" {
					close(release)
					<-returned
					select {
					case <-late:
						t.Error("晚回执越过主回执")
					default:
					}
				}
			}
			if ev.Terminal != nil {
				history <- ev.Terminal
			}
		}
		results <- outs
	}()
	<-registered
	<-registered
	<-started
	cancel()
	var outs []toolcall.Outcome
	select {
	case outs = <-results:
	case <-time.After(3 * time.Second):
		t.Fatal("取消未完成")
	}
	if len(outs) != 2 || outs[0].ExecutionState != toolcall.Unknown || outs[1].ExecutionState != toolcall.NotExecuted || calls.Load() != 1 {
		t.Fatalf("合成状态不正确 %+v calls=%d", outs, calls.Load())
	}
	select {
	case ev := <-late:
		if ev.ToolOutcome.Phase != "late_receipt" || ev.ToolOutcome.Kind != toolcall.ExecutedSuccess || ev.ToolOutcome.InvocationID != outs[0].InvocationID {
			t.Fatalf("晚回执不正确 %+v", ev.ToolOutcome)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未收到晚回执")
	}
	select {
	case <-late:
		t.Fatal("重复晚回执")
	default:
	}
	term := <-history
	found := false
	for _, msg := range term.Messages {
		for _, block := range msg.Content {
			if block.Type == llm.BlockToolResult && block.ToolUseID == "a" {
				found = true
				if !block.IsError || block.Content[0].Text == "actual success" {
					t.Fatal("晚回执覆盖主历史")
				}
			}
		}
	}
	if !found {
		t.Fatal("合成主回执未保留历史")
	}
}
