package db

import (
	"errors"
	"testing"
)

func TestResourceLedgerScopeProofCASAndCleanup(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	task, es, failed := lessonFixture(t, d)
	other, otherES, foreign := lessonFixture(t, d)
	asset := seedTreeAsset(t, d, "subdomain", map[string]any{"domain": "resource-one.example.invalid", "task_ids": []int64{task.ID}})
	foreignAsset := seedTreeAsset(t, d, "subdomain", map[string]any{"domain": "resource-other.example.invalid", "task_ids": []int64{other.ID}})
	t.Cleanup(func() { d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{asset, foreignAsset}) })
	if _, err := es.RegisterTaskResource(task.ID, foreignAsset, "session", "test-session", "结束会话并检查残留", "work#1", 24); err == nil {
		t.Fatal("foreign asset claimed")
	}
	r, err := es.RegisterTaskResource(task.ID, asset, "session", "test-session", "结束会话并检查残留", "work#1", 24)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "planned" {
		t.Fatal("registration treated as verified success")
	}
	for _, tool := range []string{"register_resource", "update_resource", "list_resources", "propose_lesson", "save_evidence", "insert_assets"} {
		ack, err := es.AppendActivity(Activity{Kind: "tool_result", Tool: tool, Detail: "resource registered successfully"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := es.UpdateTaskResource(task.ID, r.ID, r.Revision, "created", "不能用登记回执自证", []int64{ack}); err == nil {
			t.Fatalf("bookkeeping self-proof accepted: %s", tool)
		}
	}
	for _, proof := range [][]int64{nil, {failed}, {foreign}} {
		if _, err := es.UpdateTaskResource(task.ID, r.ID, r.Revision, "created", "待确认", proof); err == nil {
			t.Fatalf("invalid success proof accepted: %v", proof)
		}
	}
	if _, err := otherES.UpdateTaskResource(other.ID, r.ID, r.Revision, "failed", "不是本任务", nil); err == nil {
		t.Fatal("foreign resource mutated")
	}
	proof, err := es.AppendActivity(Activity{Kind: "tool_result", Tool: "Bash", Detail: "controlled operation succeeded"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = es.UpdateTaskResource(task.ID, r.ID, r.Revision, "created", "受控操作已完成", []int64{proof})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.UpdateTaskResource(task.ID, r.ID, r.Revision-1, "active", "stale", []int64{proof}); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("stale update accepted: %v", err)
	}
	r, err = es.UpdateTaskResource(task.ID, r.ID, r.Revision, "cleanup_pending", "计划清理，尚未执行", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.UpdateTaskResource(task.ID, r.ID, r.Revision, "cleaned", "清理失败却误标成功", []int64{failed}); err == nil {
		t.Fatal("failed cleanup presented as success")
	}
	r, err = es.UpdateTaskResource(task.ID, r.ID, r.Revision, "failed", "清理失败，保留残留记录", []int64{failed})
	if err != nil {
		t.Fatal(err)
	}
	r, err = es.UpdateTaskResource(task.ID, r.ID, r.Revision, "cleanup_pending", "重新安排清理", nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err = es.UpdateTaskResource(task.ID, r.ID, r.Revision, "cleaned", "受控清理验证完成", []int64{proof})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.UpdateTaskResource(task.ID, r.ID, r.Revision, "active", "不能复活旧记录", []int64{proof}); err == nil {
		t.Fatal("cleaned resource resurrected")
	}
	var events int
	if err := d.QueryRow(`SELECT count(*) FROM task_resource_events WHERE resource_id=$1`, r.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 5 {
		t.Fatalf("history lost: %d", events)
	}
}
