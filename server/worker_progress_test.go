package server

import (
	"encoding/json"
	"gondar/agent"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRuntimeLimitsDistinguishUnlimitedRunAndCallProtection(t *testing.T) {
	for _, seconds := range []int{0, 1200, 3600} {
		limits := runtimeLimits(seconds, 7, true)
		call := seconds
		if call == 0 {
			call = 600
		}
		if limits["run_seconds"] != seconds || limits["model_call_seconds"] != call || limits["wrapup_turns"] != 7 {
			t.Fatalf("limits misrepresent actual configuration: %v", limits)
		}
	}
}

func TestWorkerProgressConfigPersistsAndRejectsInvalidPolicy(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	old, had, err := m.pg.GetSetting(workerProgressSetting)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = m.pg.SetSetting(workerProgressSetting, old)
		} else {
			_, _ = m.pg.Exec(`DELETE FROM settings WHERE key=$1`, workerProgressSetting)
		}
	})
	s := &Server{m: m}
	call := func(key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/agents/"+key+"/config", strings.NewReader(body))
		r.SetPathValue("key", key)
		w := httptest.NewRecorder()
		s.pgSaveAgentConfig(w, r)
		return w
	}
	w := call("worker", `{"progress_policy":{"save_reminder_seconds":0,"idle_rounds":5,"idle_seconds":240}}`)
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if got := s.workerProgressPolicy(); got.SaveReminderSeconds != 0 || got.IdleRounds != 5 || got.IdleSeconds != 240 {
		t.Fatalf("policy not persisted: %+v", got)
	}
	if got := call("worker", `{"progress_policy":{"save_reminder_seconds":0,"idle_rounds":0,"idle_seconds":240}}`); got.Code != 400 {
		t.Fatal("invalid protection silently accepted")
	}
	if got := call("planner", `{"progress_policy":{"save_reminder_seconds":0,"idle_rounds":5,"idle_seconds":240}}`); got.Code != 400 {
		t.Fatal("worker policy saved from unrelated role")
	}
	r := httptest.NewRequest("GET", "/api/agents/worker", nil)
	r.SetPathValue("key", "worker")
	view := httptest.NewRecorder()
	s.pgGetAgent(view, r)
	var detail struct {
		Policy agent.WorkerProgressPolicy `json:"progress_policy"`
		Limits map[string]any             `json:"runtime_limits"`
	}
	if view.Code != 200 || json.Unmarshal(view.Body.Bytes(), &detail) != nil || detail.Policy.IdleRounds != 5 || detail.Limits["model_call_seconds"] == nil {
		t.Fatalf("saved/effective configuration unavailable: %d", view.Code)
	}
}

func TestWorkerProgressPolicyValidationKeepsProtection(t *testing.T) {
	p := agent.DefaultWorkerProgressPolicy()
	p.SaveReminderSeconds = 0
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.IdleRounds = 0
	if p.Validate() == nil {
		t.Fatal("reminder-off must not silently disable no-progress protection")
	}
}
