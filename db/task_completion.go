package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const (
	CompletionObjective  = "objective"
	CompletionContinuous = "continuous"
)

var ErrTaskExecutionActive = errors.New("请先暂停任务并等待当前执行停止，再修改完成策略或时限")

func NormalizeCompletionMode(mode string) (string, error) {
	switch strings.TrimSpace(mode) {
	case "", CompletionObjective:
		return CompletionObjective, nil
	case CompletionContinuous:
		return CompletionContinuous, nil
	default:
		return "", fmt.Errorf("完成策略必须为 objective 或 continuous")
	}
}

func normalizeArchivedCompletionMode(format int, row map[string]any) error {
	value, present := row["completion_mode"]
	if format < 5 && (!present || value == nil) {
		row["completion_mode"] = CompletionObjective
		return nil
	}
	mode, ok := value.(string)
	if !ok || (mode != CompletionObjective && mode != CompletionContinuous) {
		return fmt.Errorf("%w: 归档完成策略缺失或无效", ErrTaskArchiveFormatMismatch)
	}
	return nil
}

// TaskCompletionMode reads trusted task configuration, never model-written graph
// evidence. Bare stores used by graph callers retain objective semantics.
func (s *ExplorationStore) TaskCompletionMode(ctx context.Context) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, `SELECT completion_mode FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL`, s.expID).Scan(&mode)
	if err == sql.ErrNoRows {
		return CompletionObjective, nil
	}
	if err != nil {
		return "", err
	}
	return NormalizeCompletionMode(mode)
}

type TaskExecutionPatch struct {
	CompletionMode *string
	TimeoutSeconds *int
}

func (d *DB) UpdateTaskExecution(id int64, patch TaskExecutionPatch) (*Task, error) {
	if patch.CompletionMode == nil && patch.TimeoutSeconds == nil {
		return nil, fmt.Errorf("至少提供完成策略或时限")
	}
	if patch.CompletionMode != nil {
		mode, err := NormalizeCompletionMode(*patch.CompletionMode)
		if err != nil {
			return nil, err
		}
		patch.CompletionMode = &mode
	}
	if patch.TimeoutSeconds != nil && *patch.TimeoutSeconds < 0 {
		return nil, fmt.Errorf("任务时限不能为负数")
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var status string
	var paused, queued bool
	err = tx.QueryRow(`SELECT status,paused,queued FROM tasks WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&status, &paused, &queued)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !paused && !queued && !IsTerminal(status) {
		return nil, ErrTaskExecutionActive
	}
	if err := ensureTaskExtensionMutable(tx, id); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE tasks SET completion_mode=COALESCE($2,completion_mode),
timeout_seconds=COALESCE($3,timeout_seconds),
first_run_at=CASE WHEN $3::integer IS NOT NULL THEN NULL ELSE first_run_at END,
deadline_at=CASE WHEN $3::integer IS NOT NULL THEN NULL ELSE deadline_at END WHERE id=$1`, id, patch.CompletionMode, patch.TimeoutSeconds); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return d.GetTask(id)
}

// A continuous run can stop for lack of progress without claiming its target is
// exhausted. The predicate prevents this background action overriding a user
// pause, an archive/deletion, a terminal status, or a queued admission.
func (d *DB) PauseContinuousTask(id int64) (bool, error) {
	res, err := d.Exec(`UPDATE tasks SET paused=true,completed_at=NULL WHERE id=$1
AND completion_mode='continuous' AND status NOT IN ('done','failed','timeout')
AND NOT paused AND NOT queued AND deleted_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
