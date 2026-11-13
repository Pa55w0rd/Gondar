package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gondar/db"
)

func TestTaskFileReadersOnlyBlockDeletionAndCaptureCounterGeneration(t *testing.T) {
	e := &Engine{}
	release, ok := e.beginTaskFileRead("7")
	if !ok || e.taskFileReadCount("7") != 1 || e.inflightCount("7") != 0 {
		t.Fatal("file reader became an agent execution")
	}
	s := &Server{engine: e}
	if !e.BeginDelete("7") {
		t.Fatal("delete barrier was not installed")
	}
	if _, ok := e.beginTaskFileRead("7"); ok {
		t.Fatal("delete barrier admitted a new file reader")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.waitTaskQuiescent(ctx, "7"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deletion did not wait on admitted reader: %v", err)
	}
	release()
	if err := s.waitTaskQuiescent(context.Background(), "7"); err != nil {
		t.Fatal(err)
	}
	if e.taskFileReadCount("7") != 0 || e.inflightCount("7") != 0 {
		t.Fatal("reader counters did not drain independently")
	}

	oldRelease, ok := e.beginTaskFileRead("generation")
	if !ok {
		t.Fatal("old generation was not admitted")
	}
	e.taskReads.Delete("generation") // the StopTask cleanup generation is gone
	newRelease, ok := e.beginTaskFileRead("generation")
	if !ok {
		t.Fatal("new generation was not admitted")
	}
	oldRelease()
	oldRelease() // releases are idempotent and still reference the old pointer
	if e.taskFileReadCount("generation") != 1 {
		t.Fatal("old reader release decremented a replacement generation")
	}
	newRelease()
	if e.taskFileReadCount("generation") != 0 {
		t.Fatal("new generation did not release")
	}
}

func TestTaskFileReadDoesNotBlockStoppedTaskConfigOrGoalEditing(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask("read/edit synthetic fixture", "不访问目标、不调用模型", nil, 120, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer m.pg.DeleteTaskCascade(id, true, true)
	goal, err := task.Store.AddGoal(map[string]any{"text": "saved goal"}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetTaskStatus(task.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	s := newAdmissionTestServer(m, nil) // canceled engine context: no model/worker starts
	release, ok := s.engine.beginTaskFileRead(task.ID)
	if !ok {
		t.Fatal("reader was not admitted")
	}
	defer release()
	before := task.lifecycleSnapshot()
	if s.engine.inflightCount(task.ID) != 0 || s.engine.taskFileReadCount(task.ID) != 1 || before.Status != "timeout" {
		t.Fatal("read admission changed execution/lifecycle state")
	}
	request := httptest.NewRequest("PATCH", "/api/tasks/"+task.ID+"/execution", strings.NewReader(`{"completion_mode":"continuous","timeout_seconds":0}`))
	request.SetPathValue("id", task.ID)
	out := httptest.NewRecorder()
	s.updateTaskExecution(out, request)
	if out.Code != 200 {
		t.Fatalf("active download blocked stopped task config: %d %s", out.Code, out.Body.String())
	}
	if task.lifecycleSnapshot().Status != "timeout" {
		t.Fatal("config/read resumed stopped task")
	}
	request = httptest.NewRequest("PATCH", "/api/tasks/"+task.ID+"/goals/"+strconv.FormatInt(goal, 10), strings.NewReader(`{"text":"edited goal while file remains open"}`))
	request.SetPathValue("id", task.ID)
	request.SetPathValue("gid", strconv.FormatInt(goal, 10))
	out = httptest.NewRecorder()
	s.editGoal(out, request)
	if out.Code != 200 {
		t.Fatalf("active download blocked explicit goal editing: %d %s", out.Code, out.Body.String())
	}
	updated, err := task.Store.GetNode(goal)
	if err != nil || updated == nil || !strings.Contains(string(updated.Payload), "edited goal while file remains open") || s.engine.inflightCount(task.ID) != 0 || s.engine.taskFileReadCount(task.ID) != 1 {
		t.Fatal("goal edit or independent counters were not preserved")
	}
	if state := task.lifecycleSnapshot(); state.CompletionMode != db.CompletionContinuous || state.TimeoutSeconds != 0 {
		t.Fatal("explicit config was lost")
	}
}
