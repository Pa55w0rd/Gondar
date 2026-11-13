package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/toolcall"
)

// rawInputTool is the optional opt-out from schema validation. Discovered by
// assertion rather than declared on tool.CoreTool, so hosts implementing that
// interface are unaffected.
type rawInputTool interface{ AcceptsRawInput() bool }

func acceptsRawInput(t tool.CoreTool) bool {
	r, ok := t.(rawInputTool)
	return ok && r.AcceptsRawInput()
}

// execOne resolves, permission-checks, and runs a single tool call, returning a
// tool_result block and any extra messages the tool injects after its result
// (Result.Extra — e.g. the Skill tool's instructions message). It never panics;
// failures become error results. It is safe to call from the streaming
// executor's goroutines (read-only access to loop config; permission/hook
// callbacks are the host's responsibility to make safe).
func (l *loop) execOne(use llm.ContentBlock, emitProgress func(tool.ProgressInfo)) (llm.ContentBlock, []llm.Message) {
	result, extra, _ := l.execOneInvocation(use, emitProgress, l.newInvocation(use))
	return result, extra
}

func (l *loop) execOneInvocation(use llm.ContentBlock, emitProgress func(tool.ProgressInfo), call *toolcall.Invocation) (result llm.ContentBlock, extra []llm.Message, outcome toolcall.Outcome) {
	base := l.ctx
	if base == nil {
		base = context.Background()
	}
	ctx := toolcall.WithInvocation(base, call)
	toolCtx := toolcall.WithInvocation(l.executionContext(), call)
	defer func() { outcome = call.Result(result.IsError) }()
	if inputNormalizer(ctx) != nil && (use.ID == "" || use.Name == "") {
		toolcall.Mark(ctx, toolcall.SchemaInvalid, toolcall.NotExecuted, "sdk", "stream_identity_invalid", 0)
		return llm.ToolResultText(use.ID, "Error: invalid tool input: tool call identity is incomplete", true), nil, outcome
	}
	t, ok := l.in.Tools.Get(use.Name)
	if !ok {
		toolcall.Mark(ctx, toolcall.UnknownTool, toolcall.NotExecuted, "sdk", "unknown_tool", 0)
		return llm.ToolResultText(use.ID, fmt.Sprintf("Error: unknown tool %q", use.Name), true), nil, outcome
	}
	input := use.Input
	originalDigest := inputDigest(input)
	call.InputFacts(originalDigest, originalDigest, nil)

	// Schema validation (FR-04.5).
	//
	// A tool may opt out by reporting AcceptsRawInput: it parses its own
	// arguments and would rather repair a malformed call than have it rejected
	// here, where the only possible answer is a generic error. The schema is
	// still advertised to the model either way — see tool.Spec.RawInput.
	if !acceptsRawInput(t) {
		validateErr := tool.ValidateInput(t.InputSchema(), input)
		if normalizer := inputNormalizer(ctx); normalizer != nil {
			// In host strict mode an incomplete/empty JSON stream is never
			// repaired into a valid empty object, even for an optional schema.
			if !json.Valid(input) {
				validateErr = fmt.Errorf("input is not complete JSON")
			} else if validateErr != nil {
				if corrected, paths := normalizer(ctx, use.Name, t.InputSchema(), input); len(corrected) > 0 && len(paths) > 0 && json.Valid(corrected) && tool.ValidateInput(t.InputSchema(), corrected) == nil {
					input, validateErr = corrected, nil
					call.InputFacts(originalDigest, inputDigest(input), paths)
				}
			}
		}
		if err := validateErr; err != nil {
			toolcall.Mark(ctx, toolcall.SchemaInvalid, toolcall.NotExecuted, "sdk", "schema_invalid", 0)
			return llm.ToolResultText(use.ID, "Error: invalid tool input: "+err.Error(), true), nil, outcome
		}
	}

	mode := l.in.PermissionMode
	if l.in.PermissionModeFunc != nil {
		mode = l.in.PermissionModeFunc()
	}
	pc := permission.Context{
		Mode:       mode,
		WorkingDir: l.in.WorkingDir,
		Allowed:    l.in.Allowed,
		Disallowed: l.in.Disallowed,
	}
	dec := permission.Evaluate(ctx, permission.Request{
		ToolName:     use.Name,
		Input:        input,
		IsReadOnly:   t.IsReadOnly(input),
		ToolDecision: t.CheckPermissions(ctx, input, pc),
		Ctx:          pc,
		Ask:          l.in.CanUseTool,
	})
	if dec.Behavior != permission.Allow {
		toolcall.Mark(ctx, toolcall.PermissionDenied, toolcall.NotExecuted, "permission", "permission_denied", 0)
		msg := dec.Message
		if msg == "" {
			msg = "denied"
		}
		return llm.ToolResultText(use.ID, "Tool call was not permitted: "+msg, true), nil, outcome
	}
	if len(dec.UpdatedInput) > 0 {
		input = dec.UpdatedInput
		call.InputFacts(originalDigest, inputDigest(input), nil)
	}

	// Pre-tool hook.
	if l.in.Hooks != nil {
		if block, msg, updated := l.in.Hooks.PreToolUse(ctx, use.Name, input); block {
			toolcall.MarkDefault(ctx, toolcall.HookPrevented, toolcall.NotExecuted, "hook", "hook_prevented")
			return llm.ToolResultText(use.ID, "Blocked by hook: "+msg, true), nil, outcome
		} else if len(updated) > 0 {
			input = updated
			call.InputFacts(originalDigest, inputDigest(input), nil)
		}
	}
	if toolCtx.Err() != nil || !call.StartExecution() {
		kind := toolcall.Cancelled
		if errors.Is(toolCtx.Err(), context.DeadlineExceeded) {
			kind = toolcall.ConfiguredDeadline
		}
		toolcall.Mark(ctx, kind, toolcall.NotExecuted, "sdk", string(kind), 0)
		return llm.ToolResultText(use.ID, "tool execution interrupted", true), nil, outcome
	}

	tc := &tool.ToolContext{
		WorkingDir:     l.in.WorkingDir,
		AgentID:        l.in.AgentID,
		ToolUseID:      use.ID,
		Emit:           emitProgress,
		OutputDir:      l.in.ToolOutputDir,
		MaxOutputChars: l.in.MaxToolOutputChars,
		Tasks:          l.in.Tasks,
		Env:            l.in.BashEnv,
	}
	// toolCtx carries the run's MaxDuration deadline, so a tool that overruns the
	// wall-clock budget is interrupted here (the turn loop then enters wrap-up). During
	// the wrap-up phase toolCtx is the live parent ctx, so settlement tools run freely.
	res, err := t.Call(toolCtx, input, tc)
	if err != nil {
		res = tool.Errorf("Error: " + err.Error())
	}
	// Global output cap at the single post-tool choke point: EVERY tool —
	// built-in, MCP, custom, host — is bounded here regardless of whether it
	// remembered to Capture itself, so no tool can overflow the context window
	// (the failure mode that motivated this net). CaptureOnce is idempotent:
	// output a tool already spilled/truncated carries a marker and is left
	// untouched, so this never double-truncates or double-spills.
	res = capOutput(tc, res)

	if l.in.Hooks != nil {
		raw, _ := json.Marshal(res.Flatten())
		l.in.Hooks.PostToolUse(ctx, use.Name, input, raw, res.IsError)
	}

	content := res.Content
	if len(content) == 0 {
		content = []llm.ContentBlock{llm.TextBlock("(no output)")}
	}
	if res.IsError && toolCtx.Err() != nil {
		kind := toolcall.Cancelled
		if errors.Is(toolCtx.Err(), context.DeadlineExceeded) {
			kind = toolcall.ConfiguredDeadline
		}
		toolcall.MarkDefault(ctx, kind, toolcall.Unknown, "sdk", string(kind))
	}
	return llm.ContentBlock{Type: llm.BlockToolResult, ToolUseID: use.ID, Content: content, IsError: res.IsError}, res.Extra, outcome
}

func inputDigest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

// capOutput runs every text block of a tool result through the idempotent
// output cap (tool.CaptureOnce). Non-text blocks pass through untouched. It is
// the one place that guarantees no tool's output — however constructed — can
// blow past the context budget.
func capOutput(tc *tool.ToolContext, res tool.Result) tool.Result {
	for i, b := range res.Content {
		if b.Type == llm.BlockText {
			res.Content[i].Text = tool.CaptureOnce(tc, b.Text)
		}
	}
	return res
}
