package db

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func provenanceFixture(t *testing.T) (*DB, *Task, *Task, int64, int64) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.EnsureTaskArtifactsTable(); err != nil {
		t.Fatal(err)
	}
	create := func() *Task {
		task, err := d.CreateTask("artifact provenance fixture", "local synthetic", nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			d.Exec(`DELETE FROM task_artifacts WHERE task_id=$1`, strconv.FormatInt(task.ID, 10))
			d.DeleteTaskCascade(task.ID, true, true)
		})
		return task
	}
	a, b := create(), create()
	return d, a, b, mustIntent(t, d.Exploration(a.ExplorationID), "producer"), mustIntent(t, d.Exploration(b.ExplorationID), "other producer")
}

func TestPojunArtifactProvenanceOwnershipAndManualBinding(t *testing.T) {
	d, task, other, intent, foreign := provenanceFixture(t)
	es := d.Exploration(task.ExplorationID)
	f1, err := es.RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, IntentID: intent, VulnClass: "fixture", Name: "first", Severity: "low", Evidence: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	f2, err := es.RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, IntentID: intent, VulnClass: "fixture", Name: "second", Severity: "low", Evidence: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	foreignFinding, err := d.AddFinding(other.ID, 0, "fixture", "foreign", "low", "synthetic", "synthetic", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	newArtifact := func(i, f int64) *TaskArtifact {
		return &TaskArtifact{TaskID: strconv.FormatInt(task.ID, 10), IntentID: i, FindingID: f, Name: "proof.txt", Kind: "log", Storage: "synthetic-proof.txt"}
	}
	for _, a := range []*TaskArtifact{newArtifact(foreign, 0), newArtifact(intent, foreignFinding), newArtifact(f1.NodeID, 0)} {
		if err := d.InsertTaskArtifact(t.Context(), a); err == nil {
			t.Fatalf("invalid owner accepted: %+v", a)
		}
	}
	a := newArtifact(intent, 0)
	if err := d.InsertTaskArtifact(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	// Same-intent material is only a suggestion. Two findings share the intent;
	// neither is auto-bound or preferred as a proof claim.
	items, truncated, err := d.ArtifactFindingSuggestions(task.ID, a.ID)
	if err != nil || truncated || len(items) != 2 {
		t.Fatalf("suggestions=%+v truncated=%v err=%v", items, truncated, err)
	}
	stored, err := d.GetTaskArtifact(a.ID)
	if err != nil || stored.IntentID != intent || stored.FindingID != 0 {
		t.Fatalf("provenance changed binding: %+v %v", stored, err)
	}
	if _, err := d.UpdateTaskArtifactFinding(a.ID, foreignFinding); err == nil {
		t.Fatal("cross-task manual binding accepted")
	}
	if _, err := d.UpdateTaskArtifactFinding(a.ID, f2.FindingID); err != nil {
		t.Fatal(err)
	}
	// An automatic dedup backfill arriving after the human changed the binding
	// cannot steal it, even if its earlier list lookup observed finding_id=NULL.
	if _, err := d.UpdateTaskArtifactBinding(a.ID, f1.FindingID, "late automatic text", "https://different.invalid"); !errors.Is(err, ErrArtifactAlreadyBound) {
		t.Fatalf("late backfill overwrote human binding: %v", err)
	}
	stored, err = d.GetTaskArtifact(a.ID)
	if err != nil || stored.FindingID != f2.FindingID || stored.Note != "" || stored.IntentID != intent {
		t.Fatalf("manual association/provenance lost: %+v %v", stored, err)
	}
	if _, err := d.UpdateTaskArtifactFinding(a.ID, f1.FindingID); err != nil {
		t.Fatalf("explicit human rebind must still work: %v", err)
	}
	// Tampered historical references are not trusted by the suggestion reader.
	if _, err := d.Exec(`UPDATE task_artifacts SET intent_id=$2 WHERE id=$1`, a.ID, foreign); err != nil {
		t.Fatal(err)
	}
	items, _, err = d.ArtifactFindingSuggestions(task.ID, a.ID)
	if err != nil || len(items) != 0 {
		t.Fatalf("foreign historical intent leaked suggestions: %+v %v", items, err)
	}
}

func TestPojunArtifactProvenanceColdRoundTrip(t *testing.T) {
	d, task, other, intent, _ := provenanceFixture(t)
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	es := d.Exploration(task.ExplorationID)
	f, err := es.RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, IntentID: intent, VulnClass: "fixture", Name: "archive finding", Severity: "low", Evidence: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	a := &TaskArtifact{TaskID: strconv.FormatInt(task.ID, 10), IntentID: intent, FindingID: f.FindingID, Name: "proof.txt", Kind: "log", Storage: "synthetic"}
	if err := d.InsertTaskArtifact(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	archive, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpdateTaskArtifactFinding(a.ID, 0); !errors.Is(err, ErrTaskExtensionFrozen) {
		t.Fatalf("queued archive allowed rebind: %v", err)
	}
	if err := d.InsertTaskArtifact(t.Context(), &TaskArtifact{TaskID: a.TaskID, IntentID: intent, Name: "late.txt", Storage: "late"}); !errors.Is(err, ErrTaskExtensionFrozen) {
		t.Fatalf("queued archive allowed late insert: %v", err)
	}
	if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteTaskArchive(archive.ID, snapshot, "synthetic.tar.zst", "synthetic", 100, 50); err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetTaskArtifact(a.ID)
	if err != nil || stored.IntentID != intent {
		t.Fatalf("hot metadata provenance lost while cold: %+v %v", stored, err)
	}
	items, _, err := d.ArtifactFindingSuggestions(task.ID, a.ID)
	if err != nil || len(items) != 0 {
		t.Fatalf("cold task got suggestions: %+v %v", items, err)
	}
	if _, err := d.QueueTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A different graph deliberately occupies the original global node ID.
	// The existing restore contract aborts atomically instead of remapping an
	// intent to that other task and making retained metadata appear trustworthy.
	if _, err := d.Exec(`INSERT INTO exploration_nodes(id,exploration_id,kind,payload,priority,state,origin) VALUES($1,$2,'intent','{}',1,'open','fixture')`, intent, other.ExplorationID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RestoreTaskArchive(archive.ID, snapshot, 0); err == nil {
		t.Fatal("graph ID collision silently accepted")
	}
	if _, err := d.Exec(`DELETE FROM exploration_nodes WHERE id=$1 AND exploration_id=$2`, intent, other.ExplorationID); err != nil {
		t.Fatal(err)
	}
	warnings, err := d.RestoreTaskArchive(archive.ID, snapshot, 0)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("restore warnings=%v err=%v", warnings, err)
	}
	if err := d.CompleteTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	stored, err = d.GetTaskArtifact(a.ID)
	if err != nil || stored.IntentID != intent || stored.FindingID != f.FindingID {
		t.Fatalf("restored provenance mismatch: %+v %v", stored, err)
	}
	items, _, err = d.ArtifactFindingSuggestions(task.ID, a.ID)
	if err != nil || len(items) != 1 || items[0].FindingID != f.FindingID || !strings.Contains(items[0].Reason, "人工") {
		t.Fatalf("restored lineage not revalidated: %+v %v", items, err)
	}
}
