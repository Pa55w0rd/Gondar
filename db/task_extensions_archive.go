package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

func restoreTaskExtensions(tx *sql.Tx, tables map[string]json.RawMessage) error {
	retests, err := decodeArchiveRows(tables["finding_retests"])
	if err != nil {
		return err
	}
	for _, row := range retests {
		if id, ok := jsonInt64(row["conversation_id"]); ok && id > 0 {
			var exists bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM conversations WHERE id=$1)`, id).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				row["conversation_id"] = nil
			}
		}
		if state, _ := row["status"].(string); state == "pending" || state == "running" {
			row["status"] = "stopped"
			row["error"] = "归档恢复，旧复测不自动重发"
		}
	}
	// Cold tasks must never silently re-enable autonomous future runs on restore.
	// Credentials and current model availability should be reviewed in a new plan.
	schedules, err := decodeArchiveRows(tables["daily_task_schedules"])
	if err != nil {
		return err
	}
	for _, row := range schedules {
		if enabled, _ := row["enabled"].(bool); enabled {
			row["last_error"] = "归档恢复后保持停用，请按当前配置新建计划"
		}
		row["enabled"] = false
	}
	copyTables := make(map[string]json.RawMessage, len(tables))
	for key, value := range tables {
		copyTables[key] = value
	}
	copyTables["daily_task_schedules"], err = json.Marshal(schedules)
	if err != nil {
		return err
	}
	copyTables["finding_retests"], err = json.Marshal(retests)
	if err != nil {
		return err
	}
	for _, name := range []string{"finding_retests", "agent_lessons", "agent_lesson_reviews", "daily_task_schedules", "daily_task_occurrences", "task_resources", "task_resource_events"} {
		if err := insertArchiveRows(tx, name, copyTables[name]); err != nil {
			return fmt.Errorf("restore %s: %w", name, err)
		}
	}
	return nil
}
