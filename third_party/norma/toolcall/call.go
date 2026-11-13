// Package toolcall carries host-only invocation identity and execution facts.
// These values never become provider ContentBlock fields or tool result text.
package toolcall

import (
	"context"
	"crypto/rand"
	"sync"
)

const Version = 1

type Kind string
type ExecutionState string

const (
	Pending             Kind           = "pending"
	SchemaInvalid       Kind           = "schema_invalid"
	UnknownTool         Kind           = "unknown_tool"
	PermissionDenied    Kind           = "permission_denied"
	HookPrevented       Kind           = "hook_prevented"
	PolicyDenied        Kind           = "policy_denied"
	ApprovalRejected    Kind           = "approval_rejected"
	ApprovalTimeout     Kind           = "approval_timeout"
	SchedulerDeferred   Kind           = "scheduler_deferred"
	PreconditionMissing Kind           = "precondition_missing"
	EvidenceIncomplete  Kind           = "evidence_incomplete"
	ExecutedSuccess     Kind           = "executed_success"
	ExecutionError      Kind           = "execution_error"
	Cancelled           Kind           = "cancelled"
	ConfiguredDeadline  Kind           = "configured_deadline"
	UnknownLegacy       Kind           = "unknown_legacy"
	NotExecuted         ExecutionState = "not_executed"
	Executed            ExecutionState = "executed"
	Unknown             ExecutionState = "unknown"
)

type Identity struct {
	Version      int    `json:"version"`
	RunID        string `json:"run_id"`
	PromptID     string `json:"prompt_id"`
	InvocationID string `json:"invocation_id"`
	ToolUseID    string `json:"tool_use_id"`
	ToolName     string `json:"tool_name"`
	Turn         int    `json:"turn"`
	CallIndex    int    `json:"call_index"`
}

func (i Identity) Valid() bool {
	return i.Version == Version && i.RunID != "" && i.PromptID != "" && i.InvocationID != "" && i.ToolUseID != ""
}
func (i Identity) Key() string { return i.RunID + ":" + i.InvocationID }

type Outcome struct {
	Identity
	Phase                string         `json:"phase"`
	Kind                 Kind           `json:"kind"`
	ExecutionState       ExecutionState `json:"execution_state"`
	Source               string         `json:"source"`
	ReasonCode           string         `json:"reason_code,omitempty"`
	RuleID               int64          `json:"rule_id,omitempty"`
	OriginalInputDigest  string         `json:"original_input_digest,omitempty"`
	EffectiveInputDigest string         `json:"effective_input_digest,omitempty"`
	CorrectionPaths      []string       `json:"correction_paths,omitempty"`
}

type runKey struct{}
type promptKey struct{}
type callKey struct{}

func WithRunID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, runKey{}, id)
}
func RunID(ctx context.Context) string { id, _ := ctx.Value(runKey{}).(string); return id }
func WithPromptID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, promptKey{}, id)
}
func PromptID(ctx context.Context) string { id, _ := ctx.Value(promptKey{}).(string); return id }
func FreshID() string                     { return rand.Text() }

// Invocation is shared only by the SDK and trusted host code. External JSON is
// never decoded into this context value.
type Invocation struct {
	mu      sync.Mutex
	id      Identity
	started bool
	final   bool
	marked  bool
	out     Outcome
}

func New(id Identity) *Invocation {
	return &Invocation{id: id, out: Outcome{Identity: id, Phase: "registered", Kind: Pending, ExecutionState: NotExecuted, Source: "sdk"}}
}
func WithInvocation(ctx context.Context, call *Invocation) context.Context {
	return context.WithValue(ctx, callKey{}, call)
}
func FromContext(ctx context.Context) *Invocation {
	call, _ := ctx.Value(callKey{}).(*Invocation)
	return call
}
func IdentityFrom(ctx context.Context) (Identity, bool) {
	c := FromContext(ctx)
	if c == nil {
		return Identity{}, false
	}
	return c.Identity(), true
}
func (c *Invocation) Identity() Identity { return c.id }

// InputFacts records hashes and schema paths only; raw inputs remain in the
// original transcript and may contain secrets. Nil paths preserve prior repair
// provenance when a later trusted permission/hook updates the effective input.
func (c *Invocation) InputFacts(original, effective string, paths []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.out.OriginalInputDigest, c.out.EffectiveInputDigest = original, effective
	if paths != nil {
		c.out.CorrectionPaths = append([]string(nil), paths...)
	}
}

// Mark supplies a typed decision from a host code branch, not from output text.
// A late host may describe its actual result, but cannot change the primary
// terminal event already emitted by the executor.
func Mark(ctx context.Context, kind Kind, state ExecutionState, source, reason string, ruleID int64) {
	if c := FromContext(ctx); c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.out.Kind, c.out.ExecutionState, c.out.Source, c.out.ReasonCode, c.out.RuleID, c.marked = kind, state, source, reason, ruleID, true
	}
}
func MarkDefault(ctx context.Context, kind Kind, state ExecutionState, source, reason string) {
	if c := FromContext(ctx); c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.marked {
			c.out.Kind, c.out.ExecutionState, c.out.Source, c.out.ReasonCode, c.marked = kind, state, source, reason, true
		}
	}
}
func (c *Invocation) StartExecution() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.final {
		return false
	}
	c.started = true
	return true
}
func (c *Invocation) Result(failed bool) Outcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.out
	out.Phase = "result"
	if !c.marked {
		out.Kind, out.ExecutionState, out.Source, out.ReasonCode = ExecutedSuccess, Executed, "sdk", ""
		if failed {
			out.Kind = ExecutionError
		}
	}
	return out
}
func (c *Invocation) Finish(out Outcome) { c.mu.Lock(); defer c.mu.Unlock(); c.final = true }
func (c *Invocation) Interrupted(deadline bool) Outcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.out
	out.Phase, out.Kind, out.Source, out.ReasonCode = "result", Cancelled, "sdk", "context_cancelled"
	if deadline {
		out.Kind, out.ReasonCode = ConfiguredDeadline, "configured_deadline"
	}
	out.ExecutionState = NotExecuted
	if c.started {
		out.ExecutionState = Unknown
	}
	c.final = true
	return out
}
