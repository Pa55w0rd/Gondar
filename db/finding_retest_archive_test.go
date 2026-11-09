package db

import (
	"errors"
	"testing"
)

func TestFindingRetestArchiveHistoryAndExecutionFreeze(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask("retest archive", "controlled local case", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTaskCascade(task.ID, true, true)
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	fid, err := d.AddFinding(task.ID, 0, "controlled", "archive review", "low", "original", "proof", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	r, c, _, err := d.CreateFindingRetest(t.Context(), fid, "检查版本")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.QueueTaskArchive(task.ID); !errors.Is(err, ErrTaskArchiveIneligible) {
		t.Fatalf("active review not blocking archive: %v", err)
	}
	if err := d.FinishFindingRetest(r.ID, "stopped", "受控停止"); err != nil {
		t.Fatal(err)
	}
	a, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.CreateFindingRetest(t.Context(), fid, "late"); !errors.Is(err, ErrTaskExtensionFrozen) {
		t.Fatalf("late review accepted: %v", err)
	}
	if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DataCounts["finding_retests"] != 1 {
		t.Fatal("review history omitted")
	}
	if err := d.CompleteTaskArchive(a.ID, snapshot, "controlled-retest.tar.zst", "synthetic", 100, 50); err != nil {
		t.Fatal(err)
	}
	// Removing its detached conversation while cold must not destroy the audit.
	if err := d.DeleteConversation(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.QueueTaskArchiveRestore(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RestoreTaskArchive(a.ID, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteTaskArchiveRestore(a.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := d.ListFindingRetests(fid)
	if err != nil || len(rows) != 1 || rows[0].Status != "stopped" || rows[0].ConversationID != nil {
		t.Fatalf("audit round trip: %+v %v", rows, err)
	}
}
