package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
	"gondar/db"
)

// TestValidateFindingEvidence v0.4.5 证据门槛：critical/high 必须达到最低篇幅，
// medium/low 不设门槛。阈值用字符数（中文按字计），边界逐项钉住。
func TestValidateFindingEvidence(t *testing.T) {
	long := strings.Repeat("证", 200)
	mid := strings.Repeat("据", 120)
	cases := []struct {
		name     string
		severity string
		evidence string
		wantErr  bool
	}{
		{"critical-empty", "critical", "", true},
		{"critical-short", "critical", strings.Repeat("证", 199), true},
		{"critical-exact", "critical", long, false},
		{"high-short", "high", strings.Repeat("证", 119), true},
		{"high-exact", "high", mid, false},
		{"high-uppercase", " HIGH ", mid, false},
		{"medium-short", "medium", "一句话说明", false},
		{"low-empty", "low", "", false},
		{"unknown-passthrough", "unknown", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := validateFindingEvidence(c.severity, c.evidence)
			if (msg != "") != c.wantErr {
				t.Fatalf("severity=%q len=%d: got %q, wantErr=%v", c.severity, len([]rune(c.evidence)), msg, c.wantErr)
			}
			// 打回消息必须给出可执行的两条出路（补证据 / 降级），模型才能自纠。
			if msg != "" && !strings.Contains(msg, "降为 medium") {
				t.Fatalf("打回提示缺少降级指引: %q", msg)
			}
		})
	}
}

func TestReportFindingChecksCleanedEvidence(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil || dsn == "" {
		t.Skip("requires GONDAR_PG_DSN")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTask("cleaned finding validation", "local fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	tool := NewToolSet(d.Exploration(task.ExplorationID), "fixture").addFinding()
	for _, tc := range []struct {
		name, severity, evidence string
		blockEmptyLedger         bool
	}{
		{"severity cleaned before threshold", "hi\x00gh", "short", false},
		{"NUL cannot pad evidence", "critical", strings.Repeat("\x00", 200), false},
		{"empty ledger cannot prove a claim", "high", strings.Repeat("unobserved evidence ", 10), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.blockEmptyLedger {
				ctx = WithToolOutputs(ctx, NewToolOutputLedger(), EvidenceVerifyBlock)
			}
			raw, err := json.Marshal(map[string]any{"vulnclass": "test", "name": tc.name, "severity": tc.severity, "summary": "fixture", "evidence": tc.evidence})
			if err != nil {
				t.Fatal(err)
			}
			result, err := tool.Call(ctx, raw, nil)
			if err != nil || !result.IsError || !strings.Contains(result.Flatten(), "证据") {
				t.Fatalf("unverified finding accepted: %v / %+v", err, result)
			}
			var rows int
			if err := d.QueryRow(`SELECT count(*) FROM findings WHERE task_id=$1`, task.ID).Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("invalid evidence was persisted: rows=%d err=%v", rows, err)
			}
		})
	}
}

// TestEvidenceGuidanceInjected 证据纪律走 tool guidance：两种流量开关状态下都要
// 注入（关闭自动绑定时上报者照样要过证据门槛），reporter 额外获得报告结构补充，
// 没有上报工具的 agent 不注入。
func TestEvidenceGuidanceInjected(t *testing.T) {
	oldPolicy := FindingTrafficBindingEnabled
	t.Cleanup(func() { FindingTrafficBindingEnabled = oldPolicy })
	ts := NewToolSet(nil, "fixture")
	findingTool := []actool.CoreTool{ts.addFinding()}

	for _, enabled := range []bool{false, true} {
		FindingTrafficBindingEnabled = func() bool { return enabled }
		_, g := findingWorkflowTools("worker", findingTool)
		if !strings.Contains(g, "L3") || !strings.Contains(g, "外部运维约束") || !strings.Contains(g, "观察 ≠ 推断") {
			t.Fatalf("binding=%v: evidence discipline missing: %q", enabled, g)
		}
	}
	FindingTrafficBindingEnabled = func() bool { return true }
	// reporter 的报告结构补充只在它有 update_finding_report 时注入。
	fakeUpdate := writeTool("update_finding_report", "fake", map[string]any{"type": "object", "properties": map[string]any{}},
		func(context.Context, json.RawMessage) (actool.Result, error) { return actool.Text("ok"), nil })
	_, reporterG := findingWorkflowTools("reporter", append(append([]actool.CoreTool{}, findingTool...), fakeUpdate))
	if !strings.Contains(reporterG, "前置条件") || !strings.Contains(reporterG, "影响证明") {
		t.Fatalf("reporter annex missing: %q", reporterG)
	}
	_, noneG := findingWorkflowTools("worker", nil)
	if strings.Contains(noneG, "L3") || strings.Contains(noneG, "前置条件") {
		t.Fatalf("guidance leaked to agents without reporting tools: %q", noneG)
	}
}
