package db

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolInvocationExactAuditAndConversationPersistence(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c, err := d.CreateConversation("fixture", "调用身份", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM conversations WHERE id=$1`, c.ID)
	metadata := func(inv string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"tool_call_identity": map[string]any{"version": 1, "run_id": "host", "prompt_id": "prompt", "invocation_id": inv, "tool_use_id": "same", "tool_name": "Bash"}})
		return raw
	}
	create := func(inv string) int64 {
		id, err := d.CreateDecidedIntercept(0, c.ID, "", "fixture", "Bash", []byte(`{"command":"synthetic"}`), "allowed", "fixture", &InterceptAudit{RunID: "host", PromptID: "prompt", InvocationID: inv, ToolUseID: "same", Correlation: "exact", EffectiveAction: "allow", ExecutionStatus: "awaiting_result"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM intercept_pending WHERE id=$1`, id) })
		return id
	}
	a, b := create("a"), create("b")
	for _, inv := range []string{"a", "b"} {
		for _, kind := range []string{"tool_use", "tool_result"} {
			_, err = d.AppendConvActivity(c.ID, Activity{Worker: "fixture", Kind: kind, Tool: "Bash", ToolUseID: "same", Metadata: metadata(inv)})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = d.CompleteInterceptInvocation(a, "host", "b", "same", "succeeded", "WRONG", false); err != nil {
		t.Fatal(err)
	}
	if err = d.CompleteIntercept(a, "host", "same", "succeeded", "LEGACY WRONG", false); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetInterceptDetail(a)
	if got.Audit.Output != "" {
		t.Fatal("其它调用或旧无身份方法覆盖了新审计")
	}
	if err = d.CompleteInterceptInvocation(a, "host", "a", "same", "unknown", "primary", false); err != nil {
		t.Fatal(err)
	}
	if err = d.AppendInterceptLateReceipt(a, "host", "a", "same", "late_succeeded", "actual", false); err != nil {
		t.Fatal(err)
	}
	if err = d.AppendInterceptLateReceipt(a, "host", "a", "same", "late_failed", "overwrite", false); err != nil {
		t.Fatal(err)
	}
	got, _ = d.GetInterceptDetail(a)
	if got.Audit.ExecutionStatus != "unknown" || !strings.Contains(string(got.Audit.LateReceipt), "actual") || strings.Contains(string(got.Audit.LateReceipt), "overwrite") {
		t.Fatalf("晚回执覆盖主回执或不幂等 %+v", got.Audit)
	}
	_, err = d.AppendConvActivity(c.ID, Activity{Worker: "fixture", Kind: "tool_late_result", Tool: "Bash", ToolUseID: "same", Metadata: metadata("a")})
	if err != nil {
		t.Fatal(err)
	}
	for id, count := range map[int64]int{a: 3, b: 2} {
		target, err := d.GetInterceptExecution(id)
		if err != nil || target == nil || len(target.Items) != count {
			t.Fatalf("同参/同提供商ID串账 id=%d target=%+v err=%v", id, target, err)
		}
	}
	_, err = d.Exec(`INSERT INTO conversation_activities(conversation_id,worker,kind,summary) VALUES($1,'fixture','text','legacy')`, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	list, _, err := d.ConvActivityList(c.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	page, _, err := d.ConvActivityPage(c.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, rows := range [][]Activity{list, page} {
		if len(rows) != 6 {
			t.Fatalf("活动丢失 %d", len(rows))
		}
		if string(rows[len(rows)-1].Metadata) != "{}" {
			t.Fatalf("旧记录默认值不兼容 %s", rows[len(rows)-1].Metadata)
		}
		if !strings.Contains(string(rows[0].Metadata), `"invocation_id": "a"`) {
			t.Fatalf("保存加载丢身份 %s", rows[0].Metadata)
		}
	}
}
