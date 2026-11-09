package db

import (
	"fmt"
	"sync"
	"testing"
)

func TestContinuationChecksLiveStateAndSingleActiveBranch(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("continuation transaction", "synthetic only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTask(task.ID) })
	s := d.Exploration(task.ExplorationID)
	parent, err := s.AddIntent(map[string]any{"summary": "source"}, 5, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	add := func() (int64, error) {
		return s.AddIntent(map[string]any{"summary": "next", "continues": []int64{parent}, "continuation_depth": 1}, 5, nil, "test")
	}
	for _, state := range []string{"open", "running", "paused", "deleted"} {
		if err := s.SetIntentState(parent, state); err != nil {
			t.Fatal(err)
		}
		if id, err := add(); err == nil || id != 0 {
			t.Fatalf("accepted active/deleted source %s: id=%d err=%v", state, id, err)
		}
	}
	if err := s.SetIntentState(parent, "exhausted"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	ids := make(chan int64, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, err := add()
			results <- err
			if err == nil {
				ids <- id
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(ids)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("concurrent continuations admitted=%d, want 1", success)
	}
	child := <-ids
	var depth int
	if err := d.QueryRow(`SELECT (payload->>'continuation_depth')::int FROM exploration_nodes WHERE id=$1`, child).Scan(&depth); err != nil {
		t.Fatal(err)
	}
	if depth != 2 {
		t.Fatalf("trusted spoofed depth: %d", depth)
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent'`, s.ID()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("failed validation left orphan nodes: %d", count)
	}
	// A source becoming running while an older client is preparing input must be
	// re-checked under the same row lock used by the insertion transaction.
	if err := s.SetIntentState(child, "done"); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE exploration_nodes SET state='running' WHERE id=$1`, parent); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	checked := make(chan error, 1)
	go func() { _, err := add(); checked <- err }()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-checked; err == nil {
		t.Fatal("accepted source after its state changed to running")
	}
}

func TestContinuationPreservesDirectSourceBoundary(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	source, err := d.CreateTask("continuation source", "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTask(source.ID) })
	ss := d.Exploration(source.ExplorationID)
	id, err := ss.AddIntent(map[string]any{"summary": "source"}, 5, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := ss.SetIntentState(id, "blocked"); err != nil {
		t.Fatal(err)
	}
	for _, linked := range []bool{false, true} {
		opts := TaskCreateOptions{Name: fmt.Sprintf("continuation-related-%v", linked)}
		if linked {
			opts.SourceTaskIDs = []int64{source.ID}
		}
		task, err := d.CreateTaskWithOptions("source boundary", "synthetic", opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.DeleteTask(task.ID) })
		_, err = d.Exploration(task.ExplorationID).AddIntent(map[string]any{"summary": "next", "continues": []int64{id}}, 5, nil, "test")
		if linked && err != nil {
			t.Fatalf("direct terminal source rejected: %v", err)
		}
		if !linked && err == nil {
			t.Fatal("unrelated source accepted")
		}
	}
}
