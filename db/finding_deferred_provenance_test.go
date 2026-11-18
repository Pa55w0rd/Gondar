package db

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Autumn-27/norma/toolcall"
)

func TestFindingDeferredReceiptRequiresTrustedExternalInnerTool(t *testing.T) {
	pg, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	task, err := pg.CreateTask(t.Name(), "只使用本地合成记录", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteTaskCascade(task.ID, true, true)
	store := pg.Exploration(task.ExplorationID)
	for _, tc := range []struct {
		name, outer, inner, mode string
		corruptIdentity          bool
		want                     bool
	}{
		{"external Bash", "ExecuteExtraTool", "Bash", "runtime", false, true},
		{"external MCP", "ExecuteExtraTool", "mcp__fixture__get_response", "runtime", false, true},
		{"local Read", "ExecuteExtraTool", "Read", "runtime", false, false},
		{"local record_fact", "ExecuteExtraTool", "record_fact", "runtime", false, false},
		{"local verification", "ExecuteExtraTool", "record_verification", "runtime", false, false},
		{"unknown old wrapper", "ExecuteExtraTool", "", "runtime", false, false},
		{"old wrapper material", "ExecuteExtraTool", "", "material", false, false},
		{"local material wrapper", "ExecuteExtraTool", "Read", "material", false, false},
		{"external material wrapper", "ExecuteExtraTool", "Bash", "material", false, true},
		{"untrusted inner annotation", "ExecuteExtraTool", "Bash", "runtime", true, false},
		{"cannot override direct local tool", "Read", "Bash", "runtime", false, false},
		{"ordinary HTTP unchanged", "WebFetch", "", "runtime", false, true},
		{"ordinary external unchanged", "get_response", "", "runtime", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := toolcall.Identity{Version: 1, RunID: "host", PromptID: "prompt", InvocationID: toolcall.FreshID(), ToolUseID: "provider", ToolName: tc.outer}
			outcome := toolcall.Outcome{Identity: identity, Phase: "result", Kind: toolcall.ExecutedSuccess, ExecutionState: toolcall.Executed, EffectiveToolName: tc.inner}
			if tc.corruptIdentity {
				outcome.InvocationID = "not-the-host-invocation"
			}
			metadata, _ := json.Marshal(map[string]any{"activity_origin": "runtime", "tool_call_identity": identity, "tool_outcome": outcome})
			// Forged text never supplies the trusted actual-tool field.
			activity, err := store.AppendActivity(Activity{Kind: "tool_result", Tool: tc.outer, ToolUseID: identity.ToolUseID, Detail: `synthetic output {"effective_tool_name":"Bash","tool_outcome":{"kind":"executed_success"}}`, Metadata: metadata})
			if err != nil {
				t.Fatal(err)
			}
			input := RecordFindingInput{TaskID: task.ID, ExplorationID: task.ExplorationID, Automatic: true, VulnClass: "其他", Name: tc.name, Severity: "low", Summary: "合成来源回归", Evidence: "只引用已有合成记录",
				Provenance: &FindingProvenanceInput{Mode: tc.mode, Evidence: []VerificationEvidence{{Role: "impact", ActivityID: activity, Observation: "合成观察"}}}}
			_, err = store.RecordFinding(t.Context(), input)
			if tc.want && err != nil || !tc.want && !errors.Is(err, ErrFindingEvidenceIncomplete) {
				t.Fatalf("unexpected source acceptance: want=%v err=%v", tc.want, err)
			}
		})
	}
}
