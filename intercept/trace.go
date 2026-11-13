package intercept

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/norma/toolcall"

	"gondar/db"
)

const (
	contextLimit = 24
	entryLimit   = 8 * 1024
	promptLimit  = 32 * 1024
	outputLimit  = 64 * 1024
)

type traceKey struct{}
type callKey struct{}
type completion func(status, output string, truncated bool)

type tracedCall struct {
	key         string
	audit       db.InterceptAudit
	claimed     bool
	ambiguous   bool
	complete    completion
	primaryDone chan struct{}
}

// Trace belongs to ONE Prompt invocation. New SDK calls carry a trusted host
// invocation identity. Legacy hooks correlate only one outstanding matching
// input; never guess between simultaneous identical requests.
type Trace struct {
	mu      sync.Mutex
	runID   string
	user    string
	userCut bool
	entries []db.InterceptContextEntry
	cut     bool
	calls   map[string]*tracedCall
	late    map[string]*tracedCall
}

func WithTrace(ctx context.Context, user string, prior []db.InterceptContextEntry) (context.Context, *Trace) {
	runID := toolcall.RunID(ctx)
	if runID == "" {
		runID = rand.Text()
	}
	t := &Trace{runID: runID, calls: make(map[string]*tracedCall)}
	t.user, t.userCut = bounded(user, promptLimit)
	for _, e := range prior {
		t.append(e)
	}
	return context.WithValue(ctx, traceKey{}, t), t
}

func (t *Trace) RunID() string { return t.runID }

func (t *Trace) StartInvocation(id toolcall.Identity, tool string, input []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls[id.Key()] = &tracedCall{key: tool + ":" + digestInput(input), primaryDone: make(chan struct{}), audit: db.InterceptAudit{
		RunID: id.RunID, PromptID: id.PromptID, InvocationID: id.InvocationID, ToolUseID: id.ToolUseID, Correlation: "exact", InputDigest: digestInput(input),
		UserMessage: t.user, UserTruncated: t.userCut, CapturedAt: time.Now().UTC(), Context: append([]db.InterceptContextEntry{}, t.entries...), ContextTruncated: t.cut,
	}}
	t.append(db.InterceptContextEntry{Kind: "tool_use", Tool: tool, ToolUseID: id.ToolUseID, Text: string(input)})
}

func (t *Trace) append(e db.InterceptContextEntry) {
	var cut bool
	e.Text, cut = bounded(e.Text, entryLimit)
	e.Truncated = e.Truncated || cut
	t.cut = t.cut || e.Truncated
	t.entries = append(t.entries, e)
	if len(t.entries) > contextLimit {
		t.entries = append([]db.InterceptContextEntry(nil), t.entries[len(t.entries)-contextLimit:]...)
		t.cut = true
	}
}

func (t *Trace) Append(e db.InterceptContextEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.append(e)
}

func (t *Trace) Start(id, tool string, input []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls[id] = &tracedCall{key: tool + ":" + digestInput(input), primaryDone: make(chan struct{}), audit: db.InterceptAudit{
		RunID: t.runID, ToolUseID: id, Correlation: "exact", InputDigest: digestInput(input),
		UserMessage: t.user, UserTruncated: t.userCut, CapturedAt: time.Now().UTC(),
		Context: append([]db.InterceptContextEntry{}, t.entries...), ContextTruncated: t.cut,
	}}
	t.append(db.InterceptContextEntry{Kind: "tool_use", Tool: tool, ToolUseID: id, Text: string(input)})
}

// WithCall claims the event before model review starts, so subsequent tools
// cannot change this approval's context while the judge is running.
func WithCall(ctx context.Context, tool string, input []byte) context.Context {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	a := db.InterceptAudit{Correlation: "unavailable", InputDigest: digestInput(input), CapturedAt: time.Now().UTC()}
	if t != nil {
		t.mu.Lock()
		if id, ok := toolcall.IdentityFrom(ctx); ok && id.Valid() {
			if c := t.calls[id.Key()]; c != nil && c.audit.RunID == id.RunID && c.audit.PromptID == id.PromptID && c.audit.ToolUseID == id.ToolUseID {
				c.claimed = true
				a = c.audit
				// Effective parameters can differ only through a trusted host
				// normalizer/permission/hook update; the call identity remains exact.
				a.InputDigest = digestInput(input)
			} else {
				a.RunID, a.PromptID, a.InvocationID, a.ToolUseID = id.RunID, id.PromptID, id.InvocationID, id.ToolUseID
			}
			t.mu.Unlock()
			return context.WithValue(ctx, callKey{}, a)
		}
		var candidates []*tracedCall
		for _, c := range t.calls {
			if !c.claimed && c.key == tool+":"+a.InputDigest {
				candidates = append(candidates, c)
			}
		}
		if len(candidates) == 1 && !candidates[0].ambiguous {
			candidates[0].claimed = true
			a = candidates[0].audit
		} else {
			a.RunID, a.UserMessage, a.UserTruncated = t.runID, t.user, t.userCut
			if len(candidates) > 0 {
				a.Correlation = "ambiguous"
				for _, c := range candidates {
					c.ambiguous = true
				}
			}
		}
		t.mu.Unlock()
	}
	return context.WithValue(ctx, callKey{}, a)
}

