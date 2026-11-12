package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func verificationInput() VerificationUpdate {
	return VerificationUpdate{
		Target: VerificationTarget{Endpoint: "https://owned.invalid/api/items", Method: "GET",
			Input: "query.search", Identity: "test-user/object:42", Technique: "query-expression", Variant: "single_quote.true"},
		State: VerificationInconclusive, Summary: "登录态不可用", Observation: "当前请求返回登录页，尚未验证表达式",
		Evidence: []VerificationEvidence{{Role: "test", Observation: "HTTP 302 跳转登录页", ActivityID: 1}},
	}
}

func TestVerificationIdentityAndSemanticRetries(t *testing.T) {
	in := verificationInput()
	_, id, digest, err := normalizeVerificationUpdate(in)
	if err != nil {
		t.Fatal(err)
	}
	retry := verificationInput()
	retry.Target.Method = " get "
	retry.Summary = "仅修改展示标题"
	retry.IntentID = 999
	retry.ObservedAt = "2026-11-12T14:00:00+08:00"
	retry.Evidence[0].ActivityID = 999
	_, retryID, retryDigest, err := normalizeVerificationUpdate(retry)
	if err != nil || retryID != id || retryDigest != digest {
		t.Fatalf("equivalent provenance/time replay changed semantic digest: %s %s %v", retryID, retryDigest, err)
	}
	for name, change := range map[string]func(*VerificationUpdate){
		"endpoint representation": func(v *VerificationUpdate) { v.Target.Endpoint += "/%2F" },
		"input":                   func(v *VerificationUpdate) { v.Target.Input = "json.search" },
		"identity":                func(v *VerificationUpdate) { v.Target.Identity = "other-user/object:42" },
		"method":                  func(v *VerificationUpdate) { v.Target.Method = "POST" },
		"variant":                 func(v *VerificationUpdate) { v.Target.Variant = "single_quote.false" },
		"technique":               func(v *VerificationUpdate) { v.Target.Technique = "object-authorization" },
	} {
		t.Run(name, func(t *testing.T) {
			other := verificationInput()
			change(&other)
			_, otherID, _, err := normalizeVerificationUpdate(other)
			if err != nil || otherID == id {
				t.Fatalf("distinct case merged: %q %v", otherID, err)
			}
		})
	}
	proof := VerificationEvidence{Role: "test", Observation: "  保留\r\n编码对照  ", ActivityID: 1}
	in.Evidence = []VerificationEvidence{proof}
	_, _, _, _ = normalizeVerificationUpdate(in)
	if in.Evidence[0] != proof {
		t.Fatal("normalization mutated caller's evidence slice")
	}
}

func TestVerificationStatesRequireBoundedProof(t *testing.T) {
	base := verificationInput()
	base.State = VerificationNegative
	base.PrerequisitesMet = true
	base.Evidence = append(base.Evidence,
		VerificationEvidence{Role: "baseline", Observation: "正常查询返回目标对象", ActivityID: 2},
		VerificationEvidence{Role: "control", Observation: "已知不匹配对照为空", ActivityID: 3})
	if _, _, _, err := normalizeVerificationUpdate(base); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*VerificationUpdate){
		"no prerequisites":        func(v *VerificationUpdate) { v.PrerequisitesMet = false },
		"blocked is not negative": func(v *VerificationUpdate) { v.Blocker = "WAF拦截" },
		"missing login":           func(v *VerificationUpdate) { v.MissingPrerequisites = []string{"有效登录态"} },
		"missing control":         func(v *VerificationUpdate) { v.Evidence = v.Evidence[:2] },
		"confirmed needs impact":  func(v *VerificationUpdate) { v.State = VerificationConfirmed },
		"reference required":      func(v *VerificationUpdate) { v.Evidence[0].ActivityID = 0 },
		"ambiguous reference":     func(v *VerificationUpdate) { v.Evidence[0].NodeID = 1 },
		"unknown state":           func(v *VerificationUpdate) { v.State = "safe" },
	} {
		t.Run(name, func(t *testing.T) {
			v := base
			v.Evidence = append([]VerificationEvidence(nil), base.Evidence...)
			change(&v)
			if _, _, _, err := normalizeVerificationUpdate(v); err == nil {
				t.Fatal("unproved broad conclusion accepted")
			}
		})
	}
}

