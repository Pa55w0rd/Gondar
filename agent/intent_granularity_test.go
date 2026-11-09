package agent

import (
	"strings"
	"testing"
)

func TestComplexTaskIntentRejectsOnlyWholeTaskEcho(t *testing.T) {
	goal := "核对已经登记的两个业务入口并汇总可复查证据"
	description := "1. 记录入口\n2. 核对身份差异\n3. 汇总证据"
	if !repeatsComplexTask(goal, description, goal) {
		t.Fatal("whole complex goal echo accepted")
	}
	// Sharing the goal's beginning is legitimate: a concrete step may name the
	// same asset or outcome. No byte-prefix or broad substring rejection.
	step := goal + "：本条只核对入口A的只读响应，保留两种合成身份的差异"
	if repeatsComplexTask(step, description, goal) {
		t.Fatal("concrete step sharing goal prefix rejected")
	}
	if repeatsComplexTask("核对一个指定接口", "仅限一个接口", "核对一个指定接口") {
		t.Fatal("short single direction rejected")
	}
	longGoal := strings.Repeat("合成目标", 90)
	if !repeatsComplexTask(longGoal, "范围保持原样", longGoal) {
		t.Fatal("long goal echo accepted")
	}
}

func TestAddIntentComplexGoalEchoUsesCreationPath(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	goal := "核对两个已登记的合成入口"
	task, err := d.CreateTask("1. 检查入口\n2. 保存证据\n3. 汇总覆盖", goal, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTask(task.ID) })
	tsx := NewToolSet(d.Exploration(task.ExplorationID), "planner")
	if _, err := tsx.addOneIntent(intentItem{Summary: goal}); err == nil {
		t.Fatal("whole scenario entered frontier")
	}
	if _, err := tsx.addOneIntent(intentItem{Summary: goal + "：本条只核对入口A并保存只读响应"}); err != nil {
		t.Fatalf("concrete step rejected: %v", err)
	}
}
