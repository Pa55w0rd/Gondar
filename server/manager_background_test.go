package server

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestManagerCloseCancelsBackgroundBeforeClosingStore(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(m.managerBackgroundContext().Err(), context.Canceled) {
		t.Fatal("manager close left background lifetime active")
	}
	if m.SyncVulnIntelNow() {
		t.Fatal("closed manager accepted a new synchronization")
	}
}

func TestManagerBackgroundLoopsExitOnCloseContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	m := &Manager{backgroundCtx: ctx}
	finished := make(chan struct{}, 2)
	go func() { m.runVulnIntelSync(ctx); finished <- struct{}{} }()
	go func() { m.runVulnIntelFastLane(ctx); finished <- struct{}{} }()
	cancel()
	for range 2 {
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("closed manager retained delayed synchronization")
		}
	}
	if waitManagerBackground(ctx, time.Hour) {
		t.Fatal("canceled delay resumed")
	}
}
