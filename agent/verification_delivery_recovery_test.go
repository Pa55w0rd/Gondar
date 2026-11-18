package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"strings"
	"testing"

	"gondar/db"
)

func deliveryInput(method, variant string) []byte {
	b, _ := json.Marshal(db.VerificationUpdate{Target: db.VerificationTarget{Endpoint: "https://owned.invalid/items", Method: method, Input: "none", Identity: "anonymous", Technique: "fixture", Variant: variant}, State: db.VerificationInconclusive, Summary: "local observation", Observation: "existing material needs review"})
	return b
}

func TestPendingDeliveryPreservesLaterModelFailureAndSummary(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask(t.Name(), "local fixture only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	store := d.Exploration(task.ExplorationID)
	id, err := store.AddIntent(map[string]any{"summary": "only local existing records"}, 1, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := store.GetNode(id)
	failure := errors.New("backend_unavailable_503_fixture")
	calls := 0
	p := plannerStateProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls == 1 {
			var input map[string]any
			_ = json.Unmarshal(deliveryInput("GET", ""), &input)
			input["state"] = "negative"
			input["prerequisites_met"] = true
			raw, _ := json.Marshal(input)
			return idleToolCall(calls, "record_verification", string(raw))
		}
		return llm.Message{}, "", llm.Usage{}, failure
	}}
	w := NewWorker(p, "fixture", t.TempDir(), nil, 200000, 8)
	w.SetNonStreaming(func() bool { return true })
	var terminal db.Activity
	reason, _, runErr := w.Execute(t.Context(), "fixture", task.ID, nil, store, intent, nil, func(row db.Activity) {
		if row.Kind == "result" {
			terminal = row
		}
	}, nil, nil)
	if reason != harness.ReasonModelError || !errors.Is(runErr, failure) || errors.Is(runErr, ErrVerificationDelivery) || !strings.Contains(terminal.Summary, "model_error") || !strings.Contains(terminal.Detail, failure.Error()) || !strings.Contains(terminal.Detail, "unresolved_verification_writes") {
		t.Fatalf("保存缺口覆盖实际模型故障：reason=%s err=%v summary=%s", reason, runErr, terminal.Summary)
	}
}

func deliverySaved(input []byte, revision int64) []byte {
	b, _ := json.Marshal(verificationReceipt{Saved: true, ID: verificationDeliveryKey(input), State: db.VerificationInconclusive, NodeID: 123, Revision: revision})
	return b
}

func TestVerificationDeliveryNormalizedSuccessClearsOnlyItsCase(t *testing.T) {
	g := &verificationDeliveryGuard{}
	failed := deliveryInput(" get ", " ")
	other := deliveryInput("POST", "")
	g.PostToolUse(t.Context(), "record_verification", failed, []byte(`"missing evidence"`), true)
	g.PostToolUse(t.Context(), "record_verification", other, []byte(`"missing baseline"`), true)
	fixed := deliveryInput("GET", "default")
	g.PostToolUse(t.Context(), "record_verification", fixed, deliverySaved(fixed, 1), false)
	if g.pendingCount() != 1 {
		t.Fatalf("normalized case not settled independently: %s", g.receiptSummary())
	}
	if !strings.Contains(g.receiptSummary(), "POST") || !strings.Contains(g.receiptSummary(), "saved_verifications") {
		t.Fatal("pending details or partial success lost")
	}
}

func TestVerificationDeliveryUnidentifiedFailureIsNotErasedByAnotherSave(t *testing.T) {
	g := &verificationDeliveryGuard{}
	g.PostToolUse(t.Context(), "record_verification", []byte(`{"state":"negative"}`), []byte(`"missing target"`), true)
	valid := deliveryInput("GET", "")
	g.PostToolUse(t.Context(), "record_verification", valid, deliverySaved(valid, 1), false)
	if g.pendingCount() != 1 {
		t.Fatal("unrelated receipt hid unidentified failure")
	}
}

