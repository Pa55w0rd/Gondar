package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"gondar/db"
)

// 有新的真实证据时，链深不再是拒绝继续验证的理由。
func TestAddIntentContinuesDeepWithNewEvidence(t *testing.T) {
	d := testDB(t)
	task, err := d.CreateTask("续代数测试", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	store := d.Exploration(task.ExplorationID)
	tools := NewToolSet(store, "planner")
	tools.SetTaskID(task.ID)

	// 通过真实工具写入路径保存检查点，验证超过旧三代限制仍能继续。
	prevID, err := tools.addOneIntent(intentItem{Summary: "第一代：探某个方向"})
	if err != nil {
		t.Fatalf("第一代: %v", err)
	}
	mkID := func(id int64) json.RawMessage {
		b, _ := json.Marshal(id)
		return b
	}
	for gen := 2; gen <= 8; gen++ {
		if _, err := tools.recordOneFact(factItem{Summary: fmt.Sprintf("已实际核对的合成步骤%d", gen), Evidence: fmt.Sprintf("local fixture output %d", gen)}, prevID); err != nil {
			t.Fatal(err)
		}
		if err := store.SetIntentState(prevID, "exhausted"); err != nil {
			t.Fatal(err)
		}
		next, err := tools.addOneIntent(intentItem{
			Summary:   fmt.Sprintf("接续第%d代，验证下一合成步骤", gen),
			Continues: []json.RawMessage{mkID(prevID)},
		})
		if err != nil {
			t.Fatalf("第%d代有新证据的接续应被允许: %v", gen, err)
		}
		prevID = next
	}
	last, err := store.GetNode(prevID)
	if err != nil || continuationDepthOf(last) != 8 {
		t.Fatalf("深链追踪丢失：node=%+v err=%v", last, err)
	}
}

// 接续代数必须被**记进节点**，planner 才能读到（这是数据层可算的前提）。
func TestAddIntentRecordsContinuationDepth(t *testing.T) {
	d := testDB(t)
	task, err := d.CreateTask("续代数记录", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	store := d.Exploration(task.ExplorationID)
	tools := NewToolSet(store, "planner")
	tools.SetTaskID(task.ID)

	first, err := tools.addOneIntent(intentItem{Summary: "首探"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetIntentState(first, "exhausted"); err != nil {
		t.Fatal(err)
	}
	mkID := func(id int64) json.RawMessage {
		b, _ := json.Marshal(id)
		return b
	}
	second, err := tools.addOneIntent(intentItem{
		Summary:   "接续首探",
		Continues: []json.RawMessage{mkID(first)},
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := store.GetNode(second)
	if err != nil || node == nil {
		t.Fatalf("取接续意图失败: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(node.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if got := continuationDepthOf(node); got != 2 {
		t.Fatalf("接续意图的代数应为 2,got %d", got)
	}
	if payload["continues"] == nil {
		t.Error("接续意图应记录 continues")
	}
}

// 意图 summary 超长要被截断——planner 的判断会因长 summary 失真
// （长 → 更容易觉得「快到了」→ 续派 → 更长，正反馈）。
func TestAddIntentClampsLongSummary(t *testing.T) {
	d := testDB(t)
	task, err := d.CreateTask("summary 截断", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	store := d.Exploration(task.ExplorationID)
	tools := NewToolSet(store, "planner")
	tools.SetTaskID(task.ID)

	long := "目标主机 10.0.0.1 的登录面探测与默认口令尝试，附带大量重复叙述" +
		strings.Repeat("补充说明细节", 200)
	id, err := tools.addOneIntent(intentItem{Summary: long})
	if err != nil {
		t.Fatal(err)
	}
	node, err := store.GetNode(id)
	if err != nil || node == nil {
		t.Fatalf("取意图失败: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(node.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	got, _ := payload["summary"].(string)
	if utf8.RuneCountInString(got) > maxIntentSummaryRunes+40 {
		t.Fatalf("summary 未被截断: %d rune", utf8.RuneCountInString(got))
	}
	if !strings.HasPrefix(got, "目标主机 10.0.0.1") {
		t.Errorf("截断应保留头部: %q", got[:min(40, len(got))])
	}
}

// continues 只能指向**意图**节点——指向 fact 会被拒。
func TestAddIntentRejectsContinuesToNonIntent(t *testing.T) {
	d := testDB(t)
	task, err := d.CreateTask("continues 校验", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	store := d.Exploration(task.ExplorationID)
	tools := NewToolSet(store, "planner")
	tools.SetTaskID(task.ID)

	factID, err := store.AddNode(db.KindFact, map[string]any{"summary": "一个事实"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(factID)
	if _, err := tools.addOneIntent(intentItem{
		Summary:   "接续一个 fact（非法）",
		Continues: []json.RawMessage{b},
	}); err == nil {
		t.Fatal("continues 指向 fact 应被拒绝")
	}
}

// continues 指向不存在的 id 应被拒绝（而不是静默忽略）。
func TestAddIntentRejectsUnknownContinues(t *testing.T) {
	d := testDB(t)
	task, err := d.CreateTask("continues 未知", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	store := d.Exploration(task.ExplorationID)
	tools := NewToolSet(store, "planner")
	tools.SetTaskID(task.ID)

	b, _ := json.Marshal(int64(999999))
	if _, err := tools.addOneIntent(intentItem{
		Summary:   "接续一个不存在的意图",
		Continues: []json.RawMessage{b},
	}); err == nil {
		t.Fatal("continues 指向不存在的节点应被拒绝")
	}
}
