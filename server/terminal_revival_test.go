package server

import (
	"fmt"
	"testing"
	"time"
)

// Drive the real admission path: clearing only the database clock does not
// remove a stale engine deadline or a queued settle request from the old run.
func TestPojunTerminalRevivalStartsFreshBudget(t *testing.T) {
	for _, status := range []string{"done", "failed", "timeout"} {
		for _, queue := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/queued=%v", status, queue), func(t *testing.T) {
				m, err := NewManager(t.TempDir(), "")
				if err != nil {
					t.Fatal(err)
				}
				defer m.Close()
				defer restoreConcurrencySetting(t, m)()
				if err := m.SetConcurrency(queue, 1); err != nil {
					t.Fatal(err)
				}
				task, err := m.CreateTask("fresh terminal follow-up", "synthetic follow-up", nil, 3600, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) }()
				if _, err := m.StampTaskFirstRun(task.ID); err != nil {
					t.Fatal(err)
				}
				if err := m.SetTaskStatus(task.ID, status); err != nil {
					t.Fatal(err)
				}
				expired := time.Now().Add(-24 * time.Hour)
				if _, err := m.pg.Exec(`UPDATE tasks SET first_run_at=$2,deadline_at=$2 WHERE id=$1`, task.ID, expired); err != nil {
					t.Fatal(err)
				}
				s := newAdmissionTestServer(m, nil)
				s.engine.started.Store(task.ID, true)
				s.engine.deadline.Store(task.ID, expired.Unix())
				s.engine.stamped.Store(task.ID, true)
				s.engine.coordStarted.Store(task.ID, true)
				s.engine.settling.Store(task.ID, true)
				task.requestSettle("auto_idle", "stale previous run")
				if queue {
					holder, err := m.CreateTask("admission holder", "synthetic", nil, 0, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _, _ = m.DeleteTask(holder.ID, DeleteTaskOptions{}) }()
					if err := m.SetTaskStatus(holder.ID, "running"); err != nil {
						t.Fatal(err)
					}
					s.engine.started.Store(holder.ID, true)
				}
				queued, err := s.admitTask(task, "resume")
				if err != nil || queued != queue {
					t.Fatalf("queued=%v err=%v", queued, err)
				}
				var firstNull, deadlineNull bool
				if err := m.pg.QueryRow(`SELECT first_run_at IS NULL,deadline_at IS NULL FROM tasks WHERE id=$1`, task.ID).Scan(&firstNull, &deadlineNull); err != nil {
					t.Fatal(err)
				}
				state := task.lifecycleSnapshot()
				if !firstNull || !deadlineNull || state.FirstRunAt != 0 || state.DeadlineAt != 0 || state.Status != "running" {
					t.Fatalf("old terminal budget retained: null=%v/%v state=%+v", firstNull, deadlineNull, state)
				}
				if s.engine.isSettling(task.ID) {
					t.Fatal("retained settling gate")
				}
				for name, present := range map[string]bool{"deadline": hasEngineEntry(&s.engine.deadline, task.ID), "stamped": hasEngineEntry(&s.engine.stamped, task.ID)} {
					if present {
						t.Fatalf("retained %s gate", name)
					}
				}
				if reason, _ := task.peekSettle(); reason != "" {
					t.Fatalf("retained settle request %s", reason)
				}
				if queue {
					if hasEngineEntry(&s.engine.coordStarted, task.ID) {
						t.Fatal("queued revival retained old coordinator")
					}
					return
				}
				deadline, err := m.StampTaskFirstRun(task.ID)
				if err != nil || deadline < time.Now().Add(55*time.Minute).Unix() {
					t.Fatalf("no new full budget: deadline=%d err=%v", deadline, err)
				}
			})
		}
	}
}

type engineEntryReader interface{ Load(any) (any, bool) }

func hasEngineEntry(m engineEntryReader, key string) bool { _, ok := m.Load(key); return ok }

func TestPojunTerminalRevivalCASFailurePreservesOldGates(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	defer restoreConcurrencySetting(t, m)()
	if err := m.SetConcurrency(false, 1); err != nil {
		t.Fatal(err)
	}
	task, err := m.CreateTask("stale terminal revival", "synthetic", nil, 60, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) }()
	if err := m.SetTaskStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	s := newAdmissionTestServer(m, nil)
	s.engine.deadline.Store(task.ID, int64(123))
	task.requestSettle("auto_idle", "preserve after lost CAS")
	if _, err := m.pg.Exec(`UPDATE tasks SET status='failed' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.admitTask(task, "resume"); err == nil {
		t.Fatal("stale terminal admission won")
	}
	if !hasEngineEntry(&s.engine.deadline, task.ID) {
		t.Fatal("failed admission cleared live engine state")
	}
	if reason, _ := task.peekSettle(); reason != "auto_idle" {
		t.Fatal("failed admission cleared settle request")
	}
}