func TestVerificationDeliveryExplicitRepairPreservesKnownTargetFields(t *testing.T) {
	for _, sameTarget := range []bool{false, true} {
		g := &verificationDeliveryGuard{}
		old := []byte(`{"target":{"endpoint":"https://owned.invalid/items","method":"GET","identity":"anonymous","input":"none"},"state":"inconclusive","summary":"pending"}`)
		g.PostToolUse(t.Context(), "record_verification", old, []byte(`"missing technique"`), true)
		var next map[string]any
		_ = json.Unmarshal(deliveryInput("GET", ""), &next)
		if !sameTarget {
			next["target"].(map[string]any)["endpoint"] = "https://owned.invalid/another"
		}
		next["repair_of"] = verificationDeliveryKey(old)
		input, _ := json.Marshal(next)
		g.PostToolUse(t.Context(), "record_verification", input, deliverySaved(input, 1), false)
		if (g.pendingCount() == 0) != sameTarget {
			t.Fatalf("invalid repair correlation: same=%v summary=%s", sameTarget, g.receiptSummary())
		}
	}
}

func TestVerificationDeliveryUsesConfiguredCorrectionsAndStopsUnchangedErrors(t *testing.T) {
	g := &verificationDeliveryGuard{maxCorrections: 3}
	first, second := deliveryInput("GET", ""), deliveryInput("POST", "")
	for _, in := range [][]byte{first, second} {
		g.PostToolUse(t.Context(), "record_verification", in, []byte(`"incomplete"`), true)
	}
	if stop, notes, _ := g.Stop(t.Context(), nil); stop || len(notes) == 0 {
		t.Fatal("first repair opportunity lost")
	}
	g.PostToolUse(t.Context(), "record_verification", first, deliverySaved(first, 1), false)
	if stop, notes, _ := g.Stop(t.Context(), nil); stop || len(notes) == 0 {
		t.Fatal("partial saving must allow a further correction within configuration")
	}
	if stop, _, _ := g.Stop(t.Context(), nil); !stop {
		t.Fatal("unchanged failure must not spin through more model calls")
	}
	g.PostToolUse(t.Context(), "record_verification", second, deliverySaved(second, 1), false)
	if stop, notes, _ := g.Stop(t.Context(), nil); stop || len(notes) > 0 {
		t.Fatal("resolved handoff still blocked stop")
	}
	if block, _, _ := g.PreToolUse(t.Context(), "Bash", nil); block {
		t.Fatal("successful delivery left stale correction mode")
	}
}

func TestVerificationDeliveryReconcilesLostAcknowledgementOnlyWithMatchingContent(t *testing.T) {
	in := deliveryInput("GET", "")
	var update db.VerificationUpdate
	_ = json.Unmarshal(in, &update)
	id, digest, err := db.VerificationUpdateIdentity(update)
	if err != nil {
		t.Fatal(err)
	}
	for _, matches := range []bool{false, true} {
		gotDigest := digest
		if !matches {
			gotDigest = "older-content"
		}
		g := &verificationDeliveryGuard{lookup: func(context.Context, string) (*db.VerificationRecord, error) {
			observation := update.Observation
			if !matches {
				observation = "an earlier observation"
			}
			target, _, _ := db.NormalizeVerificationTarget(update.Target)
			return &db.VerificationRecord{ID: id, ContentDigest: gotDigest, NodeID: 125, Revision: 2, State: update.State, Target: target, Summary: update.Summary, Observation: observation}, nil
		}}
		g.PostToolUse(t.Context(), "record_verification", in, []byte(`"acknowledgement lost"`), true)
		g.reconcile(t.Context())
		if (g.pendingCount() == 0) != matches {
			t.Fatalf("wrong receipt reconciliation: matches=%v %s", matches, g.receiptSummary())
		}
	}
}

