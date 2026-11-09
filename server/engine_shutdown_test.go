package server

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"gondar/agent"
)

func TestShutdownReturnsInFlightWorkerToFrontier(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	task, err := m.CreateTask("shutdown fixture", "synthetic only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	t.Cleanup(func() { m.pg.DeleteTask(id) })
	iid, err := task.Store.AddIntent(map[string]any{"summary": "wait for synthetic provider"}, 5, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Store.SetIntentState(iid, "running"); err != nil {
		t.Fatal(err)
	}
	node, err := task.Store.GetNode(iid)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	p := retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		close(entered)
		<-ctx.Done()
		return llm.Message{}, "", llm.Usage{}, ctx.Err()
	}}
	w := agent.NewWorker(p, "synthetic", t.TempDir(), nil, 10000, 5)
	w.SetNonStreaming(func() bool { return true })
	e := NewEngine(m)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(agent.AbortShutdown)
	done := make(chan struct{})
	go func() { defer close(done); e.runIntent(ctx, task, "fixture", w, node, "", "") }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("worker never entered provider")
	}
	if task.lifecycleSnapshot().Status != "running" {
		t.Fatal("actual worker execution left task in created")
	}
	var status string
	if err := m.pg.QueryRow(`SELECT status FROM tasks WHERE id=$1`, id).Scan(&status); err != nil || status != "running" {
		t.Fatalf("persisted execution status=%s err=%v", status, err)
	}
	cancel(agent.AbortShutdown)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not finish shutdown settlement")
	}
	got, err := task.Store.GetNode(iid)
	if err != nil || got.State != "open" {
		t.Fatalf("shutdown intent=%+v err=%v", got, err)
	}
	if errors.Is(context.Cause(t.Context()), agent.AbortShutdown) {
		t.Fatal("shutdown canceled parent test context")
	}
}
