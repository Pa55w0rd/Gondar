package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gondar/db"
)

type artifactProgressReceipt struct {
	rec     *db.TaskArtifact
	created bool
	err     error
}
type artifactProgressFixture struct {
	receipts         []artifactProgressReceipt
	calls            int
	taskID, intentID int64
}

func (f *artifactProgressFixture) RecordArtifact(taskID, findingID int64, runDir, relPath, name, kind, sourceURL, note string) (*db.TaskArtifact, error) {
	rec, _, err := f.RecordArtifactWithProgress(taskID, findingID, runDir, relPath, name, kind, sourceURL, note)
	return rec, err
}
func (f *artifactProgressFixture) RecordArtifactWithProgress(taskID, findingID int64, runDir, relPath, name, kind, sourceURL, note string) (*db.TaskArtifact, bool, error) {
	f.taskID = taskID
	r := f.receipts[f.calls]
	f.calls++
	return r.rec, r.created, r.err
}
func (f *artifactProgressFixture) RecordArtifactForIntentWithProgress(taskID, intentID, findingID int64, runDir, relPath, name, kind, sourceURL, note string) (*db.TaskArtifact, bool, error) {
	f.intentID = intentID
	return f.RecordArtifactWithProgress(taskID, findingID, runDir, relPath, name, kind, sourceURL, note)
}

func TestArtifactProgressCountsOnlyNewContentReceipt(t *testing.T) {
	host := &artifactProgressFixture{receipts: []artifactProgressReceipt{
		{rec: &db.TaskArtifact{ID: 1, SHA256: "content-a"}, created: true},
		{rec: &db.TaskArtifact{ID: 1, SHA256: "content-a"}, created: false},
		{rec: &db.TaskArtifact{ID: 2, SHA256: "content-a"}, created: true},
		{rec: &db.TaskArtifact{ID: 1, SHA256: "content-b"}, created: true},
		{rec: &db.TaskArtifact{ID: 3}, created: true},
		{rec: &db.TaskArtifact{ID: 3}, created: true},
	}}
	ts := NewToolSet(nil, "fixture")
	ts.SetTaskID(7)
	ts.SetRunDir(t.TempDir())
	ts.SetArtifactRecorder(artifactRecorderForIntent(host, 7, 11))
	notifications := 0
	ts.notifyProgress = func() { notifications++ }
	wants := []int{1, 1, 1, 2, 3, 3}
	for i, want := range wants {
		res, err := ts.saveEvidence().Call(t.Context(), json.RawMessage(`{"path":"proof.txt","intent_id":999999}`), nil)
		if err != nil || res.IsError || ts.Writes().Artifacts != want || notifications != want {
			t.Fatalf("第%d次登记的进展错误：writes=%+v notifications=%d result=%+v err=%v", i, ts.Writes(), notifications, res, err)
		}
	}
	if host.taskID != 7 || host.intentID != 11 {
		t.Fatalf("模型覆盖了宿主身份：%+v", host)
	}
	if ts.Writes().Total() != 3 {
		t.Fatalf("附件未纳入保存进展：%+v", ts.Writes())
	}
}

type legacyArtifactFixture struct{}

func (legacyArtifactFixture) RecordArtifact(taskID, findingID int64, runDir, relPath, name, kind, sourceURL, note string) (*db.TaskArtifact, error) {
	return &db.TaskArtifact{ID: 9, SHA256: "already-existing"}, nil
}
func TestArtifactWithoutAtomicReceiptDoesNotInventProgress(t *testing.T) {
	ts := NewToolSet(nil, "fixture")
	ts.SetTaskID(7)
	ts.SetRunDir(t.TempDir())
	ts.SetArtifactRecorder(artifactRecorderForIntent(legacyArtifactFixture{}, 7, 11))
	res, err := ts.saveEvidence().Call(t.Context(), json.RawMessage(`{"path":"proof.txt"}`), nil)
	if err != nil || res.IsError || ts.Writes().Total() != 0 {
		t.Fatalf("旧回执被冒充新增：%+v %v", ts.Writes(), err)
	}
}

func TestArtifactInvalidReceiptDoesNotCountProgress(t *testing.T) {
	ts := NewToolSet(nil, "fixture")
	ts.SetTaskID(7)
	ts.SetRunDir(t.TempDir())
	ts.SetArtifactRecorder(&artifactProgressFixture{receipts: []artifactProgressReceipt{{created: true}}})
	res, err := ts.saveEvidence().Call(t.Context(), json.RawMessage(`{"path":"proof.txt"}`), nil)
	if err != nil || !res.IsError || ts.Writes().Total() != 0 {
		t.Fatalf("无效回执被计作成果：%+v %v", ts.Writes(), err)
	}
}

// TestListArtifactsResolvesTaskFromFinding v0.4.20：reporter 没有任务上下文，
// list_artifacts 必须能用 finding_id 反查任务，否则永远返回空列表。
func TestListArtifactsResolvesTaskFromFinding(t *testing.T) {
	oldList, oldFind := ListTaskArtifacts, FindingTaskID
	defer func() { ListTaskArtifacts, FindingTaskID = oldList, oldFind }()

	var gotTask string
	ListTaskArtifacts = func(taskID string) ([]db.TaskArtifact, error) {
		gotTask = taskID
		return []db.TaskArtifact{{ID: 9, TaskID: taskID, Name: "proof.png", Kind: "image"}}, nil
	}
	FindingTaskID = func(findingID int64) (string, error) {
		if findingID != 42 {
			t.Fatalf("findingID=%d want 42", findingID)
		}
		return "7", nil
	}

	ts := NewToolSet(nil, "reporter") // 刻意不 SetTaskID：模拟 reporter 的 chat 工具集
	tool := ts.listArtifacts()
	in, _ := json.Marshal(map[string]any{"finding_id": 42})
	res, err := tool.Call(context.Background(), in, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if gotTask != "7" {
		t.Fatalf("taskID=%q want 7", gotTask)
	}
	if !strings.Contains(toolResultText(t, res), "proof.png") {
		t.Fatalf("result missing artifact: %s", toolResultText(t, res))
	}
}

// TestListArtifactsWithoutContextStaysEmpty：没有 taskID 也没有 finding_id 时
// 不应误查其他任务（返回空列表而不是报错）。
func TestListArtifactsWithoutContextStaysEmpty(t *testing.T) {
	oldList := ListTaskArtifacts
	defer func() { ListTaskArtifacts = oldList }()
	ListTaskArtifacts = func(taskID string) ([]db.TaskArtifact, error) {
		t.Fatalf("must not query without task context, got %q", taskID)
		return nil, nil
	}
	ts := NewToolSet(nil, "reporter")
	tool := ts.listArtifacts()
	res, err := tool.Call(context.Background(), []byte(`{}`), nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if strings.TrimSpace(toolResultText(t, res)) != "[]" {
		t.Fatalf("want [], got %s", toolResultText(t, res))
	}
}
