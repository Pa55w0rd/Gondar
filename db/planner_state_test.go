package db

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestArchivedPlannerStateMissingIsCompatible(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`[]`), json.RawMessage(`null`)} {
		got, err := fenceArchivedPlannerState(raw)
		if err != nil || string(got) != "[]" {
			t.Fatalf("legacy/empty state=%s err=%v", got, err)
		}
	}
}

func TestPlannerStateRevisionAndArchiveRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("planner durable handoff", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID); d.DeleteTask(task.ID) })
	s := d.Exploration(task.ExplorationID)
	p, err := s.BeginPlannerRound()
	if err != nil {
		t.Fatal(err)
	}
	p.Todos = json.RawMessage(`[{"content":"verify synthetic evidence before dispatch","status":"pending"}]`)
	p.Summary, p.Outcome = "模型草稿，尚需核对图", "running"
	if ok, err := s.SavePlannerState(p); err != nil || !ok {
		t.Fatalf("save: %v %v", ok, err)
	}
	next, err := s.BeginPlannerRound()
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision != p.Revision+1 || next.Summary != p.Summary || string(next.Todos) != string(p.Todos) && !strings.Contains(string(next.Todos), "verify synthetic evidence") {
		t.Fatalf("handoff not restored: %+v", next)
	}
	if ok, err := s.SavePlannerState(p); err != nil || ok {
		t.Fatalf("stale round overwrote current state: %v %v", ok, err)
	}
	for _, raw := range []string{`[{"content":"x","status":"invalid"}]`, `[{"content":"` + strings.Repeat("字", 513) + `","status":"pending"}]`} {
		bad := next
		bad.Todos = json.RawMessage(raw)
		if _, err := s.SavePlannerState(bad); err == nil {
			t.Fatal("accepted unbounded/invalid handoff")
		}
	}
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	a, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginPlannerRound(); !errors.Is(err, ErrTaskExtensionFrozen) {
		t.Fatalf("late planner round entered archive freeze: %v", err)
	}
	if _, err := s.SavePlannerState(next); !errors.Is(err, ErrTaskExtensionFrozen) {
		t.Fatalf("late planner handoff changed snapshot: %v", err)
	}
	if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DataCounts["exploration_planner_state"] != 1 {
		t.Fatal("planner state omitted from archive")
	}
	if err := d.CompleteTaskArchive(a.ID, snapshot, "synthetic.tar.zst", "test", 1, 1); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_planner_state WHERE exploration_id=$1`, s.ID()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("sensitive handoff remained in archived stub")
	}
	if _, err := s.BeginPlannerRound(); !errors.Is(err, ErrTaskExtensionUnavailable) {
		t.Fatalf("late round recreated hot state on cold stub: %v", err)
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
	// Even before any new planner round, a pre-archive writer must be fenced.
	if ok, err := s.SavePlannerState(next); err != nil || ok {
		t.Fatalf("pre-archive round revived: %v %v", ok, err)
	}
	restored, err := s.BeginPlannerRound()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored.Todos), "verify synthetic evidence") || restored.Summary != p.Summary || restored.Outcome != "canceled" {
		t.Fatalf("restore lost handoff: %+v", restored)
	}
}

func TestSettleInterruptedIntentRespectsTerminalAndCAS(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, status := range []string{"running", "done", "failed", "timeout"} {
		task, err := d.CreateTask("interruption "+status, "synthetic", nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.DeleteTask(task.ID) })
		s := d.Exploration(task.ExplorationID)
		id, err := s.AddIntent(map[string]any{"summary": "recoverable"}, 5, nil, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetIntentState(id, "running"); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(`UPDATE tasks SET status=$1 WHERE id=$2`, status, task.ID); err != nil {
			t.Fatal(err)
		}
		state, changed, err := s.SettleInterruptedIntent(id)
		want := "stopped"
		if status == "running" {
			want = "open"
		}
		if err != nil || !changed || state != want {
			t.Fatalf("status=%s state=%s changed=%v err=%v", status, state, changed, err)
		}
		if _, changed, err := s.SettleInterruptedIntent(id); err != nil || changed {
			t.Fatalf("stale settlement mutated intent: %v %v", changed, err)
		}
	}
}
