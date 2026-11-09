package db

import (
	"database/sql"
	"errors"
)

// SettleInterruptedIntent returns an interrupted running worker to the frontier.
// Task-row locking orders this decision against terminal lifecycle updates; a
// late shutdown must not revive an already completed task's work.
func (s *ExplorationStore) SettleInterruptedIntent(id int64) (string, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var status string
	err = tx.QueryRow(`SELECT status FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL FOR UPDATE`, s.expID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	state := "open"
	if IsTerminal(status) {
		state = "stopped"
	}
	res, err := tx.Exec(`UPDATE exploration_nodes SET state=$1,completed_at=CASE WHEN $1='stopped' THEN now() ELSE NULL END,blocked_reason=NULL,content_version=content_version+1 WHERE id=$2 AND exploration_id=$3 AND kind='intent' AND state='running'`, state, id, s.expID)
	if err != nil {
		return "", false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", false, err
	}
	return state, n == 1, tx.Commit()
}
