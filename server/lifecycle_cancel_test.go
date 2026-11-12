package server

import (
	"context"
	"testing"
	"time"
)

func TestServerCloseCancelsOwnedBackgroundContext(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	parent := context.Background()
	s := New(parent, m, "", t.TempDir(), t.TempDir())
	s.Close()
	s.Close()
	select {
	case <-s.ctx.Done():
	default:
		t.Fatal("server background lifetime survived Close")
	}
	if parent.Err() != nil {
		t.Fatal("server close canceled caller lifetime")
	}
}

func TestManagerCloseCancelsServerBeforeFurtherBackgroundRetries(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(context.Background(), m, "", t.TempDir(), t.TempDir())
	defer s.Close()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("closed database owner left live server jobs")
	}
}
