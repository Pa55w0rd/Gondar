package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/hook"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"gondar/db"
)

func scalarFixtureSchema() map[string]any {
	return map[string]any{"type": "object", "required": []string{"count", "enabled"}, "properties": map[string]any{"count": map[string]any{"type": "integer"}, "enabled": map[string]any{"type": "boolean"}, "payload": map[string]any{"type": "string"}}}
}

func TestSchemaScalarCompatibilityConservativeAndExact(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		schema      map[string]any
		want        string
	}{
		{"precise", `{"count":"9007199254740993","enabled":"true","payload":"001 credential"}`, scalarFixtureSchema(), `{"count":9007199254740993,"enabled":true,"payload":"001 credential"}`},
		{"already-valid", `{"count":1,"enabled":false}`, scalarFixtureSchema(), ""},
		{"truncated", `{"count":"1","enabled":"true"`, scalarFixtureSchema(), ""},
		{"trailing-json", `{"count":"1","enabled":"true"}{}`, scalarFixtureSchema(), ""},
		{"integer-leading-zero", `{"count":"01","enabled":"true"}`, scalarFixtureSchema(), ""},
		{"boolean-case", `{"count":"1","enabled":"TRUE"}`, scalarFixtureSchema(), ""},
		{"missing-required", `{"count":"1"}`, scalarFixtureSchema(), ""},
		{"array-no-heuristic", `["1"]`, map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, ""},
		{"anyof-unique", `"3"`, map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "boolean"}}}, `3`},
		{"anyof-ambiguous", `"3"`, map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "number"}}}, ""},
		{"unknown-constraint", `"3"`, map[string]any{"type": "integer", "minimum": 5}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := normalizeSchemaScalars(tc.schema, []byte(tc.input))
			if string(got) != tc.want {
				t.Fatalf("修正=%s 期望=%s", got, tc.want)
			}
		})
	}
}

func TestScalarCompatibilityRealSDKUsesSameEffectiveInputAndKeepsOriginal(t *testing.T) {
	original := `{"count":"9007199254740993","enabled":"true","payload":"do not parse 001"}`
	effective := `{"count":9007199254740993,"enabled":true,"payload":"do not parse 001"}`
	seen := make(chan string, 4)
	probe := tool.Build(tool.Spec{Name: "scalar_probe", Schema: scalarFixtureSchema(), Permissions: func(_ context.Context, in json.RawMessage, _ permission.Context) permission.Decision {
		seen <- string(in)
		return permission.Allowed()
	}, Run: func(_ context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		seen <- string(in)
		return tool.Text("synthetic receipt"), nil
	}})
	hooks := hook.NewRegistry().On(hook.PreToolUse, func(_ context.Context, ev hook.Event) hook.Result { seen <- string(ev.Input); return hook.Result{} })
	calls := 0
	provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		calls++
		events := []llm.StreamEvent{{Type: llm.SETextDelta, Text: "done"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}}
		if calls == 1 {
			events = []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: "same", ToolName: "scalar_probe"}, {Type: llm.SEToolInputJSON, Text: original}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}}
		}
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}}
	var rows []db.Activity
	_, _, err := captureRun(WithExecutionRunID(t.Context(), "host"), agentcore.Options{Provider: provider, Tools: []tool.CoreTool{probe}, Hooks: hooks, PermissionMode: permission.ModeAuto, MaxTurns: 3}, "synthetic", func(a db.Activity) { rows = append(rows, a) })
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		select {
		case input := <-seen:
			if input != effective {
				t.Fatalf("权限/hook/Call有效参数不同 %s", input)
			}
		default:
			t.Fatal("未走真实SDK调用")
		}
	}
	for _, row := range rows {
		if row.Kind == "tool_use" && row.Detail != original {
			t.Fatal("原始调用历史被修改")
		}
		if row.Kind == "tool_result" {
			out, ok := ToolOutcomeFromMetadata(row.Metadata)
			if !ok || out.OriginalInputDigest == out.EffectiveInputDigest || len(out.CorrectionPaths) != 2 || out.CorrectionPaths[0] != "/count" || out.CorrectionPaths[1] != "/enabled" {
				t.Fatalf("修正出处未记录 %+v", out)
			}
		}
	}
}
