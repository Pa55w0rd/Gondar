package db

import (
	"testing"
)

// A provider's call ID is local to one conversation and may be reused even in
// that conversation. Each result must select one preceding use in its own scope.
func TestNewToolCallsSincePairsScopedCallIDs(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("isolated postgres unavailable: %v", err)
	}
	defer d.Close()
	str := func(s string) *string { return &s }
	seg := func(n int) *int { return &n }
	type event struct {
		kind, tool, detail string
		call               *string
		worker             *string
		node               string
		segment            *int
	}
	for _, tc := range []struct {
		name string
		rows []event
		want map[string]string
	}{
		{"cross worker", []event{
			{kind: "tool_use", detail: "input-a", call: str("call_1"), worker: str("worker-a")},
			{kind: "tool_use", detail: "input-b", call: str("call_1"), worker: str("worker-b")},
			{kind: "tool_result", detail: "result-b", call: str("call_1"), worker: str("worker-b")},
			{kind: "tool_result", detail: "result-a", call: str("call_1"), worker: str("worker-a")},
		}, map[string]string{"result-a": "input-a", "result-b": "input-b"}},
		{"cross intent", []event{
			{kind: "tool_use", detail: "input-a", call: str("call_1"), worker: str("worker"), node: "a"},
			{kind: "tool_use", detail: "input-b", call: str("call_1"), worker: str("worker"), node: "b"},
			{kind: "tool_result", detail: "result-a", call: str("call_1"), worker: str("worker"), node: "a"},
		}, map[string]string{"result-a": "input-a"}},
		{"main segment", []event{
			{kind: "tool_use", detail: "old-session", call: str("call_1"), worker: str("mainagent"), segment: seg(0)},
			{kind: "tool_use", detail: "new-session", call: str("call_1"), worker: str("mainagent"), segment: seg(1)},
			{kind: "tool_result", detail: "result", call: str("call_1"), worker: str("mainagent"), segment: seg(1)},
		}, map[string]string{"result": "new-session"}},
		{"legacy null segment equals zero", []event{
			{kind: "tool_use", detail: "legacy-session", call: str("call_1"), worker: str("mainagent")},
			{kind: "tool_result", detail: "result", call: str("call_1"), worker: str("mainagent"), segment: seg(0)},
		}, map[string]string{"result": "legacy-session"}},
		{"legacy null worker equals empty", []event{
			{kind: "tool_use", detail: "legacy-worker", call: str("call_1")},
			{kind: "tool_use", detail: "other-worker", call: str("call_1"), worker: str("worker")},
			{kind: "tool_result", detail: "result", call: str("call_1"), worker: str("")},
		}, map[string]string{"result": "legacy-worker"}},
		{"null intent does not match non-null", []event{
			{kind: "tool_use", detail: "intent-a", call: str("call_1"), worker: str("worker"), node: "a"},
			{kind: "tool_result", detail: "result", call: str("call_1"), worker: str("worker")},
		}, map[string]string{"result": ""}},
		{"reused ID in later turns", []event{
			{kind: "tool_use", detail: "first-input", call: str("call_1")},
			{kind: "tool_result", detail: "first-result", call: str("call_1")},
			{kind: "tool_use", detail: "second-input", call: str("call_1")},
			{kind: "tool_result", detail: "second-result", call: str("call_1")},
		}, map[string]string{"first-result": "first-input", "second-result": "second-input"}},
		{"nearest preceding use", []event{
			{kind: "tool_use", detail: "older-input", call: str("call_1")},
			{kind: "tool_use", detail: "newer-input", call: str("call_1")},
			{kind: "tool_result", detail: "result", call: str("call_1")},
		}, map[string]string{"result": "newer-input"}},
		{"different tool", []event{
			{kind: "tool_use", tool: "Bash", detail: "unrelated-command", call: str("call_1")},
			{kind: "tool_result", detail: "result", call: str("call_1")},
		}, map[string]string{"result": ""}},
		{"missing IDs", []event{
			{kind: "tool_use", detail: "null-ID"},
			{kind: "tool_use", detail: "empty-ID", call: str("")},
			{kind: "tool_result", detail: "null-result"},
			{kind: "tool_result", detail: "empty-result", call: str("")},
		}, map[string]string{"null-result": "", "empty-result": ""}},
		{"future use cannot supply earlier result", []event{
			{kind: "tool_result", detail: "early-result", call: str("call_1")},
			{kind: "tool_use", detail: "future-input", call: str("call_1")},
			{kind: "tool_result", detail: "later-result", call: str("call_1")},
		}, map[string]string{"early-result": "", "later-result": "future-input"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, err := d.CreateTask("scoped trigger "+tc.name, "fixture goal", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := d.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID); err != nil {
					t.Error(err)
				}
				if _, err := d.Exec(`DELETE FROM explorations WHERE id=$1`, task.ExplorationID); err != nil {
					t.Error(err)
				}
			})
			es := d.Exploration(task.ExplorationID)
			nodes := map[string]int64{}
			for _, name := range []string{"a", "b"} {
				id, err := es.AddIntent(map[string]any{"summary": name}, 1, nil, "planner")
				if err != nil {
					t.Fatal(err)
				}
				nodes[name] = id
			}
			var watermark int64
			for _, row := range tc.rows {
				tool := row.tool
				if tool == "" {
					tool = "report_finding"
				}
				var node any
				if row.node != "" {
					node = nodes[row.node]
				}
				var id int64
				if err := d.QueryRow(`INSERT INTO activity(exploration_id,node_id,worker,kind,tool,tool_use_id,detail) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`, task.ExplorationID, node, row.worker, row.kind, tool, row.call, row.detail).Scan(&id); err != nil {
					t.Fatal(err)
				}
				if row.kind == "tool_result" {
					watermark = id
				}
			}
			events, err := d.NewToolCallsSince(0)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]int{}
			for _, event := range events {
				if event.TaskID != task.ID {
					continue
				}
				seen[event.ToolOutput]++
				wantInput, ok := tc.want[event.ToolOutput]
				if !ok || event.ToolInput != wantInput || event.TaskDesc != task.Description || event.TaskGoal != task.Goal {
					t.Errorf("event output=%q input=%q (want %q) context=%q/%q", event.ToolOutput, event.ToolInput, wantInput, event.TaskDesc, event.TaskGoal)
				}
			}
			for output := range tc.want {
				if seen[output] != 1 {
					t.Errorf("result %q triggered %d times, want once", output, seen[output])
				}
			}
			if len(seen) != len(tc.want) {
				t.Errorf("unexpected result set: %v", seen)
			}
			remaining, err := d.NewToolCallsSince(watermark)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range remaining {
				if event.TaskID == task.ID {
					t.Errorf("watermark replayed result: %+v", event)
				}
			}
		})
	}
}
