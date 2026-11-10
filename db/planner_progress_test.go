package db

import (
	"encoding/json"
	"testing"
)

func TestPlannerOutcomesSurviveConnectionAndResetOnSuccess(t *testing.T) {
	dsn := testDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("persisted planner failures", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	appendOutcome := func(code string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"planner_outcome": PlannerOutcome{Version: 1, Code: code}})
		if _, err := s.AppendActivity(Activity{Worker: "planner", Kind: "text", Metadata: raw}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		appendOutcome("planner_model_unavailable")
	}
	d2, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	code, count, err := d2.Exploration(task.ExplorationID).PlannerFailureStreak(t.Context())
	if err != nil || count != 3 || code != "planner_model_unavailable" {
		t.Fatalf("lost persisted failures: %s %d %v", code, count, err)
	}
	appendOutcome("")
	appendOutcome("planner_model_unavailable")
	_, count, err = s.PlannerFailureStreak(t.Context())
	if err != nil || count != 1 {
		t.Fatalf("success did not reset streak: %d %v", count, err)
	}
	appendOutcome("planner_no_progress")
	code, count, err = s.PlannerFailureStreak(t.Context())
	if err != nil || count != 2 || code != "planner_repeated_failure" {
		t.Fatalf("alternating failures bypassed protection: %s %d %v", code, count, err)
	}
	appendOutcome("planner_read_loop")
	code, count, err = s.PlannerFailureStreak(t.Context())
	if err != nil || count != 3 || code != "planner_repeated_failure" {
		t.Fatalf("mixed third failure not counted: %s %d %v", code, count, err)
	}
	if _, err := d.Exec(`UPDATE tasks SET first_run_at=now() WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	_, count, err = s.PlannerFailureStreak(t.Context())
	if err != nil || count != 0 {
		t.Fatalf("new execution inherited old failures: %d %v", count, err)
	}
	appendOutcome("planner_no_progress")
	_, count, err = s.PlannerFailureStreak(t.Context())
	if err != nil || count != 1 {
		t.Fatalf("new execution failure not counted: %d %v", count, err)
	}
	appendOutcome("task_deadline")
	_, count, err = s.PlannerFailureStreak(t.Context())
	if err != nil || count != 0 {
		t.Fatalf("task budget counted as model failure: %d %v", count, err)
	}
}

func TestPlannerFailureCannotOverrideUserPause(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("paused planner guard", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	if won, err := d.FailPlannerTask(task.ID); err != nil || won {
		t.Fatalf("paused task overwritten: %v %v", won, err)
	}
	stored, err := d.GetTask(task.ID)
	if err != nil || !stored.Paused || stored.Status == "failed" {
		t.Fatalf("pause lost: %+v %v", stored, err)
	}
}
