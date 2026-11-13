package agent

import (
	"context"
	"encoding/json"

	"github.com/Autumn-27/norma/toolcall"
)

// WithExecutionRunID binds every Prompt in one host worker execution to the
// host's execution fence. captureRunSession adds a fresh PromptID; the SDK adds
// a fresh InvocationID for every call, independent of provider-supplied IDs.
func WithExecutionRunID(ctx context.Context, id string) context.Context {
	return toolcall.WithRunID(ctx, id)
}

type ToolOutcome = toolcall.Outcome
type ToolCallIdentity = toolcall.Identity

// ToolOutcomeFromMetadata decodes the reserved capture metadata namespace. It
// validates its shape, not arbitrary archived data's provenance: callers must
// additionally match RunID to their current host-owned execution fence.
func ToolOutcomeFromMetadata(metadata json.RawMessage) (ToolOutcome, bool) {
	var record struct {
		Outcome *ToolOutcome `json:"tool_outcome"`
	}
	if json.Unmarshal(metadata, &record) != nil || record.Outcome == nil || !record.Outcome.Identity.Valid() {
		return ToolOutcome{}, false
	}
	return *record.Outcome, true
}

func toolCallMetadata(id *toolcall.Identity, outcome *toolcall.Outcome) json.RawMessage {
	if id == nil || !id.Valid() {
		return nil
	}
	fields := map[string]any{"tool_call_identity": id, "activity_origin": "runtime"}
	if outcome != nil {
		fields["tool_outcome"] = outcome
	}
	raw, _ := json.Marshal(fields)
	return raw
}
