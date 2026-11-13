package server

import (
	"context"
	"testing"

	"github.com/Autumn-27/norma/toolcall"
	"gondar/agent"
)

func TestFindingReportSourceOnlyHostConversationInvocation(t *testing.T) {
	identity := toolcall.Identity{Version: 1, RunID: "host-run", PromptID: "host-prompt", InvocationID: "host-invocation", ToolUseID: "provider-id", ToolName: "update_finding_report"}
	ctx := toolcall.WithInvocation(agent.WithRunInfo(context.Background(), agent.RunInfo{Kind: agent.KindChat, SessionID: "conv-10"}), toolcall.New(identity))
	source := findingReportSourceFromContext(ctx)
	if source == nil || source.ConversationID != 10 || source.InvocationID != identity.InvocationID || source.RunID != identity.RunID {
		t.Fatal("actual host invocation was not captured")
	}
	for _, info := range []agent.RunInfo{{Kind: agent.KindChat, SessionID: "10"}, {Kind: agent.KindChat, SessionID: "conv-01"}, {Kind: agent.KindMain, SessionID: "conv-10"}, {Kind: agent.KindChat, SessionID: "conv-1 finding #10"}} {
		candidate := toolcall.WithInvocation(agent.WithRunInfo(context.Background(), info), toolcall.New(identity))
		if findingReportSourceFromContext(candidate) != nil {
			t.Fatalf("ambiguous/non-chat source accepted: %+v", info)
		}
	}
	if findingReportSourceFromContext(agent.WithRunInfo(context.Background(), agent.RunInfo{Kind: agent.KindChat, SessionID: "conv-10"})) != nil {
		t.Fatal("missing SDK invocation was guessed from a session name")
	}
}
