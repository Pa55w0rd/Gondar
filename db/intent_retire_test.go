package db

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRetireOpenIntentsPreservesGraphAndRejectsRunning(t *testing.T) {
	_, s, id := executionFixture(t)
	proof, err := s.AddNode(KindFact, map[string]any{"summary": "precondition invalidated", "evidence": "synthetic receipt"}, 0, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Link(proof, RelDerivedFrom, id); err != nil {
		t.Fatal(err)
	}
	items := []IntentRetirement{{IntentID: id, EvidenceID: proof, Reason: "已保存新事实否定此前提"}}
	ids, err := s.RetireOpenIntents(t.Context(), items)
	if err != nil || len(ids) != 1 {
		t.Fatalf("retire=%v %v", ids, err)
	}
	n, err := s.GetNode(id)
	if err != nil || n.State != "deleted" || !strings.Contains(string(n.Payload), "keep original") || !strings.Contains(string(n.Payload), "retirement") {
		t.Fatalf("evidence lost: %+v %v", n, err)
	}
	if n, _ := s.GetNode(proof); n == nil {
		t.Fatal("proof deleted")
	}
	edges, _ := s.Edges(100)
	if len(edges) == 0 {
		t.Fatal("lineage removed")
	}
	if _, err := s.RetireOpenIntents(t.Context(), items); err == nil {
		t.Fatal("retirement counted twice")
	}
	active, _ := s.AddIntent(map[string]any{"summary": "running fixture"}, 5, nil, "test")
	if _, err := s.ClaimIntentExecution(t.Context(), active, "worker", "open"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetireOpenIntents(t.Context(), []IntentRetirement{{IntentID: active, EvidenceID: proof, Reason: "should not kill"}}); err == nil {
		t.Fatal("retired a running worker")
	}
}

func TestRetireOpenIntentsBatchIsAtomicAndProofIsLocal(t *testing.T) {
	_, s, id := executionFixture(t)
	_, foreign, foreignID := executionFixture(t)
	proof, err := foreign.AddNode(KindFact, map[string]any{"summary": "foreign"}, 0, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetireOpenIntents(t.Context(), []IntentRetirement{{IntentID: id, EvidenceID: proof, Reason: "foreign"}}); err == nil {
		t.Fatal("cross task proof accepted")
	}
	replacement, _ := s.AddIntent(map[string]any{"summary": "replacement"}, 6, nil, "test")
	if _, err := s.RetireOpenIntents(t.Context(), []IntentRetirement{{IntentID: id, SupersededBy: replacement, Reason: "replacement"}, {IntentID: foreignID, SupersededBy: replacement, Reason: "invalid foreign"}}); err == nil {
		t.Fatal("cross task batch accepted")
	}
	n, _ := s.GetNode(id)
	if n.State != "open" {
		t.Fatal("failed batch changed first intent")
	}
	if _, err := s.RetireOpenIntents(t.Context(), []IntentRetirement{{IntentID: id, SupersededBy: replacement, Reason: "replacement"}, {IntentID: replacement, SupersededBy: id, Reason: "cycle"}}); err == nil {
		t.Fatal("retired its own replacement")
	}
}

func TestClaimAndRetireCompeteWithoutDoubleOwnership(t *testing.T) {
	_, s, _ := executionFixture(t)
	proof, err := s.AddNode(KindFact, map[string]any{"summary": "invalidation"}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		id, _ := s.AddIntent(map[string]any{"summary": "race fixture"}, 5, nil, "test")
		start := make(chan struct{})
		claim, retire := make(chan error, 1), make(chan error, 1)
		go func() { <-start; _, err := s.ClaimIntentExecution(t.Context(), id, "worker", "open"); claim <- err }()
		go func() {
			<-start
			_, err := s.RetireOpenIntents(t.Context(), []IntentRetirement{{IntentID: id, EvidenceID: proof, Reason: "invalidated"}})
			retire <- err
		}()
		close(start)
		c, r := <-claim, <-retire
		if (c == nil) == (r == nil) {
			t.Fatalf("claim=%v retire=%v", c, r)
		}
		n, _ := s.GetNode(id)
		var payload map[string]any
		_ = json.Unmarshal(n.Payload, &payload)
		if c == nil && (n.State != "running" || payload["retirement"] != nil) {
			t.Fatal("retired claimed worker")
		}
		if r == nil && (n.State != "deleted" || ExecutionFromNode(n).ID != "") {
			t.Fatal("claimed retired intent")
		}
	}
}
