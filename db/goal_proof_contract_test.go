package db

import (
	"encoding/json"
	"testing"
)

func TestLegacyUnverifiedGoalRemainsVisibleWithoutRewritingHistory(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask(t.Name(), "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	id, err := s.AddNode(KindGoal, map[string]any{"text": "retained goal", "unverified": true, "unverified_reason": "legacy fallback"}, 0, "met", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := s.GetNode(id)
	if open, err := s.HasOpenGoal(); err != nil || !open {
		t.Fatalf("legacy goal hid from planner: %v %v", open, err)
	}
	if total, unmet, err := s.GoalCompletionCounts(t.Context()); err != nil || total != 1 || unmet != 1 {
		t.Fatalf("legacy goal counted proved: %d %d %v", total, unmet, err)
	}
	if view := GoalOverview(original); view["state"] != "met" || view["proof_status"] != "unverified" {
		t.Fatalf("proof status hidden: %+v", view)
	}
	metrics, err := d.TaskListMetricsAll()
	if err != nil || metrics[task.ExplorationID].Goals.Met != 0 {
		t.Fatalf("list called unverified proved: %+v %v", metrics, err)
	}
	retained, _ := s.GetNode(id)
	if string(retained.Payload) != string(original.Payload) || retained.State != original.State {
		t.Fatal("read-time interpretation rewrote history")
	}
	fact, err := s.AddNode(KindFact, map[string]any{"summary": "controlled proof", "evidence": "local fixture evidence"}, 0, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ProveGoal(id, fact, original.Payload, "new controlled proof"); err != nil {
		t.Fatal(err)
	}
	proved, _ := s.GetNode(id)
	if GoalOverview(proved)["proof_status"] != "proved" {
		t.Fatal("actual proof not recognized")
	}
	if _, unmet, err := s.GoalCompletionCounts(t.Context()); err != nil || unmet != 0 {
		t.Fatal("actual proof not counted")
	}
	var payload map[string]any
	_ = json.Unmarshal(proved.Payload, &payload)
	if payload["unverified"] != nil {
		t.Fatal("new proof kept stale unverified marker")
	}
}

func TestPlannerWaitEventAndPauseCommitTogether(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask(t.Name(), "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	bad := Activity{Summary: "uncommitted wait", Metadata: json.RawMessage(`{`)}
	if won, err := d.PauseTaskForPlannerWait(t.Context(), task.ID, &bad); err == nil || won {
		t.Fatal("invalid wait event did not roll back pause")
	}
	stored, err := d.GetTask(task.ID)
	if err != nil || stored.Paused {
		t.Fatal("failed wait write left a mysterious paused task")
	}
	good := Activity{Summary: "等待前提", Metadata: json.RawMessage(`{"planner_wait":{"kind":"missing_prerequisite","reason":"controlled account required"}}`)}
	if won, err := d.PauseTaskForPlannerWait(t.Context(), task.ID, &good); err != nil || !won || good.ID <= 0 {
		t.Fatalf("atomic wait commit failed %v %v", won, err)
	}
	raw, err := d.Exploration(task.ExplorationID).LatestPlannerWait()
	if err != nil || len(raw) == 0 {
		t.Fatal("committed pause lost its restart reason")
	}
}
