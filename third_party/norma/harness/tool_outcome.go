package harness

import (
	"context"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/toolcall"
)

type lateToolObserverKey struct{}

// WithLateToolObserver records a result arriving after its synthetic interruption.
// The callback can outlive Query, must be concurrency-safe, and is never sent to
// the provider or used to overwrite the invocation's primary terminal result.
func WithLateToolObserver(ctx context.Context, observer func(Event)) context.Context {
	return context.WithValue(ctx, lateToolObserverKey{}, observer)
}

func (l *loop) newInvocation(block llm.ContentBlock) *toolcall.Invocation {
	if l.runID == "" {
		l.runID = toolcall.FreshID()
	}
	if l.promptID == "" {
		l.promptID = toolcall.FreshID()
	}
	l.callIndex++
	return toolcall.New(toolcall.Identity{Version: toolcall.Version, RunID: l.runID, PromptID: l.promptID, InvocationID: toolcall.FreshID(), ToolUseID: block.ID, ToolName: block.Name, Turn: l.turnCount, CallIndex: l.callIndex})
}

func (l *loop) executionContext() context.Context {
	if l.toolCtx != nil {
		return l.toolCtx
	}
	if l.ctx != nil {
		return l.ctx
	}
	return context.Background()
}

func (l *loop) lateToolResult(event Event) {
	if observer, _ := l.ctx.Value(lateToolObserverKey{}).(func(Event)); observer != nil {
		observer(event)
	}
}
