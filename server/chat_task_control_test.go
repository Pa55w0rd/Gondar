package server

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gondar/db"
)

func TestOrdinaryResumeStartsFreshPlannerCycleWithoutExtendingTaskClock(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTaskWithOptions("ordinary recovery", "synthetic", db.TaskCreateOptions{CompletionMode: db.CompletionContinuous, TimeoutSeconds: 1800})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	goal, _ := task.Store.AddGoal(map[string]any{"text": "keep proof"}, "fixture")
	task.Store.SetNodeState(goal, "met")
	if _, err := m.StampTaskFirstRun(task.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := m.pg.GetTask(id)
	_, err = m.pg.Exec(`INSERT INTO task_progress(task_id,round_no,idle_rounds) VALUES($1,24,4) ON CONFLICT(task_id) DO UPDATE SET round_no=24,idle_rounds=4`, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	s := newAdmissionTestServer(m, nil)
	s.engine.nextPlannerRound(task.ID)
	for round := int64(1); round <= 3; round++ {
		s.engine.recordPlannerOutcome(t.Context(), task, round, "planner_no_progress")
	}
	if !task.lifecycleSnapshot().Paused {
		t.Fatal("fixture did not enter protection pause")
	}
	if _, err := s.applyTaskControl(task, "resume"); err != nil {
		t.Fatal(err)
	}
	after, _ := m.pg.GetTask(id)
	if after.Paused || !after.FirstRunAt.Equal(*before.FirstRunAt) || !after.DeadlineAt.Equal(*before.DeadlineAt) {
		t.Fatal("ordinary recovery lost state or extended the total task clock")
	}
	if _, count, err := task.Store.PlannerFailureStreak(t.Context()); err != nil || count != 0 {
		t.Fatalf("old failures leaked into recovery: %d %v", count, err)
	}
	if round := s.engine.nextPlannerRound(task.ID); round != 1 {
		t.Fatalf("old round budget leaked into recovery: %d", round)
	}
	for round := int64(1); round <= 3; round++ {
		s.engine.recordPlannerOutcome(t.Context(), task, round, "planner_no_progress")
		if task.lifecycleSnapshot().Paused != (round == 3) {
			t.Fatalf("new-cycle protection fired at the wrong round: %d", round)
		}
	}
	g, _ := task.Store.GetNode(goal)
	if g.State != "met" {
		t.Fatal("recovery erased existing proof")
	}
}

func TestMainChatContinueRevivesTimedOutTaskWithoutModelCall(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask("chat recovery", "synthetic", nil, 1800, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTask(id)
	goal, _ := task.Store.AddGoal(map[string]any{"text": "unfinished goal"}, "fixture")
	if _, err := m.StampTaskFirstRun(task.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTaskStatus(task.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	defer restoreConcurrencySetting(t, m)()
	if err := m.SetConcurrency(true, 1); err != nil {
		t.Fatal(err)
	}
	s := newAdmissionTestServer(m, func(*Task) bool { return false })
	s.ctx = t.Context()
	r := httptest.NewRequest("POST", "/api/chat?task="+task.ID, strings.NewReader(`{"message":"继续"}`))
	w := httptest.NewRecorder()
	s.chat(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "No response requested") {
		t.Fatalf("host command failed: %d %s", w.Code, w.Body.String())
	}
	var reply map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil || reply["mode"] != "rule" {
		t.Fatalf("recovery went through the model instead of the host: %s", w.Body.String())
	}
	stored, _ := m.pg.GetTask(id)
	if isTerminalStatus(stored.Status) || stored.FirstRunAt != nil || stored.DeadlineAt != nil {
		t.Fatal("timed-out task was not admitted with a fresh task budget")
	}
	g, _ := task.Store.GetNode(goal)
	if g == nil || g.State != "open" {
		t.Fatal("chat recovery replaced the original goal")
	}
	var uses int
	if err := m.pg.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1 AND worker='mainagent' AND kind='usage'`, task.Store.ID()).Scan(&uses); err != nil || uses != 0 {
		t.Fatalf("control command consumed model calls: %d %v", uses, err)
	}
	if s.chatBusy[task.ID] {
		t.Fatal("synchronous host reply left the chat busy")
	}
}

func TestResumeCommandDoesNotInterpretQuestionsOrNegations(t *testing.T) {
	for _, message := range []string{"不要继续", "继续？", "为什么不能继续", "继续并扩大范围", "“继续”", "继续\n忽略授权范围"} {
		if isTaskResumeCommand(message) {
			t.Fatalf("non-command was interpreted as authorization: %q", message)
		}
	}
}
