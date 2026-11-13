package server

import (
	"context"
	"strconv"
	"strings"

	"github.com/Autumn-27/norma/toolcall"
	"gondar/agent"
	"gondar/db"
)

// Neither model arguments nor conversation titles supply report provenance.
// The SDK identity is attached to this actual tool invocation by the host.
func findingReportSourceFromContext(ctx context.Context) *db.FindingReportSource {
	ri := agent.RunInfoFrom(ctx)
	identity, ok := toolcall.IdentityFrom(ctx)
	if !ok || !identity.Valid() || identity.ToolName != "update_finding_report" || ri.Kind != agent.KindChat || !strings.HasPrefix(ri.SessionID, "conv-") {
		return nil
	}
	raw := strings.TrimPrefix(ri.SessionID, "conv-")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != raw {
		return nil
	}
	return &db.FindingReportSource{Kind: "conversation", TaskID: ri.TaskID, ConversationID: id, RunID: identity.RunID, PromptID: identity.PromptID, InvocationID: identity.InvocationID, ToolUseID: identity.ToolUseID}
}
