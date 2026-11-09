package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

func (s *ExplorationStore) lockPlannerTask(tx *sql.Tx) error {
	var taskID int64
	var deleted sql.NullTime
	err := tx.QueryRow(`SELECT id,deleted_at FROM tasks WHERE exploration_id=$1 FOR UPDATE`, s.expID).Scan(&taskID, &deleted)
	if err == sql.ErrNoRows {
		return nil
	} // bare exploration stores remain valid
	if err != nil {
		return err
	}
	if deleted.Valid {
		return ErrTaskExtensionUnavailable
	}
	return ensureTaskExtensionMutable(tx, taskID)
}

// PlannerState is a bounded handoff, not authoritative task evidence. Each
// planning round owns a revision so an abandoned old round cannot overwrite it.
type PlannerState struct {
	Revision int64
	Todos    json.RawMessage
	Summary  string
	Outcome  string
}

func (s *ExplorationStore) BeginPlannerRound() (PlannerState, error) {
	var p PlannerState
	tx, err := s.db.Begin()
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	if err := s.lockPlannerTask(tx); err != nil {
		return p, err
	}
	err = tx.QueryRow(`INSERT INTO exploration_planner_state(exploration_id,revision) VALUES($1,1)
ON CONFLICT(exploration_id) DO UPDATE SET revision=exploration_planner_state.revision+1
RETURNING revision,todos,summary,outcome`, s.expID).Scan(&p.Revision, &p.Todos, &p.Summary, &p.Outcome)
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}

func (s *ExplorationStore) SavePlannerState(p PlannerState) (bool, error) {
	var todos []struct {
		Content string `json:"content"`
		Status  string `json:"status"`
	}
	if len(p.Todos) == 0 {
		p.Todos = json.RawMessage(`[]`)
	}
	if len(p.Todos) > 128*1024 {
		return false, fmt.Errorf("规划待办超过128KiB")
	}
	if err := json.Unmarshal(p.Todos, &todos); err != nil {
		return false, err
	}
	if todos == nil {
		p.Todos = json.RawMessage(`[]`)
	}
	if len(todos) > 100 {
		return false, fmt.Errorf("规划待办超过100条")
	}
	for _, todo := range todos {
		if utf8.RuneCountInString(todo.Content) > 512 {
			return false, fmt.Errorf("单条规划待办超过512字")
		}
		if todo.Status != "pending" && todo.Status != "in_progress" && todo.Status != "completed" {
			return false, fmt.Errorf("无效规划待办状态")
		}
	}
	switch p.Outcome {
	case "running", "completed", "canceled", "error":
	default:
		return false, fmt.Errorf("无效规划交接状态")
	}
	summary := []rune(strings.ToValidUTF8(strings.ReplaceAll(p.Summary, "\x00", ""), ""))
	if len(summary) > 2000 {
		summary = summary[:2000]
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := s.lockPlannerTask(tx); err != nil {
		return false, err
	}
	res, err := tx.Exec(`UPDATE exploration_planner_state SET todos=$1,summary=$2,outcome=$3,updated_at=now() WHERE exploration_id=$4 AND revision=$5`, jsonbClean(p.Todos), string(summary), p.Outcome, s.expID, p.Revision)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, tx.Commit()
}
