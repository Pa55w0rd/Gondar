package db

import (
	"database/sql"
	"errors"
	"fmt"
)

var ErrTaskExtensionUnavailable = errors.New("任务不存在或已不可修改")
var ErrTaskExtensionFrozen = errors.New("任务正在归档或恢复，当前不能修改附属记录")

// New task-owned stores follow the archive writer's lock order: task first,
// extension row second. Once queued for archiving, a snapshot cannot be changed
// by a late human/model metadata write that would otherwise be silently lost.
func lockTaskExtension(tx *sql.Tx, taskID, expID int64) error {
	var id int64
	if err := tx.QueryRow(`SELECT id FROM tasks WHERE id=$1 AND ($2=0 OR exploration_id=$2) AND deleted_at IS NULL FOR UPDATE`, taskID, expID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTaskExtensionUnavailable
		}
		return fmt.Errorf("锁定任务失败：%w", err)
	}
	return ensureTaskExtensionMutable(tx, taskID)
}

// Caller holds the task row lock before invoking this check.
func ensureTaskExtensionMutable(tx *sql.Tx, taskID int64) error {
	var frozen bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM task_archives WHERE task_id=$1 AND state!='archive_failed')`, taskID).Scan(&frozen); err != nil {
		return err
	}
	if frozen {
		return ErrTaskExtensionFrozen
	}
	return nil
}
