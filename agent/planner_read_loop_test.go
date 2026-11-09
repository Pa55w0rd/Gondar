package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

func TestPlannerStableReadLoopThroughSDK(t *testing.T) {
	for _, kind := range []string{KindPlanner, KindWorker} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			p := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
				calls++
				if calls > 8 {
					yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "worker fixture complete"}, nil)
					return
				}
				for _, ev := range []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: fmt.Sprint(calls), ToolName: "graph_overview"}, {Type: llm.SEToolInputJSON, Text: `{}`}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}} {
					if !yield(ev, nil) {
						return
					}
				}
			}}
			read := readTool("graph_overview", "fixture", obj(nil), func(context.Context, json.RawMessage) (actool.Result, error) {
				return actool.Text(`{"frontier_open":0}`), nil
			})
			_, _, err := captureRun(WithRunInfo(t.Context(), RunInfo{Kind: kind}), agentcore.Options{Provider: p, Tools: []actool.CoreTool{read}, PermissionMode: permission.ModeBypass, WorkingDir: t.TempDir(), MaxTurns: 20}, "synthetic", nil)
			if kind == KindPlanner {
				if !errors.Is(err, ErrPlannerReadLoop) || calls != 6 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
			} else if err != nil || calls != 9 {
				t.Fatalf("worker polling changed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestPlannerReadLoopAllowsChangedResultsAndWrites(t *testing.T) {
	var g plannerReadLoop
	for i := 0; i < 20; i++ {
		id := fmt.Sprint(i)
		g.start(id, "graph_overview", []byte(`{}`))
		if err := g.result(id, false, fmt.Sprint(i)); err != nil {
			t.Fatal("changing results blocked", err)
		}
	}
	for i := 0; i < 5; i++ {
		id := fmt.Sprint(i)
		g.start(id, "list_facts", []byte(`{}`))
		if err := g.result(id, false, "same"); err != nil {
			t.Fatal(err)
		}
	}
	g.start("write", "add_intent", []byte(`{}`))
	if err := g.result("write", false, "created"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		id := fmt.Sprint(i)
		g.start(id, "list_facts", []byte(`{}`))
		if err := g.result(id, false, "same"); err != nil {
			t.Fatal("write failed to reset spin", err)
		}
	}
	var cycle plannerReadLoop
	for i := 0; i < 8; i++ {
		id := fmt.Sprint(i)
		name := []string{"list_facts", "graph_overview"}[i%2]
		cycle.start(id, name, []byte(`{}`))
		err := cycle.result(id, false, "stable")
		if (i == 7) != errors.Is(err, ErrPlannerReadLoop) {
			t.Fatalf("alternation i=%d err=%v", i, err)
		}
	}
}
