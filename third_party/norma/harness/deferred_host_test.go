package harness

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/norma/hook"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/toolcall"
)

type deniedDeferredFixture struct{ tool.CoreTool }

func (deniedDeferredFixture) CheckPermissions(context.Context, json.RawMessage, permission.Context) permission.Decision {
	return permission.Denied("synthetic inner hard denial")
}

func TestHostDeferredChecksActualToolBeforeStartingExecution(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        toolcall.Kind
		executed    bool
	}{
		{"allowed", `{"tool_name":"probe","params":{"n":1}}`, toolcall.ExecutedSuccess, true},
		{"inner failure", `{"tool_name":"probe","params":{"n":1}}`, toolcall.ExecutionError, true},
		{"inner permission", `{"tool_name":"probe","params":{"n":1}}`, toolcall.PermissionDenied, false},
		{"inner hard denial", `{"tool_name":"probe","params":{"n":1}}`, toolcall.PermissionDenied, false},
		{"outer permission", `{"tool_name":"probe","params":{"n":1}}`, toolcall.PermissionDenied, false},
		{"inner hook", `{"tool_name":"probe","params":{"n":1}}`, toolcall.PolicyDenied, false},
		{"locked", `{"tool_name":"probe","params":{"n":1}}`, toolcall.PermissionDenied, false},
		{"disabled", `{"tool_name":"probe","params":{"n":1}}`, toolcall.UnknownTool, false},
		{"inner schema", `{"tool_name":"probe","params":{"n":"bad"}}`, toolcall.SchemaInvalid, false},
		{"unknown", `{"tool_name":"absent","params":{}}`, toolcall.UnknownTool, false},
		{"nested", `{"tool_name":"ExecuteExtraTool","params":{"tool_name":"probe","params":{"n":1}}}`, toolcall.SchemaInvalid, false},
		{"null params", `{"tool_name":"probe","params":null}`, toolcall.SchemaInvalid, false},
		{"duplicate selector", `{"tool_name":"probe","tool_name":"absent","params":{"n":1}}`, toolcall.SchemaInvalid, false},
		{"forged identity", `{"tool_name":"probe","params":{"n":1},"invocation_id":"model-forged"}`, toolcall.SchemaInvalid, false},
		{"ordinary args cannot forge identity", `{"tool_name":"probe","params":{"n":1,"run_id":"model-forged","prompt_id":"model-forged","invocation_id":"model-forged"}}`, toolcall.ExecutedSuccess, true},
		{"malformed", `{"tool_name":"probe","params":`, toolcall.SchemaInvalid, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, pre, post := 0, 0, 0
			identity := toolcall.Identity{Version: 1, RunID: "host-run", PromptID: "host-prompt", InvocationID: toolcall.FreshID(), ToolUseID: "provider-outer", ToolName: tool.ExecuteExtraToolName}
			probe := outcomeProbe(func(ctx context.Context) (tool.Result, error) {
				calls++
				if got, ok := toolcall.IdentityFrom(ctx); !ok || got != identity {
					t.Errorf("inner execution lost immutable outer identity: %+v", got)
				}
				if tc.name == "inner failure" {
					return tool.Errorf("actual inner failure"), nil
				}
				return tool.Text("actual inner success"), nil
			})
			registry := tool.NewRegistry()
			if tc.name != "disabled" {
				if tc.name == "inner hard denial" {
					registry.Add(deniedDeferredFixture{CoreTool: probe})
				} else {
					registry.Add(probe)
				}
			}
			unlock := tool.NewUnlockSet("probe")
			if tc.name == "locked" {
				unlock = tool.NewUnlockSet()
			}
			registry.Add(tool.NewExecuteExtraTool(registry, unlock))
			hooks := hook.NewRegistry().On(hook.PreToolUse, func(ctx context.Context, ev hook.Event) hook.Result {
				pre++
				var parameters map[string]any
				_ = json.Unmarshal(ev.Input, &parameters)
				if got, ok := toolcall.IdentityFrom(ctx); !ok || got != identity || ev.ToolName != "probe" || parameters["n"] != float64(1) {
					t.Errorf("hook did not see actual tool/params with outer identity: %+v %s %s", got, ev.ToolName, ev.Input)
				}
				if tc.name == "inner hook" {
					toolcall.Mark(ctx, toolcall.PolicyDenied, toolcall.NotExecuted, "policy", "synthetic_rule", 1)
					return hook.Result{Decision: "block", Message: "synthetic deny"}
				}
				return hook.Result{}
			}).On(hook.PostToolUse, func(_ context.Context, ev hook.Event) hook.Result {
				post++
				if ev.ToolName != "probe" {
					t.Errorf("post hook saw wrapper %s", ev.ToolName)
				}
				return hook.Result{}
			})
			input := QueryInput{Tools: registry, PermissionMode: permission.ModeBypass, Hooks: hooks}
			if tc.name == "inner permission" {
				input.Disallowed = []string{"probe"}
			}
			if tc.name == "outer permission" {
				input.Disallowed = []string{tool.ExecuteExtraToolName}
			}
			loop := &loop{ctx: t.Context(), in: input}
			result, _, outcome := loop.execOneInvocation(llm.ContentBlock{Type: llm.BlockToolUse, ID: identity.ToolUseID, Name: identity.ToolName, Input: json.RawMessage(tc.input)}, nil, toolcall.New(identity))
			wantCalls := 0
			state := toolcall.NotExecuted
			if tc.executed {
				wantCalls, state = 1, toolcall.Executed
			}
			if calls != wantCalls || post != wantCalls || outcome.Kind != tc.want || outcome.ExecutionState != state || outcome.Identity != identity || result.ToolUseID != identity.ToolUseID {
				t.Fatalf("deferred identity/decision mismatch: calls=%d pre=%d post=%d result=%+v outcome=%+v", calls, pre, post, result, outcome)
			}
			if tc.executed && outcome.EffectiveToolName != "probe" {
				t.Fatal("actual registry tool missing from host-only outcome")
			}
		})
	}
}

