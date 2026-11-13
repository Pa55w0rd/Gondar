package server

import (
	"encoding/json"
	"strconv"
	"testing"

	"gondar/agent"
	"gondar/db"
)

func TestPlannerWaitPersistsReasonWithoutCountingFaultOrDiscardingProgress(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTaskWithOptions(t.Name(), "synthetic only", db.TaskCreateOptions{CompletionMode: db.CompletionContinuous})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	aid, err := task.Store.AppendActivity(db.Activity{Kind: "tool_result", Summary: "synthetic login prerequisite", Detail: "local fixture 401"})
	if err != nil {
		t.Fatal(err)
	}
	write, err := task.Store.UpsertVerification(t.Context(), db.VerificationUpdate{Target: db.VerificationTarget{Endpoint: "https://owned.invalid/items", Method: "GET", Input: "none", Identity: "anonymous", Technique: "fixture"}, State: db.VerificationMissingPrerequisite, Summary: "missing account", Observation: "only anonymous observation", MissingPrerequisites: []string{"controlled account"}, Evidence: []db.VerificationEvidence{{Role: "blocker", ActivityID: aid, Observation: "synthetic 401"}}}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	wait := agent.PlannerWait{Kind: "missing_prerequisite", Reason: "等待受控测试账号", VerificationIDs: []string{write.Record.ID}, RemainingSteps: []string{"核对登录后的资源归属"}}
	e := NewEngine(m)
	for i := 0; i < 2; i++ {
		e.recordPlannerOutcome(t.Context(), task, int64(i+1), "planner_no_progress")
	}
	runErr := &agent.PlannerWaitError{Wait: wait}
	if plannerFailureCode(runErr, false) != "planner_waiting" {
		t.Fatal("wait classified as fault")
	}
	e.recordPlannerOutcome(t.Context(), task, 3, "planner_waiting", runErr)
	stored, err := m.pg.GetTask(id)
	if err != nil || !stored.Paused || stored.Status == "done" || stored.Status == "failed" {
		t.Fatalf("wait lost lifecycle: %+v %v", stored, err)
	}
	if _, streak, err := task.Store.PlannerFailureStreak(t.Context()); err != nil || streak != 0 {
		t.Fatalf("wait counted repeated fault: %d %v", streak, err)
	}
	dto := taskDTO(task, "paused")
	var actual agent.PlannerWait
	if err := json.Unmarshal(dto.PlannerWait, &actual); err != nil || actual.Reason != wait.Reason {
		t.Fatalf("wait not visible in API: %s %v", dto.PlannerWait, err)
	}
	page, err := task.Store.ListVerifications(t.Context(), db.VerificationFilter{})
	if err != nil || len(page.Cases) != 1 {
		t.Fatal("wait discarded evidence")
	}
}

func TestPlannerWaitCannotOverrideRunningIntent(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask(t.Name(), "synthetic only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	if _, err := task.Store.AddIntent(map[string]any{"summary": "local fixture"}, 1, nil, "fixture"); err != nil {
		t.Fatal(err)
	}
	if won, err := m.pg.PauseTaskForPlannerWait(t.Context(), id); err != nil || won {
		t.Fatalf("waiting overrode executable work: %v %v", won, err)
	}
}

func TestExecutionToolUpgradePreservesDisabledAndRoleBindings(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	pg := m.pg
	const key = "record_verification"
	old, err := pg.GetTool(key)
	if err != nil {
		t.Fatal(err)
	}
	wasAbsent := old == nil
	if wasAbsent {
		for _, seed := range agent.BuiltinToolSeeds() {
			if seed.Key == key {
				schema, _ := json.Marshal(seed.Schema)
				roles, _ := json.Marshal(seed.Agents)
				if err := pg.SeedTool(key, seed.Desc, schema, roles); err != nil {
					t.Fatal(err)
				}
			}
		}
		old, err = pg.GetTool(key)
		if err != nil || old == nil {
			t.Fatal("fixture seed failed")
		}
	}
	agentsRaw, _ := json.Marshal(old.Agents)
	oldFlag, hadFlag, _ := pg.GetSetting("execution_tool_contracts_v033")
	defer func() {
		if wasAbsent {
			defer func() { _, _ = pg.Exec(`DELETE FROM tools WHERE key=$1`, key) }()
		}
		_ = pg.RefreshToolDefaults(key, old.Description, old.Schema)
		_, _ = pg.Exec(`UPDATE tools SET enabled=$2,agents=$3 WHERE key=$1`, key, old.Enabled, string(agentsRaw))
		if hadFlag {
			_ = pg.SetSetting("execution_tool_contracts_v033", oldFlag)
		} else {
			_, _ = pg.Exec(`DELETE FROM settings WHERE key='execution_tool_contracts_v033'`)
		}
	}()
	_, err = pg.Exec(`UPDATE tools SET enabled=false,agents='["worker"]',schema='{}' WHERE key=$1`, key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pg.Exec(`DELETE FROM settings WHERE key='execution_tool_contracts_v033'`)
	if err != nil {
		t.Fatal(err)
	}
	if err := refreshExecutionToolContracts(pg); err != nil {
		t.Fatal(err)
	}
	row, err := pg.GetTool(key)
	if err != nil || row.Enabled || len(row.Agents) != 1 || row.Agents[0] != "worker" {
		t.Fatalf("tool preferences overwritten: %+v %v", row, err)
	}
	var schema map[string]any
	_ = json.Unmarshal(row.Schema, &schema)
	if schema["allOf"] == nil {
		t.Fatal("old schema prevented contract upgrade")
	}
}
