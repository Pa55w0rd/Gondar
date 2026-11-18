package db

import "database/sql"

// LatestWorkerResult returns only this task's latest result for one intent.
// Looking it up directly also works after many later tool/chat/late-receipt rows.
func (s *ExplorationStore) LatestWorkerResult(intentID int64) (*Activity, error) {
	var row Activity
	err := s.db.QueryRow(`SELECT id,COALESCE(summary,''),metadata FROM activity
WHERE exploration_id=$1 AND node_id=$2 AND kind='result'
ORDER BY id DESC LIMIT 1`, s.expID, intentID).Scan(&row.ID, &row.Summary, &row.Metadata)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row.NodeID = &intentID
	return &row, nil
}
