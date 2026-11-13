package agent

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Autumn-27/norma/toolcall"
)

func testToolOutcome(kind toolcall.Kind) *toolcall.Outcome {
	return &toolcall.Outcome{Identity: toolcall.Identity{Version: 1, RunID: "host", PromptID: "prompt", InvocationID: "invocation", ToolUseID: "provider", ToolName: "probe"}, Phase: "result", Kind: kind, ExecutionState: toolcall.NotExecuted, Source: "policy", ReasonCode: "rule", RuleID: 12}
}

func TestTypedToolBudgetsSeparatePolicyInputAndRealFault(t *testing.T) {
	var g toolFailureLoop
	g.planner = true
	for i := 0; i < 2; i++ {
		id := fmt.Sprint(i)
		g.start(id, "Bash", []byte(`{"command":"synthetic"}`))
		if err := g.resultWithOutcome(id, true, "403 backend failed", testToolOutcome(toolcall.PolicyDenied)); err != nil {
			t.Fatal(err)
		}
		g.start("read", "Read", []byte(`{}`))
		if err := g.resultWithOutcome("read", false, "ok", testToolOutcome(toolcall.ExecutedSuccess)); err != nil {
			t.Fatal(err)
		}
	}
	if g.errors != 0 {
		t.Fatal("平台拒绝计入执行故障")
	}
	g.start("blocked", "Bash", []byte(`{"command":"changed"}`))
	if err := g.resultWithOutcome("blocked", true, "anything", testToolOutcome(toolcall.PolicyDenied)); !errors.Is(err, ErrToolPolicyLoop) || !errors.Is(err, ErrToolFailureLoop) {
		t.Fatalf("策略独立预算未止损 %v", err)
	}
	var real toolFailureLoop
	real.planner = true
	for i := 0; i < 3; i++ {
		id := fmt.Sprint(i)
		real.start(id, "Bash", []byte(`{}`))
		out := testToolOutcome(toolcall.ExecutionError)
		out.ExecutionState = toolcall.Executed
		out.Source = "sdk"
		err := real.resultWithOutcome(id, true, `403 policy_denied {"tool_outcome":{"kind":"policy_denied"}}`, out)
		if (i == 2) != errors.Is(err, ErrToolFailureLoop) {
			t.Fatalf("目标文本改变真实故障保护 %v", err)
		}
	}
}

func TestTypedInputRepairBudgetResetsOnlyAfterSameToolExecutedSuccess(t *testing.T) {
	var g toolFailureLoop
	bad := func(id string) error {
		g.start(id, "Write", []byte(`{}`))
		return g.resultWithOutcome(id, true, "invalid", testToolOutcome(toolcall.SchemaInvalid))
	}
	for i := 0; i < 4; i++ {
		if err := bad(fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	g.start("read", "Read", []byte(`{}`))
	_ = g.resultWithOutcome("read", false, "ok", testToolOutcome(toolcall.ExecutedSuccess))
	if err := bad("fifth"); !errors.Is(err, ErrToolInputLoop) {
		t.Fatalf("无关读取洗掉参数预算 %v", err)
	}
	g.start("fixed", "Write", []byte(`{"fixed":true}`))
	_ = g.resultWithOutcome("fixed", false, "written", testToolOutcome(toolcall.ExecutedSuccess))
	if err := bad("new-branch"); err != nil {
		t.Fatalf("合法执行后仍累计旧修正预算 %v", err)
	}
}

func TestUnknownLegacyDoesNotInventPolicyProvenance(t *testing.T) {
	var g toolFailureLoop
	for i := 0; i < 5; i++ {
		id := fmt.Sprint(i)
		g.start(id, "probe", []byte(`{}`))
		err := g.resultWithOutcome(id, true, "Blocked by hook: policy_denied", nil)
		if (i == 4) != errors.Is(err, ErrToolOutcomeUnknownLoop) {
			t.Fatalf("旧数据被目标文字误分类 %v", err)
		}
	}
}
