package db

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Serialize with intent writers. Do not pause a task if new executable work was
// admitted after the planner decided to wait, or override a lifecycle change.
func (d *DB) PauseTaskForPlannerWait(ctx context.Context, id int64, events ...*Activity) (bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var expID int64
	if err = tx.QueryRowContext(ctx, `SELECT exploration_id FROM tasks WHERE id=$1 FOR UPDATE`, id).Scan(&expID); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET paused=true,completed_at=NULL WHERE id=$1 AND status NOT IN ('done','failed','timeout') AND NOT paused AND NOT queued AND deleted_at IS NULL AND archived_at IS NULL AND NOT EXISTS(SELECT 1 FROM exploration_nodes WHERE exploration_id=$2 AND kind='intent' AND state IN ('open','running'))`, id, expID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 && len(events) > 0 && events[0] != nil {
		event := events[0]
		err = tx.QueryRowContext(ctx, `INSERT INTO activity(exploration_id,worker,kind,summary,detail,metadata) VALUES($1,'system','text',$2,$3,$4) RETURNING id,created_at`, expID, utf8Clean(event.Summary), utf8Clean(event.Detail), event.Metadata).Scan(&event.ID, &event.CreatedAt)
		if err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *ExplorationStore) LatestPlannerWait() (json.RawMessage, error) {
	var raw []byte
	err := s.db.QueryRow(`SELECT metadata->'planner_wait' FROM activity WHERE exploration_id=$1 AND worker='system' AND kind='text' AND (metadata ? 'planner_wait' OR metadata ? 'human_resume' OR metadata ? 'human_continue' OR metadata ? 'continuous_pause' OR metadata ? 'runtime_failure') ORDER BY id DESC LIMIT 1`, s.expID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return raw, err
}
