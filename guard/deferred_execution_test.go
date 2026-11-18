package guard

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/toolcall"
	"gondar/db"
	"gondar/intercept"
)

type deferredGovernanceProvider struct {
	inputs []string
	turn   int
}

func (p *deferredGovernanceProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if p.turn < len(p.inputs) {
		input := p.inputs[p.turn]
		p.turn++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "provider-reused", Name: actool.ExecuteExtraToolName, Input: json.RawMessage(input)}}}, "tool_use", llm.Usage{}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("本地合成调用结束")}}, "end_turn", llm.Usage{}, nil
}

func (*deferredGovernanceProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{}, errors.New("test uses non-streaming"))
	}
}

// This Bash is a pure in-memory fixture. It never starts a shell or target request.
func deferredGovernanceTool(t *testing.T, calls *atomic.Int32) actool.CoreTool {
	t.Helper()
	return actool.Build(actool.Spec{Name: "Bash", Schema: map[string]any{"type": "object", "required": []any{"command"}, "properties": map[string]any{"command": map[string]any{"type": "string"}}},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, input json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			calls.Add(1)
			id, ok := toolcall.IdentityFrom(ctx)
			if !ok || !id.Valid() || id.ToolName != actool.ExecuteExtraToolName || id.ToolUseID != "provider-reused" {
				t.Error("inner operation lost outer host invocation identity")
			}
			if strings.Contains(string(input), "failure") {
				return actool.Errorf("synthetic inner execution failure"), nil
			}
			return actool.Text("synthetic raw operation receipt"), nil
		}})
}

func runDeferredGovernance(t *testing.T, ctx context.Context, guard *Guard, tools []actool.CoreTool, unlock *actool.UnlockSet, inputs ...string) []toolcall.Outcome {
	t.Helper()
	ctx, trace := intercept.WithTrace(toolcall.WithRunID(ctx, "host-deferred-run"), "本地合成治理回归", nil)
	defer trace.Finish()
	session := agentcore.NewSession(agentcore.Options{Provider: &deferredGovernanceProvider{inputs: inputs}, Tools: tools,
		DeferredTools: []string{"Bash"}, UnlockSet: unlock, Hooks: guard.Hooks(), PermissionMode: permission.ModeBypass,
		NonStreaming: true, DisableBackgroundTasks: true, WorkingDir: t.TempDir(), MaxTurns: 10})
	defer session.Close()
	var outcomes []toolcall.Outcome
	for event, err := range session.Prompt(ctx, "只运行本地合成工具") {
		if err != nil {
			if ctx.Err() == nil {
				t.Fatal(err)
			}
			continue
		}
		if event.Kind == harness.KindToolUse && event.ToolCall != nil {
			trace.StartInvocation(*event.ToolCall, event.ToolUse.Name, event.ToolUse.Input)
		}
		if event.Kind == harness.KindToolResult && event.ToolOutcome != nil {
			outcomes = append(outcomes, *event.ToolOutcome)
			if event.ToolResult.ToolUseID != "provider-reused" {
				t.Fatal("result no longer pairs with the provider's outer call")
			}
			var output strings.Builder
			for _, block := range event.ToolResult.Content {
				output.WriteString(block.Text)
			}
			trace.CompleteOutcome(event.ToolOutcome.Key(), output.String(), event.ToolResult.IsError, event.ToolOutcome)
		}
	}
	return outcomes
}

func TestDeferredSDKUsesActualGuardRulesAndSeparatesRepeatedOuterCalls(t *testing.T) {
	interceptor := intercept.New(nil)
	interceptor.SetRulesInMemory([]db.InterceptRule{e2eRule(1, "合成内层拒绝", "deny", "deny", 100, false, 1, "deny")})
	guard := NewWithInterceptor(interceptor)
	var calls atomic.Int32
	outcomes := runDeferredGovernance(t, t.Context(), guard, []actool.CoreTool{deferredGovernanceTool(t, &calls)}, actool.NewUnlockSet("Bash"),
		`{"tool_name":"Bash","params":{"command":"deny"}}`,
		`{"tool_name":"Bash","params":{"command":"failure"}}`)
	if len(outcomes) != 2 || calls.Load() != 1 || outcomes[0].Kind != toolcall.PolicyDenied || outcomes[0].ExecutionState != toolcall.NotExecuted || outcomes[1].Kind != toolcall.ExecutionError || outcomes[1].ExecutionState != toolcall.Executed || outcomes[0].InvocationID == outcomes[1].InvocationID {
		t.Fatalf("inner governance/receipt association failed: calls=%d %+v", calls.Load(), outcomes)
	}
	audit := guard.Audit()
	if len(audit) != 2 || audit[0].Tool != "Bash" || audit[0].Command != "deny" || audit[0].Action != "block" || audit[1].Action != "allow" || audit[1].Tool != "Bash" || audit[1].Command != "failure" {
		t.Fatalf("audit still describes wrapper: %+v", audit)
	}
	for i := range outcomes {
		if audit[i].InvocationID != outcomes[i].InvocationID || audit[i].RunID != outcomes[i].RunID || outcomes[i].ToolName != actool.ExecuteExtraToolName || outcomes[i].EffectiveToolName != "Bash" {
			t.Fatalf("inner audit lost trusted outer identity: %+v %+v", audit, outcomes)
		}
	}
}

