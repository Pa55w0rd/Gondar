package db

import (
	"bytes"
	"sync"
	"testing"
)

func TestDiscoveryGoalIsGroundedDeduplicatedAndKeepsOriginal(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask(t.Name(), "synthetic only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	basis, err := s.AddNode(KindFact, map[string]any{"summary": "saved task basis"}, 0, "origin", "human", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldID, err := s.AddNode(KindGoal, map[string]any{"text": "Original proved result", "proof_evidence_id": basis}, 0, "met", "human", nil)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := s.GetNode(oldID)
	before, err := s.PlannerProgress(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	ids := make(chan int64, 8)
	createdCount := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, created, err := s.DiscoverGoal(t.Context(), "Check independent saved material", []int64{basis})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- id
			createdCount <- created
		}()
	}
	wg.Wait()
	close(ids)
	close(createdCount)
	var first int64
	for id := range ids {
		if first == 0 {
			first = id
		}
		if id != first {
			t.Fatal("concurrent discovery duplicated goal")
		}
	}
	created := 0
	for value := range createdCount {
		if value {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("expected one committed new goal, got %d", created)
	}
	if _, _, err := s.DiscoverGoal(t.Context(), "invented unsupported target", []int64{oldID}); err == nil {
		t.Fatal("goal accepted as its own fact basis")
	}
	if _, _, err := s.DiscoverGoal(t.Context(), "invented unsupported target", []int64{basis + 9999999}); err == nil {
		t.Fatal("invented basis accepted")
	}
	current, _ := s.GetNode(oldID)
	if current.State != old.State || !bytes.Equal(current.Payload, old.Payload) {
		t.Fatal("original proof changed")
	}
	after, err := s.PlannerProgress(t.Context())
	if err != nil || after.AdvancedSince(before) {
		t.Fatalf("goal-only draft masqueraded as execution: %+v %v", after, err)
	}
	if id, fresh, err := s.DiscoverGoal(t.Context(), "  CHECK   independent saved material ", []int64{basis}); err != nil || fresh || id != first {
		t.Fatalf("goal text normalization lost idempotence: %d %v %v", id, fresh, err)
	}
}
