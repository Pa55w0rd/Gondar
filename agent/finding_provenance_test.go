package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/toolcall"
	"gondar/db"
)

func TestAutomaticFindingMissingSourceIsTypedAndSeverityIndependentThroughSDK(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("自动出处SDK回归", "纯合成", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	ts := NewToolSet(d.Exploration(task.ExplorationID), "fixture")
	ts.SetTaskID(task.ID)
	for _, severity := range []string{"critical", "high", "medium", "low"} {
		t.Run(severity, func(t *testing.T) {
			payload, _ := json.Marshal(map[string]any{"vulnclass": "其他", "name": "synthetic", "severity": severity, "summary": "合成出处缺失", "evidence": strings.Repeat("没有原件的长文字不能证明执行成功。", 30), "automatic": false})
			calls := 0
			provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
				calls++
				events := []llm.StreamEvent{{Type: llm.SETextDelta, Text: "保存待核验线索并换路"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}}
				if calls == 1 {
					events = []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: "reused", ToolName: "report_finding"}, {Type: llm.SEToolInputJSON, Text: string(payload)}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}}
				}
				for _, ev := range events {
					if !yield(ev, nil) {
						return
					}
				}
			}}
			var outcomes []ToolOutcome
			_, _, err := captureRun(WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, ExplorationID: task.ExplorationID, Kind: KindPlanner}), agentcore.Options{Provider: provider, Tools: []tool.CoreTool{ts.addFinding()}, PermissionMode: permission.ModeBypass, MaxTurns: 3}, "合成", func(a db.Activity) {
				if a.Kind == "tool_result" {
					out, ok := ToolOutcomeFromMetadata(a.Metadata)
					if ok {
						outcomes = append(outcomes, out)
					}
				}
			})
			if err != nil || len(outcomes) != 1 || outcomes[0].Kind != toolcall.EvidenceIncomplete || outcomes[0].ExecutionState != toolcall.NotExecuted {
				t.Fatalf("缺证被误计为执行故障 %+v %v", outcomes, err)
			}
		})
	}
	var count int
	if err = d.QueryRow(`SELECT count(*) FROM findings WHERE task_id=$1`, task.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("缺证仍确认了漏洞 count=%d err=%v", count, err)
	}
}
