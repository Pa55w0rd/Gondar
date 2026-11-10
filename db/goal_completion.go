package db

import (
	"context"
	"encoding/json"
	"fmt"
)

func (s *ExplorationStore) GoalCompletionCounts(ctx context.Context) (total, unmet int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE state<>'met') FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal'`, s.expID).Scan(&total, &unmet)
	return
}

// ProveGoal commits the evidence link and state together. A goal edited since
// the caller read it cannot be acknowledged against the previous definition.
func (s *ExplorationStore) ProveGoal(goalID, evidenceID int64, expected json.RawMessage, reason string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.lockPlannerTask(tx); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE exploration_nodes SET state='met',
payload=(payload-'unverified'-'unverified_reason') || jsonb_build_object('proof_reason',$4::text,'proof_evidence_id',$3::bigint),
content_version=content_version+1 WHERE exploration_id=$1 AND id=$2 AND kind='goal' AND payload=$5::jsonb`, s.expID, goalID, evidenceID, reason, string(expected))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("目标已变更或不存在，请重新读取当前目标后核对证据")
	}
	if _, err := tx.Exec(`INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) VALUES($1,$2,'proves',$3) ON CONFLICT(exploration_id,src_id,rel,dst_id) DO NOTHING`, s.expID, evidenceID, goalID); err != nil {
		return err
	}
	return tx.Commit()
}
