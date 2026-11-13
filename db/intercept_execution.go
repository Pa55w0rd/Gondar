package db

import (
	"errors"
	"fmt"
	"strconv"
)

var ErrInterceptTaskDeleted = errors.New("任务已被删除或归档")
var ErrInterceptSessionDeleted = errors.New("对应会话或执行记录已被删除或不存在")

var ErrInterceptExecutionUnavailable = errors.New("未找到可唯一关联的原始工具调用；记录可能已删除，或旧审批没有保存关联 ID")

// InterceptExecution is a navigation target read from original activity rows.
// It is not model context and never falls back to matching command text.
type InterceptExecution struct {
	ConversationID *int64     `json:"conversation_id,omitempty"`
	TaskID         *string    `json:"task_id,omitempty"`
	Session        string     `json:"session"`
	Seq            int64      `json:"seq"`
	Items          []Activity `json:"-"`
}

func (d *DB) GetInterceptExecution(id int64) (*InterceptExecution, error) {
	approval, err := d.GetInterceptDetail(id)
	if err != nil || approval == nil {
		return nil, err
	}
	audit := approval.Audit
	if audit == nil || audit.Correlation != "exact" || audit.ToolUseID == "" {
		return nil, ErrInterceptExecutionUnavailable
	}
	var query string
	var scope any
	if approval.ConversationID != nil {
		scope = *approval.ConversationID
		query = `SELECT id, NULL::bigint, COALESCE(worker,''), kind, COALESCE(tool,''), tool_use_id, is_error, COALESCE(summary,''), created_at, NULL::integer,metadata FROM conversation_activities WHERE conversation_id=$1`
	} else if approval.TaskID != nil {
		taskID, parseErr := strconv.ParseInt(*approval.TaskID, 10, 64)
		if parseErr != nil || taskID <= 0 {
			return nil, ErrInterceptExecutionUnavailable
		}
		var exists bool
		if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND archived_at IS NULL AND deleted_at IS NULL)`, taskID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrInterceptTaskDeleted
		}
		scope = taskID
		query = `SELECT id, node_id, COALESCE(worker,''), kind, COALESCE(tool,''), tool_use_id, is_error, COALESCE(summary,''), created_at, main_seg,metadata FROM activity WHERE exploration_id=(SELECT exploration_id FROM tasks WHERE id=$1 AND archived_at IS NULL AND deleted_at IS NULL)`
	} else {
		return nil, ErrInterceptExecutionUnavailable
	}
	// Three matches suffice to detect duplicate IDs without loading a transcript.
	args := []any{scope, audit.ToolUseID}
	filter := ` AND tool_use_id=$2 AND kind IN ('tool_use','tool_result') ORDER BY id LIMIT 3`
	if audit.InvocationID != "" {
		filter = ` AND tool_use_id=$2 AND metadata#>>'{tool_call_identity,run_id}'=$3 AND metadata#>>'{tool_call_identity,invocation_id}'=$4
AND kind IN ('tool_use','tool_result','tool_late_result') ORDER BY id LIMIT 4`
		args = append(args, audit.RunID, audit.InvocationID)
	}
	rows, err := d.Query(query+filter, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &InterceptExecution{ConversationID: approval.ConversationID, TaskID: approval.TaskID}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Worker, &a.Kind, &a.Tool, &a.ToolUseID, &a.IsError, &a.Summary, &a.CreatedAt, &a.MainSeg, &a.Metadata); err != nil {
			return nil, err
		}
		out.Items = append(out.Items, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Items) == 0 {
		return nil, ErrInterceptSessionDeleted
	}
	if len(out.Items) > 2 && (audit.InvocationID == "" || len(out.Items) > 3 || out.Items[2].Kind != "tool_late_result") {
		return nil, ErrInterceptExecutionUnavailable
	}
	call := out.Items[0]
	if call.Kind != "tool_use" || call.Tool != approval.ToolName {
		return nil, ErrInterceptExecutionUnavailable
	}
	if len(out.Items) >= 2 {
		result := out.Items[1]
		if result.Kind != "tool_result" || result.Worker != call.Worker || (result.Tool != "" && result.Tool != call.Tool) || !sameOptionalInt64(call.NodeID, result.NodeID) || !sameMainSegment(call.MainSeg, result.MainSeg) {
			return nil, ErrInterceptExecutionUnavailable
		}
	}
	if len(out.Items) == 3 {
		late := out.Items[2]
		if late.Worker != call.Worker || late.Tool != call.Tool || !sameOptionalInt64(call.NodeID, late.NodeID) || !sameMainSegment(call.MainSeg, late.MainSeg) {
			return nil, ErrInterceptExecutionUnavailable
		}
	}
	out.Seq = call.ID
	if approval.ConversationID == nil {
		switch {
		case call.Worker == "mainagent":
			seg := 0
			if call.MainSeg != nil {
				seg = *call.MainSeg
			}
			out.Session = fmt.Sprintf("main:%d", seg)
		case call.Worker == "planner":
			out.Session = "plan"
		case call.NodeID != nil:
			out.Session = fmt.Sprintf("intent:%d", *call.NodeID)
		default:
			return nil, ErrInterceptExecutionUnavailable
		}
	}
	return out, nil
}

func sameOptionalInt64(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
func sameMainSegment(a, b *int) bool {
	av, bv := 0, 0
	if a != nil {
		av = *a
	}
	if b != nil {
		bv = *b
	}
	return av == bv
}
