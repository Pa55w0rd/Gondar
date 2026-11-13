package guard

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/norma/toolcall"
	"gondar/db"
	"gondar/intercept"
)

func TestAuditHasOnlyFinalGuardDecisionAndInvocation(t *testing.T) {
	for _, action := range []string{"allow", "deny"} {
		t.Run(action, func(t *testing.T) {
			ic := intercept.New(nil)
			ic.SetRulesInMemory([]db.InterceptRule{e2eRule(17, "synthetic", `synthetic`, action, 100, false, 1, "deny")})
			g := NewWithInterceptor(ic)
			id := toolcall.Identity{Version: 1, RunID: "host", PromptID: "prompt", InvocationID: "unique", ToolUseID: "reused", ToolName: "Bash"}
			call := toolcall.New(id)
			ctx := toolcall.WithInvocation(context.Background(), call)
			blocked, _, _ := g.Hooks().PreToolUse(ctx, "Bash", []byte(`{"command":"synthetic"}`))
			entries := g.Audit()
			want := "allow"
			if action == "deny" {
				want = "block"
			}
			if len(entries) != 1 || entries[0].Action != want || entries[0].RunID != id.RunID || entries[0].InvocationID != id.InvocationID || blocked != (action == "deny") {
				t.Fatalf("审计未记录唯一最终裁决 %+v", entries)
			}
		})
	}
}

func TestAuditAskFinalAllowRejectAndTimeoutHaveOneEntry(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("需要专用测试数据库")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, action := range []string{"allow", "deny", "timeout"} {
		t.Run(action, func(t *testing.T) {
			ic := intercept.New(d)
			_ = ic.IsToolEnabled("Bash") // 先加载独立的工具开关，随后仅覆盖测试规则。
			ic.SetRulesInMemory([]db.InterceptRule{e2eRule(0, "合成审批", `synthetic`, "ask", 100, true, 1, "deny")})
			g := NewWithInterceptor(ic)
			identity := toolcall.Identity{Version: 1, RunID: "host", PromptID: "prompt", InvocationID: toolcall.FreshID(), ToolUseID: "reused", ToolName: "Bash"}
			call := toolcall.New(identity)
			ctx := toolcall.WithInvocation(t.Context(), call)
			ctx = intercept.WithTaskContext(ctx, "audit-final-fixture", "fixture", func(a db.Activity) {
				var v struct {
					ID int64 `json:"pending_id"`
				}
				if err := json.Unmarshal([]byte(a.Detail), &v); err != nil {
					t.Error(err)
				}
				t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM intercept_pending WHERE id=$1`, v.ID) })
				if action != "timeout" {
					if err := ic.Decide(v.ID, action == "allow"); err != nil {
						t.Error(err)
					}
				}
			})
			blocked, _, _ := g.Hooks().PreToolUse(ctx, "Bash", []byte(`{"command":"synthetic"}`))
			entries := g.Audit()
			want := "allow"
			if action != "allow" {
				want = "block"
			}
			if len(entries) != 1 || entries[0].Action != want || blocked != (action != "allow") {
				t.Fatalf("审批产生多条或提前allow审计 %+v", entries)
			}
			if blocked {
				out := call.Result(true)
				kind := toolcall.ApprovalRejected
				if action == "timeout" {
					kind = toolcall.ApprovalTimeout
				}
				if out.Kind != kind || out.ExecutionState != toolcall.NotExecuted {
					t.Fatalf("审批分类不精确 %+v", out)
				}
			}
		})
	}
}