func TestHostDeferredNormalizesInnerSchemaAndPreservesProviderInput(t *testing.T) {
	var seen json.RawMessage
	probe := tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object", "required": []any{"n"}, "properties": map[string]any{"n": map[string]any{"type": "integer"}}},
		Run: func(ctx context.Context, input json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			seen = append(json.RawMessage(nil), input...)
			if id, ok := toolcall.IdentityFrom(ctx); !ok || id.ToolName != tool.ExecuteExtraToolName {
				t.Error("provider identity rewritten to inner name")
			}
			return tool.Text("ok"), nil
		}})
	registry := tool.NewRegistry(probe)
	registry.Add(tool.NewExecuteExtraTool(registry, tool.NewUnlockSet("probe")))
	repairs := 0
	ctx := WithInputNormalizer(t.Context(), func(_ context.Context, name string, schema map[string]any, input json.RawMessage) (json.RawMessage, []string) {
		repairs++
		if name != "probe" || string(input) != `{"n":"1"}` || schema["properties"] == nil {
			t.Error("normalization did not use inner tool contract")
		}
		return json.RawMessage(`{"n":1}`), []string{"/n"}
	})
	raw := json.RawMessage(`{"tool_name":"probe","params":{"n":"1"}}`)
	id := toolcall.Identity{Version: 1, RunID: "run", PromptID: "prompt", InvocationID: "host", ToolUseID: "provider", ToolName: tool.ExecuteExtraToolName}
	loop := &loop{ctx: ctx, in: QueryInput{Tools: registry, PermissionMode: permission.ModeBypass}}
	result, _, outcome := loop.execOneInvocation(llm.ContentBlock{Type: llm.BlockToolUse, ID: id.ToolUseID, Name: id.ToolName, Input: raw}, nil, toolcall.New(id))
	if result.IsError || repairs != 1 || string(seen) != `{"n":1}` || string(raw) != `{"tool_name":"probe","params":{"n":"1"}}` || outcome.OriginalInputDigest != inputDigest(raw) || outcome.EffectiveInputDigest != inputDigest(seen) || len(outcome.CorrectionPaths) != 1 {
		t.Fatalf("inner normalization lost input/identity facts: repairs=%d input=%s outcome=%+v", repairs, seen, outcome)
	}
}

func TestHostDeferredReusedProviderIDKeepsOneOuterReceiptPerInvocation(t *testing.T) {
	probe := outcomeProbe(func(context.Context) (tool.Result, error) { return tool.Text("inner success"), nil })
	registry := tool.NewRegistry(probe)
	registry.Add(tool.NewExecuteExtraTool(registry, tool.NewUnlockSet("probe")))
	model := &scriptedModel{turns: [][]llm.StreamEvent{
		toolTurn("reused", tool.ExecuteExtraToolName, `{"tool_name":"probe","params":{"n":1}}`),
		toolTurn("reused", tool.ExecuteExtraToolName, `{"tool_name":"probe","params":{"n":1}}`), textTurn("done"),
	}}
	var identities []toolcall.Identity
	var results []toolcall.Outcome
	for event, err := range Query(t.Context(), QueryInput{Tools: registry, PermissionMode: permission.ModeBypass}, QueryDeps{CallModel: model.call}) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == KindToolUse {
			identities = append(identities, *event.ToolCall)
		}
		if event.ToolOutcome != nil {
			results = append(results, *event.ToolOutcome)
		}
	}
	if len(identities) != 2 || len(results) != 2 || identities[0].InvocationID == identities[1].InvocationID {
		t.Fatalf("outer calls or receipts duplicated/mixed: %+v %+v", identities, results)
	}
	for i := range identities {
		if identities[i].ToolName != tool.ExecuteExtraToolName || results[i].Identity != identities[i] || results[i].Kind != toolcall.ExecutedSuccess {
			t.Fatalf("provider result pair lost: %+v %+v", identities, results)
		}
	}
}
