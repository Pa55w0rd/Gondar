package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func executionFixture(t *testing.T) (*DB, *ExplorationStore, int64) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("execution receipt fixture", "synthetic only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTaskCascade(task.ID, true, true) })
	s := d.Exploration(task.ExplorationID)
	id, err := s.AddIntent(map[string]any{"summary": "synthetic execution", "evidence": "keep original"}, 5, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	return d, s, id
}

func TestIntentExecutionAtomicReceiptAndStaleFence(t *testing.T) {
	d, s, id := executionFixture(t)
	n, err := s.ClaimIntentExecution(t.Context(), id, "fixture", "open")
	if err != nil {
		t.Fatal(err)
	}
	run := ExecutionFromNode(n)
	if run.ID == "" || run.Phase != "claimed" {
		t.Fatalf("claim receipt=%+v", run)
	}
	if err := s.BeginIntentExecution(t.Context(), id, run.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		saved, err := s.SettleIntentExecution(t.Context(), id, run.ID, "blocked", "synthetic_reason", 123)
		if err != nil || !saved {
			t.Fatalf("settle %v %v", saved, err)
		}
	}
	n, err = s.GetNode(id)
	if err != nil || n.State != "blocked" || n.BlockedReason != "synthetic_reason" {
		t.Fatalf("node=%+v err=%v", n, err)
	}
	got := ExecutionFromNode(n)
	if got.Phase != "settled" || got.WallMS != 123 || got.State != "blocked" {
		t.Fatalf("receipt=%+v", got)
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM activity WHERE node_id=$1 AND metadata->'intent_execution'->>'id'=$2`, id, run.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("receipts=%d err=%v", count, err)
	}
	if err := s.SetIntentState(id, "open"); err != nil {
		t.Fatal(err)
	}
	newNode, err := s.ClaimIntentExecution(t.Context(), id, "new", "open")
	if err != nil {
		t.Fatal(err)
	}
	newID := ExecutionFromNode(newNode).ID
	if newID == run.ID {
		t.Fatal("execution identity reused")
	}
	if err := s.BeginIntentExecution(t.Context(), id, run.ID); !errors.Is(err, ErrIntentStateConflict) {
		t.Fatalf("old run began a newer claim: %v", err)
	}
	if saved, err := s.SettleIntentExecution(t.Context(), id, run.ID, "done", "", 555); err != nil || saved {
		t.Fatalf("stale result=%v %v", saved, err)
	}
	if err := s.BeginIntentExecution(t.Context(), id, newID); err != nil {
		t.Fatal(err)
	}
	if err := s.CompareExecutionForTest(id, newID); err != nil {
		t.Fatal(err)
	}
}

// Keep the assertion in the test package; it does not add a production seam.
func (s *ExplorationStore) CompareExecutionForTest(id int64, expected string) error {
	n, err := s.GetNode(id)
	if err != nil {
		return err
	}
	if n.State != "running" || ExecutionFromNode(n).ID != expected {
		return fmt.Errorf("new execution overwritten: %+v", n)
	}
	return nil
}

func TestIntentExecutionReceiptFailureRollsBackState(t *testing.T) {
	d, s, id := executionFixture(t)
	n, err := s.ClaimIntentExecution(t.Context(), id, "fixture", "open")
	if err != nil {
		t.Fatal(err)
	}
	run := ExecutionFromNode(n)
	if err := s.BeginIntentExecution(t.Context(), id, run.ID); err != nil {
		t.Fatal(err)
	}
	fn := fmt.Sprintf("reject_execution_receipt_%d", id)
	trigger := fmt.Sprintf("reject_execution_receipt_%d", id)
	ddl := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.node_id=%d AND NEW.metadata ? 'intent_execution' THEN RAISE EXCEPTION 'synthetic receipt failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER %s BEFORE INSERT ON activity FOR EACH ROW EXECUTE FUNCTION %s()`, fn, id, trigger, fn)
	if _, err := d.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	clean := func() {
		d.Exec("DROP TRIGGER IF EXISTS " + trigger + " ON activity")
		d.Exec("DROP FUNCTION IF EXISTS " + fn + "()")
	}
	t.Cleanup(clean)
	if saved, err := s.SettleIntentExecution(t.Context(), id, run.ID, "done", "", 300); err == nil || saved {
		t.Fatalf("failed transaction=%v %v", saved, err)
	}
	n, _ = s.GetNode(id)
	if n.State != "running" || ExecutionFromNode(n).Phase != "executing" {
		t.Fatalf("partial settlement persisted: %+v", n)
	}
	clean()
	if saved, err := s.SettleIntentExecution(t.Context(), id, run.ID, "done", "", 300); err != nil || !saved {
		t.Fatalf("recovery=%v %v", saved, err)
	}
}

func TestStartupIsolatesUnknownExecutionButRecoversUnstartedClaim(t *testing.T) {
	_, s, id := executionFixture(t)
	n, err := s.ClaimIntentExecution(t.Context(), id, "fixture", "open")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetRunningIntents(); err != nil {
		t.Fatal(err)
	}
	n, _ = s.GetNode(id)
	if n.State != "open" {
		t.Fatal("unstarted claim was not safely recovered")
	}
	n, err = s.ClaimIntentExecution(t.Context(), id, "fixture", "open")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BeginIntentExecution(t.Context(), id, ExecutionFromNode(n).ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetRunningIntents(); err != nil {
		t.Fatal(err)
	}
	n, _ = s.GetNode(id)
	if n.State != "blocked" || ExecutionFromNode(n).Phase != "unknown" {
		t.Fatalf("unknown execution replayed: %+v", n)
	}
	if _, err := s.ReopenBlockedIntents(); err != nil {
		t.Fatal(err)
	}
	n, _ = s.GetNode(id)
	if n.State != "blocked" {
		t.Fatal("task-wide resume replayed unknown execution")
	}
}

func TestAbortedDetachedClaimKeepsPausedState(t *testing.T) {
	_, s, id := executionFixture(t)
	if err := s.SetIntentState(id, "paused"); err != nil {
		t.Fatal(err)
	}
	n, err := s.ClaimIntentExecution(t.Context(), id, "chat", "paused")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AbortUnstartedClaimToState(t.Context(), id, ExecutionFromNode(n).ID, "paused"); err != nil {
		t.Fatal(err)
	}
	n, _ = s.GetNode(id)
	if n.State != "paused" || ExecutionFromNode(n).Phase != "not_executed" {
		t.Fatal("aborted chat claim resumed a paused intent")
	}
}

func TestPausedUnknownExecutionWarningSurvivesResumeClaim(t *testing.T) {
	_, s, id := executionFixture(t)
	n, err := s.ClaimIntentExecution(t.Context(), id, "worker", "open")
	if err != nil {
		t.Fatal(err)
	}
	oldID := ExecutionFromNode(n).ID
	if err := s.BeginIntentExecution(t.Context(), id, oldID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SettleIntentExecution(t.Context(), id, oldID, "paused", "execution_result_unknown: work_pause_by_user", 7); err != nil || !ok {
		t.Fatalf("pause %v %v", ok, err)
	}
	if ok, err := s.CompareAndSetIntentState(id, "paused", "open"); err != nil || !ok {
		t.Fatalf("resume %v %v", ok, err)
	}
	n, err = s.ClaimIntentExecution(t.Context(), id, "worker", "open")
	if err != nil {
		t.Fatal(err)
	}
	current := ExecutionFromNode(n)
	if current.PriorUncertainRun != oldID || current.ID == oldID {
		t.Fatal("resume lost uncertainty or reused execution")
	}
	if err := s.BeginIntentExecution(t.Context(), id, current.ID); err != nil {
		t.Fatal(err)
	}
	n, _ = s.GetNode(id)
	if ExecutionFromNode(n).PriorUncertainRun != oldID {
		t.Fatal("start overwrote resume warning")
	}
}

func TestUnstartedCleanupCannotReviveTerminalOrNewExecution(t *testing.T) {
	d, s, id := executionFixture(t)
	legacy, _ := s.GetNode(id)
	if err := s.SetIntentState(id, "running"); err != nil {
		t.Fatal(err)
	}
	if err := s.AbortUnregisteredIntent(t.Context(), legacy, "open"); err != nil {
		t.Fatal(err)
	}
	n, err := s.ClaimIntentExecution(t.Context(), id, "new", "open")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AbortUnregisteredIntent(t.Context(), legacy, "open"); err != nil {
		t.Fatal(err)
	}
	current, _ := s.GetNode(id)
	if current.State != "running" || ExecutionFromNode(current).ID != ExecutionFromNode(n).ID {
		t.Fatal("legacy rollback overwrote new claim")
	}
	if _, err := d.Exec(`UPDATE tasks SET status='done' WHERE exploration_id=$1`, s.expID); err != nil {
		t.Fatal(err)
	}
	if err := s.AbortUnstartedClaim(t.Context(), id, ExecutionFromNode(n).ID); err != nil {
		t.Fatal(err)
	}
	current, _ = s.GetNode(id)
	if current.State != "stopped" {
		t.Fatal("unstarted cleanup revived terminal task work")
	}
}

func TestUnstartedClaimCleanupCannotTouchStartedOrNewerExecution(t *testing.T) {
	_, s, id := executionFixture(t)
	n, err := s.ClaimIntentExecution(t.Context(), id, "fixture", "open")
	if err != nil {
		t.Fatal(err)
	}
	oldID := ExecutionFromNode(n).ID
	if err := s.AbortUnstartedClaim(context.Background(), id, oldID); err != nil {
		t.Fatal(err)
	}
	n, err = s.ClaimIntentExecution(t.Context(), id, "new", "open")
	if err != nil {
		t.Fatal(err)
	}
	newID := ExecutionFromNode(n).ID
	if err := s.BeginIntentExecution(t.Context(), id, newID); err != nil {
		t.Fatal(err)
	}
	if err := s.AbortUnstartedClaim(t.Context(), id, oldID); err != nil {
		t.Fatal(err)
	}
	if err := s.AbortUnstartedClaim(t.Context(), id, newID); err != nil {
		t.Fatal(err)
	}
	if err := s.CompareExecutionForTest(id, newID); err != nil {
		t.Fatal(err)
	}
}