func TestDeferredSDKUnavailableAndMalformedPayloadNeverReachInnerGuardOrTool(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"disabled", `{"tool_name":"Bash","params":{"command":"synthetic"}}`},
		{"locked", `{"tool_name":"Bash","params":{"command":"synthetic"}}`},
		{"malformed", `{"tool_name":"Bash","params":null}`},
		{"forged host id", `{"tool_name":"Bash","params":{"command":"synthetic"},"run_id":"forged"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			tools := []actool.CoreTool{deferredGovernanceTool(t, &calls)}
			unlock := actool.NewUnlockSet("Bash")
			if tc.name == "disabled" {
				tools = []actool.CoreTool{} // host filtered the disabled tool out
			}
			if tc.name == "locked" {
				unlock = actool.NewUnlockSet()
			}
			guard := New()
			outcomes := runDeferredGovernance(t, t.Context(), guard, tools, unlock, tc.input)
			if calls.Load() != 0 || len(outcomes) != 1 || outcomes[0].ExecutionState != toolcall.NotExecuted || len(guard.Audit()) != 0 {
				t.Fatalf("invalid deferred dispatch executed or fell back: calls=%d outcomes=%+v audit=%+v", calls.Load(), outcomes, guard.Audit())
			}
		})
	}
}

func TestDeferredSDKJudgeReceivesActualToolAndParameters(t *testing.T) {
	for _, action := range []string{"allow", "deny"} {
		t.Run(action, func(t *testing.T) {
			interceptor := intercept.New(nil)
			interceptor.SetRulesInMemory([]db.InterceptRule{e2eRule(1, "合成内层裁判", "synthetic", "judge", 100, true, 1, "deny")})
			judgements := 0
			interceptor.SetReviewer(func(ctx context.Context, _ int64, _ string, input intercept.ReviewInput) (intercept.Decision, error) {
				judgements++
				var args map[string]any
				_ = json.Unmarshal(input.Arguments, &args)
				id, ok := toolcall.IdentityFrom(ctx)
				if input.Tool != "Bash" || args["command"] != "synthetic" || !ok || id.ToolName != actool.ExecuteExtraToolName {
					t.Errorf("judge saw wrapper or lost outer identity: input=%+v id=%+v", input, id)
				}
				return intercept.Decision{Action: action, Message: "合成裁决"}, nil
			})
			var calls atomic.Int32
			outcomes := runDeferredGovernance(t, t.Context(), NewWithInterceptor(interceptor), []actool.CoreTool{deferredGovernanceTool(t, &calls)}, actool.NewUnlockSet("Bash"), `{"tool_name":"Bash","params":{"command":"synthetic"}}`)
			want := int32(0)
			if action == "allow" {
				want = 1
			}
			if judgements != 1 || calls.Load() != want || len(outcomes) != 1 {
				t.Fatalf("judge not applied to actual inner operation: judgements=%d calls=%d outcomes=%+v", judgements, calls.Load(), outcomes)
			}
		})
	}
}

func TestDeferredSDKActualApprovalAndAuditPersistence(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("需要本轮专用测试数据库")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	for _, action := range []string{"allow", "deny", "pause"} {
		t.Run(action, func(t *testing.T) {
			interceptor := intercept.New(pg)
			_ = interceptor.IsToolEnabled("Bash")
			interceptor.SetRulesInMemory([]db.InterceptRule{e2eRule(0, "合成内层审批", "synthetic", "ask", 100, false, 1, "deny")})
			guard := NewWithInterceptor(interceptor)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var pending int64
			ctx = intercept.WithTaskContext(ctx, "", "deferred-fixture", func(activity db.Activity) {
				var v struct {
					ID   int64  `json:"pending_id"`
					Tool string `json:"tool"`
				}
				if json.Unmarshal([]byte(activity.Detail), &v) != nil || v.Tool != "Bash" || v.ID <= 0 {
					t.Error("approval did not describe the actual inner operation")
				}
				pending = v.ID
				if action == "pause" {
					cancel()
				} else if err := interceptor.Decide(v.ID, action == "allow"); err != nil {
					t.Error(err)
				}
			})
			var calls atomic.Int32
			outcomes := runDeferredGovernance(t, ctx, guard, []actool.CoreTool{deferredGovernanceTool(t, &calls)}, actool.NewUnlockSet("Bash"), `{"tool_name":"Bash","params":{"command":"synthetic"}}`)
			if pending > 0 {
				t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM intercept_pending WHERE id=$1`, pending) })
			}
			want := int32(0)
			state, kind := toolcall.NotExecuted, toolcall.ApprovalRejected
			if action == "allow" {
				want, state, kind = 1, toolcall.Executed, toolcall.ExecutedSuccess
			} else if action == "pause" {
				kind = toolcall.Cancelled
			}
			if pending <= 0 || len(outcomes) != 1 || calls.Load() != want || outcomes[0].ExecutionState != state || outcomes[0].Kind != kind {
				t.Fatalf("approval/pausing executed unexpected operation: pending=%d calls=%d outcomes=%+v", pending, calls.Load(), outcomes)
			}
			detail, err := pg.GetInterceptDetail(pending)
			if err != nil || detail == nil || detail.ToolName != "Bash" || detail.Audit == nil || detail.Audit.InvocationID != outcomes[0].InvocationID || detail.Audit.RunID != outcomes[0].RunID || detail.Audit.Correlation != "exact" {
				t.Fatalf("stored inner audit lost outer invocation association: %+v %v", detail, err)
			}
		})
	}
}
