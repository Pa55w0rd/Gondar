package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"gondar/agent"
	"gondar/db"
)

// This ledger observes host events, never target response text. It is local to
// one execution and only decides whether an interrupted run is safe to resume.
type executionEvidence struct {
	mu      sync.Mutex
	unknown map[string]bool
	runID   string
}

func (x *executionEvidence) observe(a db.Activity) {
	if a.Kind != "tool_use" && a.Kind != "tool_result" {
		return
	}
	var meta struct {
		Identity *agent.ToolCallIdentity `json:"tool_call_identity"`
	}
	_ = json.Unmarshal(a.Metadata, &meta)
	if meta.Identity != nil && meta.Identity.Valid() && meta.Identity.RunID != x.runID {
		return
	}
	if a.ToolUseID == "" && (meta.Identity == nil || !meta.Identity.Valid()) {
		return
	}
	key := "legacy:" + a.ToolUseID
	outcome, known := agent.ToolOutcomeFromMetadata(a.Metadata)
	if known && outcome.RunID != x.runID {
		return
	}
	known = known && outcome.RunID == x.runID
	if a.Kind == "tool_use" && meta.Identity != nil && meta.Identity.Valid() && meta.Identity.RunID == x.runID {
		key = meta.Identity.InvocationID
	} else if known {
		key = outcome.InvocationID
	}
	if key == "" {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.unknown == nil {
		x.unknown = map[string]bool{}
	}
	if a.Kind == "tool_use" {
		x.unknown[key] = true
	} else if known {
		if outcome.Phase != "late_receipt" {
			x.unknown[key] = outcome.ExecutionState != "executed" && outcome.ExecutionState != "not_executed"
		}
	} else {
		x.unknown[key] = a.IsError // legacy successful receipts are still evidence
	}
}

func (x *executionEvidence) uncertain() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, unknown := range x.unknown {
		if unknown {
			return true
		}
	}
	return false
}

func (e *Engine) executionStoragePending(taskID string) bool {
	value, ok := e.executionSaving.Load(taskID)
	return ok && value.(*atomic.Int64).Load() > 0
}

// Database retries never run the model or target tools again. Keep ownership
// while storage is unavailable and stop new task work until receipts can commit.
// Cancellation still releases task deletion/shutdown; the durable pre-execution
// marker then makes startup isolate an unknown outcome rather than replay it.
func (e *Engine) persistExecution(ctx context.Context, t *Task, id int64, write func(context.Context) (bool, error)) (bool, error) {
	var pending *atomic.Int64
	defer func() {
		if pending != nil {
			pending.Add(-1)
			t.Notify()
		}
	}()
	for attempt := 0; ; attempt++ {
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		saved, err := write(callCtx)
		cancel()
		if err == nil || errors.Is(err, db.ErrIntentStateConflict) {
			return saved, nil
		}
		if pending == nil {
			value, _ := e.executionSaving.LoadOrStore(t.ID, &atomic.Int64{})
			pending = value.(*atomic.Int64)
			pending.Add(1)
			metadata, _ := json.Marshal(map[string]any{"execution_storage": map[string]any{"intent_id": id, "phase": "saving", "persisted": false, "target_operations_replayed": false}})
			// Storage is already failing: do not synchronously write this warning
			// into the same DB and defeat the bounded retry/shutdown deadline.
			e.bc.Publish(t.ID, db.Activity{NodeID: &id, Worker: "system", Kind: "text", IsError: true,
				Summary: "执行记录存储暂不可用，结果尚未保存，正在重试", Metadata: metadata, CreatedAt: time.Now()})
			log.Printf("[execution] task %s intent %d storage pending: %v", t.ID, id, err)
		}
		if ctx.Err() != nil {
			return false, err
		}
		delay := 200 * time.Millisecond
		if attempt >= 2 {
			delay = time.Second
		}
		if sleepCtx(ctx, delay) {
			return false, err
		}
	}
}
