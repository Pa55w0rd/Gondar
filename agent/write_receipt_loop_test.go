package agent

import (
	"errors"
	"fmt"
	"testing"
)

func TestIdenticalSuccessfulWriteReceiptsCannotKeepRunAlive(t *testing.T) {
	for _, name := range []string{"record_fact", "save_evidence", "record_verification"} {
		t.Run(name, func(t *testing.T) {
			var guard toolFailureLoop
			for i := 0; i < 6; i++ {
				id := fmt.Sprint(i)
				guard.start(id, name, []byte(`{"key":"same"}`))
				err := guard.result(id, false, "same committed id 7")
				if (i == 5) != errors.Is(err, ErrToolFailureLoop) {
					t.Fatalf("receipt %d: %v", i, err)
				}
			}
		})
	}
}

func TestNewWriteReceiptsAndRepeatedReadsAreAllowed(t *testing.T) {
	for _, name := range []string{"record_fact", "save_evidence", "record_verification", "node_detail"} {
		t.Run(name, func(t *testing.T) {
			var guard toolFailureLoop
			for i := 0; i < 12; i++ {
				id := fmt.Sprint(i)
				guard.start(id, name, []byte(`{"key":"same"}`))
				out := "unchanged read"
				if name != "node_detail" {
					out = "new committed id " + id
				}
				if err := guard.result(id, false, out); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
