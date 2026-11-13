package selfupdate

import (
	"encoding/json"
	"os"
	"testing"
)

func operationFixture(t *testing.T, p Paths) Operation {
	t.Helper()
	if err := os.WriteFile(p.Current, []byte("fixture signed binary contents"), 0o700); err != nil {
		t.Fatal(err)
	}
	sum, err := fileSHA256(p.Current)
	if err != nil {
		t.Fatal(err)
	}
	return Operation{ID: "fixture-operation", RequestID: "fixture-request", Repository: "fixture/repo", FromVersion: "1.0.0", TargetVersion: "1.1.0", AcceptedBootID: "previous-process", BinarySHA256: sum, Phase: "staged"}
}

func TestTrackedUpdateSettlementRetainsExactIdentityAfterMarkerRemoval(t *testing.T) {
	p := testPaths(t)
	op := operationFixture(t, p)
	m := marker{From: op.FromVersion, To: op.TargetVersion, Attempts: 1, StagedAt: 123, Operation: &op}
	if err := writeMarker(p.Marker, m); err != nil {
		t.Fatal(err)
	}
	settle(p)
	if _, exists := readMarker(p.Marker); exists {
		t.Fatal("settled tracked update left its rollback marker")
	}
	got, ok := readOperation(p)
	if !ok || got.ID != op.ID || got.RequestID != op.RequestID || got.Phase != "completed" || got.BinarySHA256 != op.BinarySHA256 || got.AcceptedBootID != op.AcceptedBootID {
		t.Fatal("settlement lost or changed the accepted installation identity")
	}
}

func TestTrackedUpdateCannotSettleOldProcessOrWrongBinary(t *testing.T) {
	for _, tc := range []struct {
		name        string
		attempts    int
		wrongDigest bool
	}{
		{"before target boot", 0, false}, {"wrong binary", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			op := operationFixture(t, p)
			if tc.wrongDigest {
				op.BinarySHA256 = "another-binary"
			}
			if err := saveOperation(p, op); err != nil {
				t.Fatal(err)
			}
			if err := writeMarker(p.Marker, marker{From: op.FromVersion, To: op.TargetVersion, Attempts: tc.attempts, Operation: &op}); err != nil {
				t.Fatal(err)
			}
			settle(p)
			if _, exists := readMarker(p.Marker); !exists {
				t.Fatal("unconfirmed binary lost rollback protection")
			}
			got, _ := readOperation(p)
			if got.Phase == "completed" {
				t.Fatal("wrong/old process was marked completed")
			}
		})
	}
}

func TestUpdateMarkerAtomicReplacementKeepsRecoveryFields(t *testing.T) {
	p := testPaths(t)
	op := operationFixture(t, p)
	m := marker{From: "1.0.0", To: "1.1.0", Attempts: 2, StagedAt: 123, Operation: &op}
	for i := range 3 {
		m.Attempts = i
		if err := writeMarker(p.Marker, m); err != nil {
			t.Fatal(err)
		}
		got, ok := readMarker(p.Marker)
		if !ok || got.Attempts != i || got.From != m.From || got.To != m.To || got.Operation == nil || got.Operation.ID != op.ID {
			t.Fatal("replacement lost recovery/operation fields")
		}
	}
	if err := writeUpdateJSON(p.Marker, make(chan int)); err == nil {
		t.Fatal("invalid metadata was accepted")
	}
	got, ok := readMarker(p.Marker)
	if !ok || got.Attempts != 2 || got.Operation == nil {
		t.Fatal("failed write destroyed last complete recovery marker")
	}
	legacy := []byte(`{"from":"1.0.0","to":"1.1.0","attempts":1,"staged_at":123}`)
	if err := os.WriteFile(p.Marker, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok = readMarker(p.Marker); !ok || got.Operation != nil {
		t.Fatal("legacy marker compatibility changed")
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatal(err)
	}
}

func TestTrackedUpdateRollbackReceiptDoesNotBecomeSuccess(t *testing.T) {
	p := testPaths(t)
	op := operationFixture(t, p)
	if err := os.WriteFile(p.Old, []byte("previous binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, state := confirmOrRollback(p, marker{From: op.FromVersion, To: op.TargetVersion, Attempts: maxAttempts, Operation: &op})
	got, ok := readOperation(p)
	if !state.RolledBack || !ok || got.Phase != "rolled_back" || got.ID != op.ID {
		t.Fatal("automatic rollback lost its tracked failure result")
	}
}
