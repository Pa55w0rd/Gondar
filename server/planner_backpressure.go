package server

import (
	"context"
	"time"

	"gondar/agent"
	"gondar/db"
)

// Only an otherwise empty heartbeat may yield to an already productive queue.
// Concrete changes and empty frontiers always reach the existing planner.
func skipBackloggedHeartbeat(source string, triggers []agent.TriggerEvent, backlog db.IntentBacklog) bool {
	return source == "heartbeat" && len(triggers) == 0 && backlog.Open >= 32 && backlog.Running > 0
}

func (e *Engine) deferBackloggedHeartbeat(ctx context.Context, t *Task, source string) bool {
	if source != "heartbeat" {
		return false
	}
	t.trigMu.Lock()
	busy := len(t.pendingTriggers) > 0
	t.trigMu.Unlock()
	if busy {
		return false
	}
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	backlog, err := t.Store.IntentBacklog(readCtx)
	if err != nil {
		return false
	}
	t.trigMu.Lock()
	defer t.trigMu.Unlock()
	return skipBackloggedHeartbeat(source, t.pendingTriggers, backlog)
}
