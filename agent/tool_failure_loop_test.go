package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"gondar/db"
)

func TestCaptureStopsSchemaFailureLoopThroughSDK(t *testing.T) {
	calls := 0
	provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		calls++
		for _, ev := range []llm.StreamEvent{
			{Type: llm.SEToolUseStart, ToolID: fmt.Sprintf("bad-%d", calls), ToolName: "Write"},
			{Type: llm.SEToolInputJSON, Text: `{}`},
			{Type: llm.SEMessageDelta, StopReason: "tool_use", Usage: llm.Usage{InputTokens: 10, OutputTokens: 2}},
		} {
			if !yield(ev, nil) {
				return
			}
		}
	}}
	s := agentcore.NewSession(agentcore.Options{Provider: provider, Tools: []tool.CoreTool{tool.NewWrite()},
		PermissionMode: permission.ModeBypass, WorkingDir: t.TempDir(), MaxTurns: 40})
	defer s.Close()
	var rows []db.Activity
	_, _, err := captureRunSession(t.Context(), s, "fixture", func(a db.Activity) { rows = append(rows, a) })
	if !errors.Is(err, ErrToolFailureLoop) || calls != maxIdenticalToolFailures {
		t.Fatalf("err=%v model calls=%d, want bounded failure at %d", err, calls, maxIdenticalToolFailures)
	}
	if t.Context().Err() != nil {
		t.Fatal("guard canceled its parent")
	}
	errorsSeen, terminals := 0, 0
	for _, a := range rows {
		if a.Kind == "tool_result" && a.IsError && strings.Contains(a.Detail, "missing required property") {
			errorsSeen++
		}
		if a.Kind == "result" {
			terminals++
			if !a.IsError || !strings.Contains(a.Detail, "tool_failure_loop") || a.InputTokens == nil {
				t.Fatalf("terminal lost typed reason or usage: %+v", a)
			}
		}
	}
	if errorsSeen != maxIdenticalToolFailures || terminals != 1 {
		t.Fatalf("errors=%d terminals=%d", errorsSeen, terminals)
	}
	// The SDK must reach its terminal event: its session state is only committed
	// there, including the failed calls needed for a later corrected continuation.
	uses, results := map[string]bool{}, map[string]bool{}
	for _, message := range s.Messages() {
		for _, block := range message.Content {
			if block.Type == llm.BlockToolUse {
				uses[block.ID] = true
			}
			if block.Type == llm.BlockToolResult {
				results[block.ToolUseID] = true
			}
		}
	}
	if len(uses) != maxIdenticalToolFailures || len(results) != len(uses) {
		t.Fatalf("unpaired history uses=%d results=%d", len(uses), len(results))
	}
}

func TestToolFailureLoopAllowsCorrectionAndSuccess(t *testing.T) {
	var g toolFailureLoop
	fail := func(id, input, output string) error {
		g.start(id, "Write", []byte(input))
		return g.result(id, true, output)
	}
	for i := 0; i < maxIdenticalToolFailures-1; i++ {
		if err := fail(fmt.Sprint(i), `{"a":1,"b":2}`, "same failure"); err != nil {
			t.Fatal(err)
		}
	}
	// A successful tool result is observable progress, even for another tool.
	g.start("ok", "Read", []byte(`{}`))
	if err := g.result("ok", false, "read content"); err != nil {
		t.Fatal(err)
	}
	if err := fail("new", `{"b":2,"a":1}`, "same failure"); err != nil {
		t.Fatal("success did not reset", err)
	}
	if err := fail("changed", `{"a":2,"b":2}`, "same failure"); err != nil {
		t.Fatal("changed input did not reset", err)
	}
	for i := 0; i < maxIdenticalToolFailures-2; i++ {
		if err := fail(fmt.Sprint("again", i), `{"b":2,"a":2}`, "same failure"); err != nil {
			t.Fatal(err)
		}
	}
	if err := fail("last", `{"a":2,"b":2}`, "same failure"); !errors.Is(err, ErrToolFailureLoop) {
		t.Fatalf("key reorder escaped guard: %v", err)
	}
}

func TestToolFailureLoopAlternatingErrors(t *testing.T) {
	for _, schema := range []bool{false, true} {
		var g toolFailureLoop
		limit := 6 // A/B/A/B/A/B without an intervening successful result
		if schema {
			limit = maxIdenticalToolFailures
		}
		for i := 0; i < limit; i++ {
			id := fmt.Sprint(i)
			g.start(id, "Write", []byte(fmt.Sprintf(`{"part":%d}`, i%2)))
			out := "same backend failure"
			if schema {
				out = "Error: invalid tool input: missing required property file_path"
			}
			err := g.result(id, true, out)
			if (i == limit-1) != errors.Is(err, ErrToolFailureLoop) {
				t.Fatalf("schema=%v at %d: %v", schema, i, err)
			}
		}
	}
}
