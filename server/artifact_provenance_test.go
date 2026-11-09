package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"gondar/db"
)

func TestPojunArtifactHostPersistsIntentWithoutPathInference(t *testing.T) {
	s, _, taskID := artifactContentFixture(t)
	pg := s.m.pg
	task, err := pg.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	es := pg.Exploration(task.ExplorationID)
	iid, err := es.AddIntent(map[string]any{"summary": "real producer"}, 1, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(s.m.dir, "tasks", strconv.FormatInt(taskID, 10), "directory-with-no-intent-id")
	if err := os.MkdirAll(runDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "proof.txt"), []byte("new synthetic host proof"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := s.RecordArtifactForIntent(taskID, iid, 0, runDir, "proof.txt", "proof.txt", "log", "", "synthetic")
	if err != nil || a.IntentID != iid || a.FindingID != 0 {
		t.Fatalf("host provenance not persisted: %+v %v", a, err)
	}
	// The legacy recorder has no producer context. A suggestive path alone must
	// not manufacture an intent identity for historic files or main-agent output.
	fakePath := filepath.Join(runDir, fmt.Sprintf("i%d", iid))
	if err := os.MkdirAll(fakePath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakePath, "legacy.txt"), []byte("legacy source deliberately unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	legacy, err := s.RecordArtifact(taskID, 0, fakePath, "legacy.txt", "legacy.txt", "log", "", "")
	if err != nil || legacy.IntentID != 0 {
		t.Fatalf("path created provenance: %+v %v", legacy, err)
	}
	f1, err := pg.AddFinding(taskID, 0, "fixture", "manual first", "low", "summary", "synthetic", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := pg.AddFinding(taskID, 0, "fixture", "manual second", "low", "summary", "synthetic", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.UpdateTaskArtifactFinding(a.ID, f1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordArtifactForIntent(taskID, iid, f2, runDir, "proof.txt", "proof.txt", "log", "", ""); !errors.Is(err, db.ErrArtifactAlreadyBound) {
		t.Fatalf("dedup silently stole/accepted other binding: %v", err)
	}
	if err := pg.SetPaused(taskID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.QueueTaskArchive(taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordArtifactForIntent(taskID, iid, f1, runDir, "proof.txt", "proof.txt", "log", "", ""); !errors.Is(err, db.ErrTaskExtensionFrozen) {
		t.Fatalf("dedup bypassed archive freeze: %v", err)
	}
}