func (t *Trace) bind(id string, f completion) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c := t.calls[id]; c != nil {
		c.complete = f
	}
}

func (t *Trace) Complete(id, output string, isError bool) {
	t.CompleteOutcome(id, output, isError, nil)
}

func (t *Trace) CompleteOutcome(id, output string, isError bool, outcome *toolcall.Outcome) {
	t.mu.Lock()
	c := t.calls[id]
	delete(t.calls, id)
	if c != nil && c.complete != nil && outcome != nil && outcome.ExecutionState == toolcall.Unknown {
		if t.late == nil {
			t.late = map[string]*tracedCall{}
		}
		t.late[id] = c
	}
	t.mu.Unlock()
	if c == nil || c.complete == nil {
		return
	}
	status := "succeeded"
	if isError {
		status = "failed"
	}
	if outcome != nil {
		switch outcome.ExecutionState {
		case toolcall.NotExecuted:
			status = "not_executed"
		case toolcall.Unknown:
			status = "unknown"
		}
	}
	out, cut := bounded(output, outputLimit)
	c.complete(status, out, cut)
	close(c.primaryDone)
}

func (t *Trace) CompleteLate(id, output string, isError bool, outcome *toolcall.Outcome) {
	if outcome == nil || outcome.Phase != "late_receipt" {
		return
	}
	t.mu.Lock()
	c := t.late[id]
	delete(t.late, id)
	t.mu.Unlock()
	if c == nil || c.complete == nil {
		return
	}
	// A tool may finish while the primary unknown receipt is still being
	// persisted. Its supplemental receipt must never overtake that write.
	<-c.primaryDone
	status := "late_succeeded"
	if isError {
		status = "late_failed"
	}
	if outcome.ExecutionState != toolcall.Executed {
		status = "late_unknown"
	}
	out, cut := bounded(output, outputLimit)
	c.complete(status, out, cut)
}

// Finish marks missing results unknown, never successful. The tool may have
// been interrupted or its final event lost; this is distinct from a tool error.
func (t *Trace) Finish() {
	t.mu.Lock()
	calls := t.calls
	t.calls = make(map[string]*tracedCall)
	if t.late == nil {
		t.late = map[string]*tracedCall{}
	}
	for key, c := range calls {
		if c.complete != nil && c.audit.InvocationID != "" {
			t.late[key] = c
		}
	}
	t.mu.Unlock()
	for _, c := range calls {
		if c.complete != nil {
			c.complete("unknown", "执行结束但未收到工具结果", false)
			close(c.primaryDone)
		}
	}
}

func auditFor(ctx context.Context, dec Decision, input []byte, status string) *db.InterceptAudit {
	a, ok := ctx.Value(callKey{}).(db.InterceptAudit)
	if !ok {
		a = db.InterceptAudit{Correlation: "unavailable", InputDigest: digestInput(input), CapturedAt: time.Now().UTC()}
	}
	a.InitialAction, a.InitialReason = dec.Action, dec.Message
	a.ModelFallback = dec.ModelFallback
	a.ModelInput, a.ModelInputDigest = dec.ModelInput, dec.ModelInputDigest
	a.RuleName, a.ConfigDigest, a.ProfileID = dec.RuleName, dec.ConfigDigest, dec.ProfileID
	a.ExecutionStatus = "not_started"
	if status == "allowed" {
		a.EffectiveAction, a.ExecutionStatus = "allow", "awaiting_result"
		if a.Correlation != "exact" {
			a.ExecutionStatus = "unknown"
		}
		// Keep the exact model input for EVERY model verdict, including automatic
		// allows. Raw audit history is not model input. Preserve the existing
		// lightweight allow-retention policy; render the saved input directly.
		a.UserMessage, a.UserTruncated = "", false
		a.Context, a.ContextTruncated = nil, false
	}
	if status == "denied" {
		a.EffectiveAction, a.ExecutionStatus = "deny", "not_executed"
	}
	return &a
}

func (i *Interceptor) bindResult(ctx context.Context, id int64, audit *db.InterceptAudit) {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	if t == nil || audit.Correlation != "exact" || audit.ToolUseID == "" {
		return
	}
	key := audit.ToolUseID
	if audit.InvocationID != "" {
		key = audit.RunID + ":" + audit.InvocationID
	}
	t.bind(key, func(status, output string, cut bool) {
		_ = i.completeRecordedResult(id, audit, status, output, cut)
	})
}

func (i *Interceptor) completeRecordedResult(id int64, audit *db.InterceptAudit, status, output string, cut bool) error {
	if status == "late_succeeded" || status == "late_failed" || status == "late_unknown" {
		return i.db.AppendInterceptLateReceipt(id, audit.RunID, audit.InvocationID, audit.ToolUseID, status, output, cut)
	}
	if audit.InvocationID != "" {
		return i.db.CompleteInterceptInvocation(id, audit.RunID, audit.InvocationID, audit.ToolUseID, status, output, cut)
	}
	return i.db.CompleteIntercept(id, audit.RunID, audit.ToolUseID, status, output, cut)
}

func digestInput(input []byte) string {
	var value any
	d := json.NewDecoder(bytes.NewReader(input))
	d.UseNumber()
	if d.Decode(&value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			input = canonical
		}
	}
	h := sha256.Sum256(input)
	return hex.EncodeToString(h[:])
}

func bounded(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	end := limit
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end], true
}
