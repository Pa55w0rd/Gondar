package db

import (
	"strings"
	"testing"
	"time"
)

func TestDailySchedulePreservesFullConstraintSnapshot(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("daily constraints fixture", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTask(task.ID) })
	es := d.Exploration(task.ExplorationID)
	deny, err := es.AddConstraint("deny", "禁止写操作；只允许读取已登记入口", "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.AddConstraint("allow", "仅允许核对合成身份的只读响应", "human"); err != nil {
		t.Fatal(err)
	}
	plan, err := d.CreateDailySchedule(task.ID, "09:00", "Asia/Shanghai", 1, 120, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := es.UpdateConstraint(deny, "deny", "后来新增的限制不改已有计划快照"); err != nil {
		t.Fatal(err)
	}
	child, err := d.MaterializeDailySchedule(plan.NextRunAt.Add(time.Second))
	if err != nil || child == nil {
		t.Fatalf("materialize: %+v %v", child, err)
	}
	t.Cleanup(func() { d.DeleteTask(child.ID) })
	items, err := d.Exploration(child.ExplorationID).ListConstraints()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("allow/deny boundaries lost: %+v", items)
	}
	found := false
	for _, item := range items {
		if item.Kind == "deny" && item.Text == "禁止写操作；只允许读取已登记入口" && item.Origin == "human" {
			found = true
		}
	}
	if !found {
		t.Fatalf("snapshot changed or was truncated: %+v", items)
	}
}

func TestDailyScheduleRejectsOversizedOrLegacyConstraints(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("bounded daily constraints", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTask(task.ID) })
	es := d.Exploration(task.ExplorationID)
	id, err := es.AddConstraint("deny", strings.Repeat("x", (1<<20)+1), "human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateDailySchedule(task.ID, "09:00", "Asia/Shanghai", 1, 120, time.Now()); err == nil {
		t.Fatal("oversized constraints silently copied/truncated")
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM daily_task_schedules WHERE source_task_id=$1`, task.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial plan left behind: %d %v", count, err)
	}
	if err := es.DeleteConstraint(id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO task_constraints(exploration_id,kind,text,origin) SELECT $1,'deny','bounded constraint','human' FROM generate_series(1,1001)`, task.ExplorationID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateDailySchedule(task.ID, "09:00", "Asia/Shanghai", 1, 120, time.Now()); err == nil {
		t.Fatal("excess constraint count silently copied/truncated")
	}
	if _, err := d.Exec(`DELETE FROM task_constraints WHERE exploration_id=$1`, task.ExplorationID); err != nil {
		t.Fatal(err)
	}
	plan, err := d.CreateDailySchedule(task.ID, "09:00", "Asia/Shanghai", 1, 120, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE daily_task_schedules SET snapshot=snapshot-'Constraints' WHERE id=$1`, plan.ID); err != nil {
		t.Fatal(err)
	}
	if child, err := d.MaterializeDailySchedule(plan.NextRunAt.Add(time.Second)); err != nil || child != nil {
		t.Fatalf("legacy boundary-free task launched: %+v %v", child, err)
	}
	plans, err := d.ListDailySchedules(task.ID)
	if err != nil || len(plans) != 1 || plans[0].Enabled {
		t.Fatalf("legacy plan not visibly disabled: %+v %v", plans, err)
	}
}
