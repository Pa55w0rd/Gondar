package agent

import (
	"context"
	"encoding/json"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
	"gondar/db"
)

type provenanceRecorderFixture struct{ taskID, intentID, calls int64 }

func (f *provenanceRecorderFixture) RecordArtifact(taskID, findingID int64, runDir, relPath, name, kind, sourceURL, note string) (*db.TaskArtifact, error) {
	f.calls++
	return &db.TaskArtifact{ID: 1}, nil
}
func (f *provenanceRecorderFixture) RecordArtifactForIntent(taskID, intentID, findingID int64, runDir, relPath, name, kind, sourceURL, note string) (*db.TaskArtifact, error) {
	f.calls++
	f.taskID, f.intentID = taskID, intentID
	return &db.TaskArtifact{ID: 2, IntentID: intentID}, nil
}

func TestPojunWorkerArtifactUsesHostIdentityAndReadTools(t *testing.T) {
	host := &provenanceRecorderFixture{}
	ts := NewToolSet(nil, "fixture")
	ts.SetTaskID(7)
	ts.SetRunDir(t.TempDir())
	ts.SetArtifactRecorder(artifactRecorderForIntent(host, 7, 11))
	var save actool.CoreTool
	counts := map[string]int{}
	for _, tool := range ts.WorkerTools() {
		if tool.Name() == "save_evidence" {
			save = tool
		}
		switch tool.Name() {
		case "expand_digest", "list_untested_assets", "list_goals":
			counts[tool.Name()]++
			if !tool.IsReadOnly(json.RawMessage(`{}`)) || !tool.IsConcurrencySafe(json.RawMessage(`{}`)) {
				t.Fatalf("context tool %s became a write/serial tool", tool.Name())
			}
		}
	}
	for _, name := range []string{"expand_digest", "list_untested_assets", "list_goals"} {
		if counts[name] != 1 {
			t.Fatalf("worker tool %s count=%d", name, counts[name])
		}
	}
	if save == nil {
		t.Fatal("save_evidence missing")
	}
	res, err := save.Call(context.Background(), json.RawMessage(`{"path":"proof.txt","intent_id":999999}`), nil)
	if err != nil || res.IsError || host.calls != 1 || host.taskID != 7 || host.intentID != 11 {
		t.Fatalf("model overrode producer identity: host=%+v result=%+v err=%v", host, res, err)
	}
	if _, err := artifactRecorderForIntent(host, 7, 11).RecordArtifact(8, 0, "", "", "", "", "", ""); err == nil || host.calls != 1 {
		t.Fatal("recorder accepted another task")
	}
}
