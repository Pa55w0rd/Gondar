package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func lessonFixture(t *testing.T, d *DB) (*Task, *ExplorationStore, int64) {
	t.Helper()
	task, err := d.CreateTask("lesson-fixture", "本地受控工具错误", nil, 300, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID) })
	es := d.Exploration(task.ExplorationID)
	id, err := es.AppendActivity(Activity{Kind: "tool_result", Tool: "Write", Worker: "work#1", IsError: true, Summary: "invalid input", Detail: "missing required property file_path"})
	if err != nil {
		t.Fatal(err)
	}
	return task, es, id
}

func TestLessonsEvidenceReviewIsolationExpiry(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	a, es, evidence := lessonFixture(t, d)
	b, other, _ := lessonFixture(t, d)
	input := LessonInput{Tool: "Write", Summary: "Write 参数缺失", Resolution: "适用前置：校验失败且尚未写文件。根据实际schema填写file_path和content，不重复提交空对象。", Tags: []string{"schema", "write"}, EvidenceIDs: []int64{evidence}}
	l, err := es.ProposeLesson(a.ID, "work#1", input)
	if err != nil {
		t.Fatal(err)
	}
	if l.Status != "pending" || l.Shared || len(l.Evidence) != 1 || len(l.Evidence[0].SHA256) != 64 {
		t.Fatalf("proposal state: %+v", l)
	}
	duplicate, err := es.ProposeLesson(a.ID, "work#1", input)
	if err != nil || duplicate.ID != l.ID {
		t.Fatalf("duplicate created: %v %v", duplicate, err)
	}
	if _, err := other.ProposeLesson(b.ID, "work#2", input); err == nil {
		t.Fatal("cross-task evidence accepted")
	}
	if _, err := es.ProposeLesson(b.ID, "work#1", input); err == nil {
		t.Fatal("foreign task context accepted")
	}
	bad := input
	bad.Tool = "Bash"
	if _, err := es.ProposeLesson(a.ID, "work#1", bad); err == nil {
		t.Fatal("wrong tool evidence accepted")
	}
	got, err := es.RecallLessons(a.ID, "Write", "")
	if err != nil || len(got.Lessons) != 0 {
		t.Fatalf("unreviewed lesson recalled: %v %v", got, err)
	}
	l, err = d.ReviewLesson(a.ID, l.ID, l.Revision, "accepted", false, "已对照参数契约", 30)
	if err != nil {
		t.Fatal(err)
	}
	got, err = es.RecallLessons(a.ID, "Write", "schema")
	if err != nil || len(got.Lessons) != 1 {
		t.Fatalf("approved local recall failed: %v %v", got, err)
	}
	got, err = other.RecallLessons(b.ID, "Write", "")
	if err != nil || len(got.Lessons) != 0 {
		t.Fatal("local lesson leaked across tasks")
	}
	if _, err := d.ReviewLesson(a.ID, l.ID, l.Revision-1, "accepted", true, "stale", 30); !errors.Is(err, ErrLessonConflict) {
		t.Fatalf("stale CAS accepted: %v", err)
	}
	l, err = d.ReviewLesson(a.ID, l.ID, l.Revision, "accepted", true, "已确认不含目标数据与凭据", 7)
	if err != nil {
		t.Fatal(err)
	}
	got, err = other.RecallLessons(b.ID, "Write", "")
	if err != nil || len(got.Lessons) != 1 {
		t.Fatalf("explicit shared recall failed: %v %v", got, err)
	}
	var history int
	if err := d.QueryRow(`SELECT count(*) FROM agent_lesson_reviews WHERE lesson_id=$1`, l.ID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if history != 2 {
		t.Fatalf("review history lost: %d", history)
	}
	if _, err := d.Exec(`UPDATE agent_lessons SET expires_at=now()-interval '1 second' WHERE id=$1`, l.ID); err != nil {
		t.Fatal(err)
	}
	got, err = other.RecallLessons(b.ID, "Write", "")
	if err != nil || len(got.Lessons) != 0 {
		t.Fatal("expired lesson recalled")
	}
	l, err = d.ReviewLesson(a.ID, l.ID, l.Revision, "rejected", true, "建议不适用", 30)
	if err != nil || l.Shared {
		t.Fatalf("rejected lesson remains shared: %v %v", l, err)
	}
}

func TestLessonsEvidenceMutationAndBoundedRecall(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	task, es, id := lessonFixture(t, d)
	in := LessonInput{Tool: "Write", Summary: "真实错误", Resolution: "先核对工具契约再重试", EvidenceIDs: []int64{id}}
	l, err := es.ProposeLesson(task.ID, "work#1", in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE activity SET detail='changed' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReviewLesson(task.ID, l.ID, l.Revision, "accepted", false, "", 30); err == nil {
		t.Fatal("changed evidence accepted")
	}
	for i := 0; i < 6; i++ {
		in.Summary = fmt.Sprintf("受控经验 %d", i)
		in.Resolution = strings.Repeat("保留适用条件和停止条件。", 80)
		l, err := es.ProposeLesson(task.ID, "work#1", in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.ReviewLesson(task.ID, l.ID, l.Revision, "accepted", false, "", 30); err != nil {
			t.Fatal(err)
		}
	}
	got, err := es.RecallLessons(task.ID, "Write", "")
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || len(wire) > 4096 || len(got.Lessons) == 0 {
		t.Fatalf("recall not bounded/explicit: %d bytes, %+v", len(wire), got)
	}
}
