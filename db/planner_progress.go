package db

import (
	"context"
	"encoding/json"
)

// PlannerProgress describes committed task-local effects, never draft text.
type PlannerProgress struct {
	OpenGoals     int
	ActiveIntents int
	LatestIntent  int64
	LatestFinding int64
}

func (s *ExplorationStore) PlannerProgress(ctx context.Context) (PlannerProgress, error) {
	var p PlannerProgress
	err := s.db.QueryRowContext(ctx, `SELECT
count(*) FILTER (WHERE kind='goal' AND state='open'),
count(*) FILTER (WHERE kind='intent' AND state IN ('open','running')),
coalesce(max(id) FILTER (WHERE kind='intent' AND state<>'deleted'),0),
coalesce(max(id) FILTER (WHERE kind='finding'),0)
FROM exploration_nodes WHERE exploration_id=$1`, s.expID).
		Scan(&p.OpenGoals, &p.ActiveIntents, &p.LatestIntent, &p.LatestFinding)
	return p, err
}

func (p PlannerProgress) AdvancedSince(before PlannerProgress) bool {
	return p.OpenGoals < before.OpenGoals || p.ActiveIntents > 0 || p.LatestIntent > before.LatestIntent || p.LatestFinding > before.LatestFinding
}

func (d *DB) FailPlannerTask(taskID int64) (bool, error) {
	res, err := d.Exec(`UPDATE tasks SET status='failed',completed_at=COALESCE(completed_at,now())
WHERE id=$1 AND status NOT IN ('done','failed','timeout') AND NOT paused AND NOT queued AND deleted_at IS NULL`, taskID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Outcomes reuse task-owned activity so their failure streak survives restart
// and follows the existing archive/restore contract without another state table.
type PlannerOutcome struct {
	Version int    `json:"version"`
	Round   int64  `json:"round"`
	Code    string `json:"code"`
}

func (s *ExplorationStore) PlannerFailureStreak(ctx context.Context) (string, int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.metadata->'planner_outcome' FROM activity a
WHERE a.exploration_id=$1 AND a.worker='planner' AND a.kind='text' AND a.metadata ? 'planner_outcome'
AND a.created_at >= COALESCE((SELECT COALESCE(t.first_run_at,t.created_at) FROM tasks t WHERE t.exploration_id=$1 AND t.deleted_at IS NULL LIMIT 1),'-infinity'::timestamptz)
ORDER BY a.id DESC LIMIT 3`, s.expID)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	code, count := "", 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return "", 0, err
		}
		var outcome PlannerOutcome
		if json.Unmarshal(raw, &outcome) != nil || outcome.Version != 1 {
			break
		}
		switch outcome.Code {
		case "planner_model_unavailable", "planner_no_progress", "planner_tool_failure_loop", "planner_read_loop", "planner_round_timeout", "planner_response_interrupted":
		default:
			return code, count, rows.Err()
		}
		if count == 0 {
			code = outcome.Code
		} else if code != outcome.Code {
			code = "planner_repeated_failure"
		}
		count++
	}
	return code, count, rows.Err()
}
