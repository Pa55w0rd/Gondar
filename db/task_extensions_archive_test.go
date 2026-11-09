package db

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestTaskExtensionsArchiveRestorePreservesEvidenceAndFreezesSchedules(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	task, es, activityID := lessonFixture(t, d)
	in := LessonInput{Tool: "Write", Summary: "参数契约经验", Resolution: "检查schema后填充实际参数", EvidenceIDs: []int64{activityID}}
	l, err := es.ProposeLesson(task.ID, "work#1", in)
	if err != nil {
		t.Fatal(err)
	}
	l, err = d.ReviewLesson(task.ID, l.ID, l.Revision, "accepted", true, "受控证明", 30)
	if err != nil {
		t.Fatal(err)
	}
	asset := seedTreeAsset(t, d, "subdomain", map[string]any{"domain": "archive-resource.example.invalid", "task_ids": []int64{task.ID}})
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE domain IN ('archive-resource.example.invalid','collision-resource.example.invalid')`)
	})
	r, err := es.RegisterTaskResource(task.ID, asset, "upload", "controlled-upload", "删除测试文件并验证不可访问", "work#1", 24)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := es.AppendActivity(Activity{Kind: "tool_result", Tool: "Bash", Detail: "controlled file created"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = es.UpdateTaskResource(task.ID, r.ID, r.Revision, "created", "实际结果已保存", []int64{proof})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := d.CreateDailySchedule(task.ID, "09:00", "Asia/Shanghai", 2, 600, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	child, err := d.MaterializeDailySchedule(plan.NextRunAt.Add(time.Second))
	if err != nil || child == nil {
		t.Fatalf("plan occurrence: %+v %v", child, err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM tasks WHERE id=$1`, child.ID) })
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	a, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.ProposeLesson(task.ID, "work#1", in); !errors.Is(err, ErrTaskExtensionFrozen) {
		t.Fatalf("late proposal accepted: %v", err)
	}
	if _, err := es.UpdateTaskResource(task.ID, r.ID, r.Revision, "cleanup_pending", "晚到写入", nil); !errors.Is(err, ErrTaskExtensionFrozen) {
		t.Fatalf("late ledger update accepted: %v", err)
	}
	if err := d.CancelDailySchedule(task.ID, plan.ID); !errors.Is(err, ErrTaskExtensionFrozen) {
		t.Fatalf("archive schedule mutated: %v", err)
	}
	claimed, err := d.ClaimTaskArchiveJob(t.Context())
	if err != nil || claimed == nil || claimed.ID != a.ID {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"agent_lessons", "agent_lesson_reviews", "daily_task_schedules", "daily_task_occurrences", "task_resources", "task_resource_events"} {
		if snapshot.DataCounts[table] != 1 {
			t.Fatalf("snapshot missing %s: %v", table, snapshot.DataCounts)
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TaskArchiveSnapshot
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteTaskArchive(a.ID, &decoded, "controlled-extensions.tar.zst", "synthetic-digest", 100, 50); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DELETE FROM tasks WHERE id=$1`, child.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"agent_lessons", "task_resources"} {
		var count int
		if err := d.QueryRow(`SELECT count(*) FROM `+table+` WHERE task_id=$1`, task.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("hot %s retained: %d %v", table, count, err)
		}
	}
	// A global asset can claim the old ID while the task is cold.
	if _, err := d.Exec(`INSERT INTO assets(id,type,domain) VALUES($1,'subdomain','collision-resource.example.invalid')`, asset); err != nil {
		t.Fatal(err)
	}
	if _, err := d.QueueTaskArchiveRestore(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RestoreTaskArchive(a.ID, &decoded, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteTaskArchiveRestore(a.ID); err != nil {
		t.Fatal(err)
	}
	resources, err := d.ListTaskResources(task.ID)
	if err != nil || len(resources) != 1 || resources[0].State != "created" || resources[0].AssetID == nil || *resources[0].AssetID == asset {
		t.Fatalf("resource/asset remap failed: %+v %v", resources, err)
	}
	lessons, err := d.ListTaskLessons(task.ID)
	if err != nil || len(lessons) != 1 || lessons[0].Status != "accepted" || lessons[0].Evidence[0].ActivityID != activityID {
		t.Fatalf("lesson evidence lost: %+v %v", lessons, err)
	}
	plans, err := d.ListDailySchedules(task.ID)
	if err != nil || len(plans) != 1 || plans[0].Enabled {
		t.Fatalf("restored daily plan reactivated: %+v %v", plans, err)
	}
	if next, err := d.MaterializeDailySchedule(time.Now().Add(72 * time.Hour)); err != nil || next != nil {
		t.Fatalf("restored plan ran: %+v %v", next, err)
	}
}