func TestVerificationDeliveryBusinessEquivalentRecordCannotAcknowledgeDifferentEvidence(t *testing.T) {
	var update db.VerificationUpdate
	_ = json.Unmarshal(deliveryInput("GET", ""), &update)
	update.Evidence = []db.VerificationEvidence{{Role: "test", NodeID: 9999, Observation: "same observation"}}
	id, digest, err := db.VerificationUpdateIdentity(update)
	if err != nil {
		t.Fatal(err)
	}
	target, _, _ := db.NormalizeVerificationTarget(update.Target)
	record := &db.VerificationRecord{ID: id, ContentDigest: digest, NodeID: 125, Revision: 1, Target: target, State: update.State, Summary: update.Summary, Observation: update.Observation, Evidence: []db.VerificationEvidence{{Role: "test", NodeID: 1, Observation: "same observation"}}}
	g := &verificationDeliveryGuard{lookup: func(context.Context, string) (*db.VerificationRecord, error) { return record, nil }}
	raw, _ := json.Marshal(update)
	g.PostToolUse(t.Context(), "record_verification", raw, []byte(`"invalid evidence reference"`), true)
	g.reconcile(t.Context())
	if g.pendingCount() != 1 {
		t.Fatal("business progress digest concealed unsaved evidence provenance")
	}
}

func TestVerificationDeliveryPendingSurvivesMultipleHandoffsUntilConfirmed(t *testing.T) {
	in := deliveryInput("GET", "")
	first := &verificationDeliveryGuard{}
	first.PostToolUse(t.Context(), "record_verification", in, []byte(`"not yet saved"`), true)
	second := &verificationDeliveryGuard{}
	second.restore(first.snapshot().Persisted)
	third := &verificationDeliveryGuard{}
	third.restore(second.snapshot().Persisted) // no write in the middle round
	if third.pendingCount() != 1 || !third.correcting {
		t.Fatal("unresolved handoff disappeared without confirmed save")
	}
	third.PostToolUse(t.Context(), "record_verification", in, deliverySaved(in, 1), false)
	fourth := &verificationDeliveryGuard{}
	fourth.restore(third.snapshot().Persisted)
	if fourth.pendingCount() != 0 || fourth.correcting {
		t.Fatal("confirmed repair did not clear durable handoff")
	}
}

func TestVerificationDeliveryLostRepairAcknowledgementAlsoResolvesInvalidAttempt(t *testing.T) {
	old := []byte(`{"state":"inconclusive","summary":"missing target"}`)
	var input map[string]any
	_ = json.Unmarshal(deliveryInput("GET", ""), &input)
	input["repair_of"] = verificationDeliveryKey(old)
	raw, _ := json.Marshal(input)
	var update db.VerificationUpdate
	_ = json.Unmarshal(raw, &update)
	target, id, _ := db.NormalizeVerificationTarget(update.Target)
	g := &verificationDeliveryGuard{lookup: func(context.Context, string) (*db.VerificationRecord, error) {
		return &db.VerificationRecord{ID: id, NodeID: 127, Revision: 1, Target: target, State: update.State, Summary: update.Summary, Observation: update.Observation}, nil
	}}
	g.PostToolUse(t.Context(), "record_verification", old, []byte(`"target required"`), true)
	g.PostToolUse(t.Context(), "record_verification", raw, []byte(`"ack lost"`), true)
	g.reconcile(t.Context())
	if g.pendingCount() != 0 {
		t.Fatalf("ACK reconciliation left stale invalid attempt: %s", g.receiptSummary())
	}
}

func TestWorkerDeliveryHandoffIsTaskLocalAndNotLostBehindLaterRows(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask(t.Name(), "local only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	store := d.Exploration(task.ExplorationID)
	id, err := store.AddIntent(map[string]any{"summary": "local handoff"}, 1, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	g := &verificationDeliveryGuard{}
	g.PostToolUse(t.Context(), "record_verification", deliveryInput("GET", ""), []byte(`"pending local record"`), true)
	metadata, _ := json.Marshal(map[string]any{"verification_delivery": g.snapshot().Persisted})
	if _, err := store.AppendActivity(db.Activity{Kind: "result", NodeID: &id, Metadata: metadata}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		_, _ = store.AppendActivity(db.Activity{Kind: "info", NodeID: &id, Summary: "later local row"})
	}
	text := verificationResumeHandoff(store, id)
	if !strings.Contains(text, "pending local record") || strings.Contains(text, "pending_inputs") || strings.Contains(text, "get_worker_output") {
		t.Fatal("resume handoff missing or relies on unavailable tool")
	}
	other, err := d.CreateTask(t.Name()+" other", "local only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(other.ID)
	if text := verificationResumeHandoff(d.Exploration(other.ExplorationID), id); text != "" {
		t.Fatal("cross-task handoff leaked")
	}
}
