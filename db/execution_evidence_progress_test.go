package db

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func TestPlannerProgressTracksEvidenceContentNotRetries(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("evidence progress", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	before, err := s.PlannerProgress(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"summary": "observed negative for one input", "confidence": "observed", "detail": "synthetic control", "evidence": "synthetic probe returned the controlled negative"}
	id, created, err := s.RecordFact(payload, 5, "worker", nil, 0)
	if err != nil || !created {
		t.Fatalf("first: %v %v", created, err)
	}
	after, err := s.PlannerProgress(t.Context())
	if err != nil || !after.EvidenceAdvancedSince(before) {
		t.Fatalf("new evidence not recognized: %+v %v", after, err)
	}
	id2, created, err := s.RecordFact(payload, 5, "worker", nil, 0)
	if err != nil || created || id2 != id {
		t.Fatalf("retry changed evidence: %d %d %v %v", id, id2, created, err)
	}
	retried, err := s.PlannerProgress(t.Context())
	if err != nil || retried.EvidenceAdvancedSince(after) {
		t.Fatalf("retry counted: %+v %v", retried, err)
	}
	clock := map[string]any{"summary": payload["summary"], "confidence": "observed", "detail": "synthetic control", "evidence": payload["evidence"], "observed_at": "new clock"}
	if _, err := s.AddNode(KindFact, clock, 5, "confirmed", "worker", nil); err != nil {
		t.Fatal(err)
	}
	clocked, err := s.PlannerProgress(t.Context())
	if err != nil || clocked.EvidenceAdvancedSince(retried) {
		t.Fatalf("clock counted: %+v %v", clocked, err)
	}
	if _, err := s.AddNode(KindFact, map[string]any{"summary": "new draft", "confidence": "inferred", "detail": "unexecuted hypothesis"}, 5, "confirmed", "planner", nil); err != nil {
		t.Fatal(err)
	}
	draft, err := s.PlannerProgress(t.Context())
	if err != nil || draft.EvidenceAdvancedSince(clocked) {
		t.Fatalf("draft counted: %+v %v", draft, err)
	}
	if _, err := s.AddNode(KindFact, map[string]any{"verification": map[string]any{"id": "case1", "revision": 1, "progress": false, "state": "not_tested"}}, 5, "confirmed", "worker", nil); err != nil {
		t.Fatal(err)
	}
	planned, err := s.PlannerProgress(t.Context())
	if err != nil || planned.EvidenceAdvancedSince(clocked) {
		t.Fatalf("planned counted: %+v %v", planned, err)
	}
	if _, err := s.AddNode(KindFact, map[string]any{"verification": map[string]any{"id": "case1", "revision": 2, "progress": true, "state": "negative"}}, 5, "confirmed", "worker", nil); err != nil {
		t.Fatal(err)
	}
	verified, err := s.PlannerProgress(t.Context())
	if err != nil || !verified.EvidenceAdvancedSince(planned) {
		t.Fatalf("verification lost: %+v %v", verified, err)
	}
}

func TestRecordFactConcurrentRetriesKeepOneFactAndYield(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("atomic fact", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	intent, err := s.AddNode(KindIntent, map[string]any{"summary": "synthetic"}, 5, "running", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	receipts := make(chan bool, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := s.RecordFact(map[string]any{"summary": "same evidence"}, 5, "worker", nil, intent)
			receipts <- created
			errs <- err
		}()
	}
	wg.Wait()
	close(receipts)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for created := range receipts {
		if created {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("created %d facts", n)
	}
	var edges int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_edges WHERE exploration_id=$1 AND src_id=$2 AND rel='yields'`, task.ExplorationID, intent).Scan(&edges); err != nil || edges != 1 {
		t.Fatalf("yield not atomic: %d %v", edges, err)
	}
}

func TestArtifactAtomicContentReceiptAndPlannerProgress(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.EnsureTaskArtifactsTable(); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask("artifact receipt", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	before, err := s.PlannerProgress(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	receipts := make(chan bool, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := &TaskArtifact{TaskID: strconv.FormatInt(task.ID, 10), Name: "proof.txt", Kind: "log", SHA256: "synthetic-sha256", SizeBytes: 4, Storage: fmt.Sprintf("proof-%d.txt", i)}
			created, err := d.InsertTaskArtifactWithProgress(t.Context(), a)
			if err == nil && a.NewContent != created {
				err = fmt.Errorf("receipt mismatch")
			}
			receipts <- created
			errs <- err
		}(i)
	}
	wg.Wait()
	close(receipts)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for created := range receipts {
		if created {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("created %d duplicate artifacts", n)
	}
	after, err := s.PlannerProgress(t.Context())
	if err != nil || !after.EvidenceAdvancedSince(before) {
		t.Fatalf("attachment not progress: %+v %v", after, err)
	}
	a := &TaskArtifact{TaskID: strconv.FormatInt(task.ID, 10), Name: "again.txt", Kind: "log", SHA256: "synthetic-sha256", SizeBytes: 4, Storage: "retry.txt"}
	created, err := d.InsertTaskArtifactWithProgress(t.Context(), a)
	if err != nil || created || a.NewContent {
		t.Fatalf("repeat: %v %v", created, err)
	}
	retried, err := s.PlannerProgress(t.Context())
	if err != nil || retried.EvidenceAdvancedSince(after) {
		t.Fatalf("attachment retry counted: %+v %v", retried, err)
	}
}
