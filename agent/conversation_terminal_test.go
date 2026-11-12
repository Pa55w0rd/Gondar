package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/harness"
)

func TestConversationIncompleteTerminalCannotBecomeSuccess(t *testing.T) {
	for _, reason := range []harness.TerminalReason{harness.ReasonStopHookPrevented, harness.ReasonMaxTurns, harness.ReasonTimeout, harness.ReasonModelError, harness.ReasonHookStopped, ""} {
		err := completedRunError(reason, nil)
		if err == nil || !strings.Contains(err.Error(), "成果保留") {
			t.Fatalf("terminal lost: %q %v", reason, err)
		}
	}
	sentinel := errors.New("synthetic transport error")
	if !errors.Is(completedRunError(harness.ReasonModelError, sentinel), sentinel) {
		t.Fatal("original error replaced")
	}
	if err := completedRunError(harness.ReasonCompleted, nil); err != nil {
		t.Fatal(err)
	}
}
