package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/toolcall"
)

func (l *loop) evaluateToolPermission(ctx context.Context, target tool.CoreTool, name string, input json.RawMessage) permission.Decision {
	mode := l.in.PermissionMode
	if l.in.PermissionModeFunc != nil {
		mode = l.in.PermissionModeFunc()
	}
	pc := permission.Context{Mode: mode, WorkingDir: l.in.WorkingDir, Allowed: l.in.Allowed, Disallowed: l.in.Disallowed}
	return permission.Evaluate(ctx, permission.Request{
		ToolName: name, Input: input, IsReadOnly: target.IsReadOnly(input),
		ToolDecision: target.CheckPermissions(ctx, input, pc), Ctx: pc, Ask: l.in.CanUseTool,
	})
}

// Resolve the host-created gateway before any target execution starts. All
// subsequent schema/permission/hook checks use the actual tool and parameters,
// while the caller keeps the immutable outer invocation and provider result ID.
func (l *loop) resolveDeferredInvocation(ctx context.Context, gateway tool.CoreTool, input json.RawMessage) (tool.CoreTool, json.RawMessage, error) {
	resolver, ok := gateway.(tool.DeferredCallResolver)
	if !ok {
		toolcall.Mark(ctx, toolcall.SchemaInvalid, toolcall.NotExecuted, "sdk", "deferred_resolver_unavailable", 0)
		return nil, nil, fmt.Errorf("deferred gateway resolver is unavailable")
	}
	target, params, err := resolver.ResolveDeferredCall(input)
	if err == nil {
		// Preserve explicit permission restrictions on the outer access tool too.
		decision := l.evaluateToolPermission(ctx, gateway, gateway.Name(), input)
		if decision.Behavior != permission.Allow {
			toolcall.Mark(ctx, toolcall.PermissionDenied, toolcall.NotExecuted, "permission", "deferred_gateway_denied", 0)
			return nil, nil, fmt.Errorf("deferred gateway was not permitted: %s", decision.Message)
		}
		if len(decision.UpdatedInput) > 0 {
			target, params, err = resolver.ResolveDeferredCall(decision.UpdatedInput)
		}
	}
	if err != nil {
		kind := toolcall.SchemaInvalid
		if errors.Is(err, tool.ErrDeferredToolUnknown) {
			kind = toolcall.UnknownTool
		} else if errors.Is(err, tool.ErrDeferredToolLocked) {
			kind = toolcall.PermissionDenied
		}
		toolcall.Mark(ctx, kind, toolcall.NotExecuted, "sdk", "deferred_resolution_failed", 0)
		return nil, nil, err
	}
	return target, params, nil
}
