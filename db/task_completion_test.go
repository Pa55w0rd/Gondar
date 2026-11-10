package db

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestCompletionExecutionPatchPreservesGraphAndRejectsLiveChange(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("execution patch", "synthetic only", nil, 60, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	goal, err := s.AddGoal(map[string]any{"text": "retained delivery"}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeState(goal, "met"); err != nil {
		t.Fatal(err)
	}
	mode := CompletionContinuous
	zero := 0
	if _, err := d.UpdateTaskExecution(task.ID, TaskExecutionPatch{CompletionMode: &mode}); !errors.Is(err, ErrTaskExecutionActive) {
		t.Fatalf("live mutation accepted: %v", err)
	}
	if _, err := d.StampFirstRun(task.ID, 60); err != nil {
		t.Fatal(err)
	}
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	updated, err := d.UpdateTaskExecution(task.ID, TaskExecutionPatch{CompletionMode: &mode, TimeoutSeconds: &zero})
	if err != nil || updated.CompletionMode != CompletionContinuous || updated.TimeoutSeconds != 0 || updated.FirstRunAt != nil || updated.DeadlineAt != nil {
		t.Fatalf("bad config/budget: %+v %v", updated, err)
	}
	g, err := s.GetNode(goal)
	if err != nil || g.State != "met" {
		t.Fatalf("history reset by config change: %+v %v", g, err)
	}
	if got, err := s.TaskCompletionMode(t.Context()); err != nil || got != CompletionContinuous {
		t.Fatalf("trusted policy missing: %s %v", got, err)
	}
	bad := "invented"
	if _, err := d.UpdateTaskExecution(task.ID, TaskExecutionPatch{CompletionMode: &bad}); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

func TestEditedGoalReopensWithoutLosingHistoryOrEvidence(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("goal revision", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	goal, _ := s.AddGoal(map[string]any{"text": "old definition", "unverified": true, "unverified_reason": "old claim", "custom": "retained"}, "fixture")
	evidence, err := s.AddNode(KindFact, map[string]any{"summary": "synthetic evidence"}, 0, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetNode(goal)
	if err := s.ProveGoal(goal, evidence, before.Payload, "old proof"); err != nil {
		t.Fatal(err)
	}
	proven, _ := s.GetNode(goal)
	var p map[string]any
	json.Unmarshal(proven.Payload, &p)
	if proven.State != "met" || p["unverified"] != nil {
		t.Fatalf("stale unverified kept: %s", proven.Payload)
	}
	if err := s.UpdateGoalPayload(goal, "new definition", ""); err != nil {
		t.Fatal(err)
	}
	updated, _ := s.GetNode(goal)
	p = make(map[string]any)
	json.Unmarshal(updated.Payload, &p)
	if updated.State != "open" || p["text"] != "new definition" || p["custom"] != "retained" || p["proof_evidence_id"] != nil {
		t.Fatalf("new definition reused old proof: %s %s", updated.State, updated.Payload)
	}
	history := p["goal_history"].([]any)
	if len(history) != 1 || history[0].(map[string]any)["state"] != "met" {
		t.Fatal("old goal history lost")
	}
	if err := s.ProveGoal(goal, evidence, proven.Payload, "stale definition"); err == nil {
		t.Fatal("old definition proof accepted")
	}
	var edges int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_edges WHERE exploration_id=$1 AND src_id=$2 AND dst_id=$3 AND rel='proves'`, task.ExplorationID, evidence, goal).Scan(&edges); err != nil || edges != 1 {
		t.Fatalf("old evidence link lost: %d %v", edges, err)
	}
	if err := s.UpdateGoalPayload(goal, "new definition", ""); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := s.GetNode(goal)
	if string(unchanged.Payload) != string(updated.Payload) {
		t.Fatal("unchanged edit manufactured another goal version")
	}
}

func TestGoalProofWriteFailureRollsBackState(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("atomic proof", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	goal, _ := s.AddGoal(map[string]any{"text": "still open"}, "fixture")
	g, _ := s.GetNode(goal)
	if err := s.ProveGoal(goal, 9223372036854770000, g.Payload, "invalid source"); err == nil {
		t.Fatal("missing evidence accepted")
	}
	after, _ := s.GetNode(goal)
	if after.State != "open" || string(after.Payload) != string(g.Payload) {
		t.Fatal("failed edge write left completed goal")
	}
}

func TestArchiveCompletionModeNeverSilentlyDropsPolicy(t *testing.T) {
	for version := 1; version <= 4; version++ {
		row := map[string]any{"id": 1}
		if err := normalizeArchivedCompletionMode(version, row); err != nil || row["completion_mode"] != CompletionObjective {
			t.Fatalf("legacy compatibility: %d %v", version, err)
		}
	}
	for _, value := range []any{nil, "", false, "invented"} {
		if err := normalizeArchivedCompletionMode(5, map[string]any{"completion_mode": value}); !errors.Is(err, ErrTaskArchiveFormatMismatch) {
			t.Fatalf("invalid current archive accepted: %v %v", value, err)
		}
	}
	row := map[string]any{"completion_mode": CompletionContinuous}
	if err := normalizeArchivedCompletionMode(5, row); err != nil || row["completion_mode"] != CompletionContinuous {
		t.Fatal("current policy not preserved")
	}
}
