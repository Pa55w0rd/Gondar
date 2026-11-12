package agent

import (
	"context"
	"encoding/json"
	"testing"

	"gondar/db"
)

func TestVerificationToolsRefuseMissingTask(t *testing.T) {
	tsx := NewToolSet(nil, "")
	for _, tool := range []string{"record", "list"} {
		var resultError bool
		if tool == "record" {
			result, err := tsx.RecordVerificationTool().Call(t.Context(), json.RawMessage(`{}`), nil)
			if err != nil {
				t.Fatal(err)
			}
			resultError = result.IsError
		} else {
			result, err := tsx.ListVerificationsTool().Call(t.Context(), json.RawMessage(`{}`), nil)
			if err != nil {
				t.Fatal(err)
			}
			resultError = result.IsError
		}
		if !resultError {
			t.Fatalf("%s accepted a non-task invocation", tool)
		}
	}
	notifications := 0
	ctx := WithProgressNotify(context.Background(), func() { notifications++ })
	notify := ProgressNotifyFromContext(ctx)
	if notify == nil {
		t.Fatal("progress callback missing from context")
	}
	notify()
	if notifications != 1 || ProgressNotifyFromContext(context.Background()) != nil || ProgressNotifyFromContext(nil) != nil {
		t.Fatal("progress callback context leaked or was not invoked")
	}
}

func TestVerificationToolCatalogBindings(t *testing.T) {
	seeds := map[string]ToolSeed{}
	for _, seed := range BuiltinToolSeeds() {
		seeds[seed.Key] = seed
	}
	for _, name := range []string{"record_verification", "list_verifications"} {
		seed, found := seeds[name]
		if !found {
			t.Fatalf("%s has an implementation but no tool catalog seed", name)
		}
		for _, role := range []string{"planner", "worker", "mainagent"} {
			bound := false
			for _, agent := range seed.Agents {
				bound = bound || agent == role
			}
			if !bound {
				t.Errorf("%s is not exposed to %s", name, role)
			}
		}
	}
}

func TestVerificationToolsNotifyCommittedSemanticChanges(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTask(t.Name(), "保留受控证据并通知规划者", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	store := d.Exploration(task.ExplorationID)
	activity, err := store.AppendActivity(db.Activity{Kind: "tool_result", Summary: "需要有效登录态", Detail: "HTTP 302 /login"})
	if err != nil {
		t.Fatal(err)
	}
	tools := NewToolSet(store, "worker")
	tools.SetTaskID(task.ID)
	var notifications int
	tools.SetNotifyProgress(func() {
		page, err := store.ListVerifications(t.Context(), db.VerificationFilter{})
		if err != nil || len(page.Cases) != 1 {
			t.Errorf("notification preceded persistence: %+v %v", page, err)
		}
		notifications++
	})
	target := db.VerificationTarget{Endpoint: "https://owned.invalid/api/items", Method: "GET",
		Input: "query.search", Identity: "controlled-user/object:1", Technique: "query-expression", Variant: "single_quote.true"}
	input := db.VerificationUpdate{Target: target, State: db.VerificationNotTested, Summary: "待验证的一个输入格"}
	call := func(in db.VerificationUpdate, wantError bool) map[string]any {
		t.Helper()
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		result, err := tools.RecordVerificationTool().Call(t.Context(), raw, nil)
		if err != nil || result.IsError != wantError {
			t.Fatalf("tool error=%v transport=%v text=%s", result.IsError, err, result.Flatten())
		}
		if wantError {
			return nil
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(result.Flatten()), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := call(input, false)
	if first["changed"] != true || first["progress"] != false || tools.Writes().Total() != 0 || notifications != 0 {
		t.Fatalf("inventory refreshed progress: %+v counts=%+v notify=%d", first, tools.Writes(), notifications)
	}
	one := int64(1)
	input.ExpectedRevision = &one
	input.State = db.VerificationInconclusive
	input.Observation = "受控账号登录态失效，当前格尚未完成"
	input.Evidence = []db.VerificationEvidence{{Role: "test", Observation: "HTTP 302 /login", ActivityID: activity}}
	changed := call(input, false)
	if changed["revision"] != float64(2) || changed["progress"] != true || tools.Writes().Facts != 1 || notifications != 1 {
		t.Fatalf("new observation not delivered exactly once: %+v counts=%+v notify=%d", changed, tools.Writes(), notifications)
	}
	retry := input
	retry.ObservedAt = "2026-11-12T18:00:00+08:00"
	retry.Summary = "展示标题发生变化"
	again := call(retry, false)
	if again["changed"] != false || again["progress"] != false || tools.Writes().Facts != 1 || notifications != 1 {
		t.Fatalf("retry notified or inflated progress: %+v counts=%+v notify=%d", again, tools.Writes(), notifications)
	}
	input.Observation = "另一条观察，但仍持有旧修订号"
	call(input, true)
	if tools.Writes().Facts != 1 || notifications != 1 {
		t.Fatal("failed CAS notified or refreshed progress")
	}
	result, err := tools.ListVerificationsTool().Call(t.Context(), json.RawMessage(`{"include_history":true}`), nil)
	if err != nil || !result.IsError {
		t.Fatal("unbounded cross-case history accepted")
	}
	result, err = tools.ListVerificationsTool().Call(t.Context(), json.RawMessage(`{}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("list failed: %s %v", result.Flatten(), err)
	}
	var page struct {
		Cases         []map[string]any `json:"cases"`
		RecordedCases int              `json:"recorded_cases"`
		Unrecorded    string           `json:"unrecorded_cases"`
	}
	if err := json.Unmarshal([]byte(result.Flatten()), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Cases) != 1 || page.RecordedCases != 1 || page.Unrecorded != "unknown" || page.Cases[0]["evidence"] != nil {
		t.Fatalf("list double-counted revision or dumped full evidence: %+v", page)
	}
}