func verificationTestStore(t *testing.T) (*DB, *Task, *ExplorationStore) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTask(t.Name(), "受控验证记账", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.DeleteTask(task.ID); err != nil {
			t.Error(err)
		}
	})
	return d, task, d.Exploration(task.ExplorationID)
}

func TestVerificationAppendOnlyCASAndTaskIsolation(t *testing.T) {
	d, _, store := verificationTestStore(t)
	ctx := context.Background()
	activity, err := store.AppendActivity(Activity{Kind: "tool_result", Summary: "HTTP 302", Detail: "实际请求跳转登录页"})
	if err != nil {
		t.Fatal(err)
	}
	in := verificationInput()
	in.Evidence[0].ActivityID = activity
	first, err := store.UpsertVerification(ctx, in, "worker#1")
	if err != nil || !first.Changed || first.Record.Revision != 1 {
		t.Fatalf("first write: %+v %v", first, err)
	}
	retry := in
	retry.ObservedAt = "2026-11-12T15:00:00+08:00"
	zero := int64(0)
	retry.ExpectedRevision = &zero
	again, err := store.UpsertVerification(ctx, retry, "worker#2")
	if err != nil || again.Changed || again.Record.NodeID != first.Record.NodeID {
		t.Fatalf("retry not idempotent: %+v %v", again, err)
	}
	in.Observation = "已确认缺少普通用户账号"
	in.State = VerificationMissingPrerequisite
	in.MissingPrerequisites = []string{"普通用户账号"}
	if _, err := store.UpsertVerification(ctx, in, "worker"); !errors.Is(err, ErrVerificationRevisionConflict) {
		t.Fatalf("missing CAS accepted: %v", err)
	}
	one := int64(1)
	in.ExpectedRevision = &one
	second, err := store.UpsertVerification(ctx, in, "worker")
	if err != nil || !second.Changed || second.Record.Revision != 2 || second.Record.PreviousNodeID != first.Record.NodeID {
		t.Fatalf("second write: %+v %v", second, err)
	}
	old, err := store.GetNode(first.Record.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	oldRecord, err := decodeVerification(old.ID, old.Payload, old.CreatedAt)
	if err != nil || oldRecord.Revision != 1 || oldRecord.State != VerificationInconclusive {
		t.Fatalf("old evidence overwritten: %+v %v", oldRecord, err)
	}
	page, err := store.ListVerifications(ctx, VerificationFilter{})
	if err != nil || page.Total != 1 || page.RecordedCases != 1 || len(page.Cases) != 1 || page.Cases[0].Revision != 2 {
		t.Fatalf("latest revision counted twice: %+v %v", page, err)
	}
	history, err := store.ListVerifications(ctx, VerificationFilter{ID: first.Record.ID, IncludeHistory: true, Limit: 1})
	if err != nil || history.Total != 2 || history.RecordedCases != 1 || !history.HasMore {
		t.Fatalf("history page: %+v %v", history, err)
	}
	older, err := store.ListVerifications(ctx, VerificationFilter{ID: first.Record.ID, IncludeHistory: true, Before: history.NextBefore, Limit: 1})
	if err != nil || len(older.Cases) != 1 || older.Cases[0].NodeID != first.Record.NodeID {
		t.Fatalf("history continuation: %+v %v", older, err)
	}
	beforeLatest, err := store.ListVerifications(ctx, VerificationFilter{Before: second.Record.NodeID})
	if err != nil || len(beforeLatest.Cases) != 0 {
		t.Fatalf("cursor resurrected superseded revision: %+v %v", beforeLatest, err)
	}
	otherTask, err := d.CreateTask("verification-other", "other", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(otherTask.ID) })
	other := d.Exploration(otherTask.ExplorationID)
	foreignActivity, err := other.AppendActivity(Activity{Kind: "tool_result", Summary: "foreign evidence"})
	if err != nil {
		t.Fatal(err)
	}
	foreign := in
	foreign.Evidence = append([]VerificationEvidence(nil), in.Evidence...)
	foreign.Evidence[0].ActivityID = foreignActivity
	if _, err := store.UpsertVerification(ctx, foreign, "worker"); err == nil {
		t.Fatal("foreign reference bypassed scope through unchanged digest")
	}
	foreignIntent, err := other.AddIntent(map[string]any{"summary": "foreign"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	foreign = in
	foreign.IntentID = foreignIntent
	if _, err := store.UpsertVerification(ctx, foreign, "worker"); err == nil {
		t.Fatal("foreign intent accepted")
	}
	foreignPage, err := other.ListVerifications(ctx, VerificationFilter{ID: first.Record.ID})
	if err != nil || len(foreignPage.Cases) != 0 {
		t.Fatalf("task-local read leaked: %+v %v", foreignPage, err)
	}
}

func TestVerificationConcurrentRevisionAndUntestedInventory(t *testing.T) {
	_, _, store := verificationTestStore(t)
	in := verificationInput()
	in.State = VerificationNotTested
	in.Observation = ""
	in.Evidence = nil
	first, err := store.UpsertVerification(t.Context(), in, "planner")
	if err != nil || first.Record.Progress {
		t.Fatalf("untested inventory became progress: %+v %v", first, err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 2)
	one := int64(1)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			update := in
			update.ExpectedRevision = &one
			update.State = VerificationMissingPrerequisite
			update.Observation = fmt.Sprintf("缺少受控身份%d", index)
			update.MissingPrerequisites = []string{update.Observation}
			_, err := store.UpsertVerification(t.Context(), update, "worker")
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	var success, conflict int
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrVerificationRevisionConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("lost-update gate: success=%d conflict=%d", success, conflict)
	}
}

func TestVerificationProofErrorsDismissalAndArchiveFence(t *testing.T) {
	d, task, store := verificationTestStore(t)
	failed, err := store.AppendActivity(Activity{Kind: "tool_result", IsError: true, Summary: "连接超时"})
	if err != nil {
		t.Fatal(err)
	}
	in := verificationInput()
	in.State, in.PrerequisitesMet = VerificationNegative, true
	in.Observation = "仅当前输入在有效对照下未触发"
	in.Evidence = []VerificationEvidence{
		{Role: "baseline", Observation: "已知匹配正常", ActivityID: failed},
		{Role: "test", Observation: "当前变体未触发", ActivityID: failed},
		{Role: "control", Observation: "不匹配对照正常", ActivityID: failed},
	}
	if _, err := store.UpsertVerification(t.Context(), in, "worker"); err == nil {
		t.Fatal("execution error used as a negative proof")
	}
	good, err := store.AppendActivity(Activity{Kind: "tool_result", Summary: "已保存匹配/不匹配/测试对照", Detail: "受控对象42与空集"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range in.Evidence {
		in.Evidence[i].ActivityID = good
	}
	result, err := store.UpsertVerification(t.Context(), in, "worker")
	if err != nil || result.Record.Revision != 1 {
		t.Fatalf("valid paired proof refused: %+v %v", result, err)
	}
	if err := store.SetNodeState(result.Record.NodeID, "dismissed"); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListVerifications(t.Context(), VerificationFilter{})
	if err != nil || page.RecordedCases != 0 || len(page.Cases) != 0 {
		t.Fatalf("dismissed latest case became active: %+v %v", page, err)
	}
	history, err := store.ListVerifications(t.Context(), VerificationFilter{ID: result.Record.ID, IncludeHistory: true})
	if err != nil || len(history.Cases) != 1 || history.Cases[0].NodeState != "dismissed" {
		t.Fatalf("dismissal erased historical evidence: %+v %v", history, err)
	}
	if _, err := store.UpsertVerification(t.Context(), in, "worker"); err == nil {
		t.Fatal("identical replay revived a dismissed conclusion")
	}
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	archive, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM task_archives WHERE id=$1`, archive.ID) })
	in.Target.Variant = "new-case"
	if _, err := store.UpsertVerification(t.Context(), in, "worker"); !errors.Is(err, ErrTaskExtensionFrozen) {
		t.Fatalf("verification write escaped archive fence: %v", err)
	}
}
