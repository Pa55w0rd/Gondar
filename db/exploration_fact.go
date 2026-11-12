package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
)

// RecordFact commits content, anchors and yield provenance together. Exact
// content retries on the same assets reuse the original fact, preserving evidence
// while keeping retries from satisfying progress guards.
func (s *ExplorationStore) RecordFact(payload map[string]any, priority int, origin string, anchors []int64, intentID int64) (int64, bool, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	var taskID int64
	if err := tx.QueryRow(`SELECT id FROM tasks WHERE exploration_id=$1`, s.expID).Scan(&taskID); errors.Is(err, sql.ErrNoRows) {
		// Standalone exploration stores are also used without a task. A deleted
		// or archived owner never enters this path: its task row is found above.
		if err := tx.QueryRow(`SELECT id FROM explorations WHERE id=$1 FOR UPDATE`, s.expID).Scan(&taskID); err != nil {
			return 0, false, err
		}
	} else if err != nil {
		return 0, false, err
	} else if err := lockTaskExtension(tx, taskID, s.expID); err != nil {
		return 0, false, err
	}
	if intentID > 0 {
		var owned bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='intent')`, intentID, s.expID).Scan(&owned); err != nil {
			return 0, false, err
		}
		if !owned {
			return 0, false, errors.New("事实来源意图必须属于当前任务")
		}
	}
	ids := s.existingAssetIDs(tx, anchors)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if len(unique) == 0 || unique[len(unique)-1] != id {
			unique = append(unique, id)
		}
	}
	anchorJSON, _ := json.Marshal(unique)
	var id int64
	err = tx.QueryRow(`SELECT n.id FROM exploration_nodes n WHERE n.exploration_id=$1 AND n.kind='fact' AND n.payload=$2::jsonb
AND COALESCE((SELECT jsonb_agg(a.asset_id ORDER BY a.asset_id) FROM exploration_anchors a WHERE a.node_id=n.id),'[]'::jsonb)=$3::jsonb ORDER BY n.id LIMIT 1`, s.expID, string(raw), string(anchorJSON)).Scan(&id)
	created := errors.Is(err, sql.ErrNoRows)
	if err != nil && !created {
		return 0, false, err
	}
	if created {
		if err := tx.QueryRow(`INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin) VALUES($1,'fact',$2,$3,'confirmed',$4) RETURNING id`, s.expID, string(raw), priority, origin).Scan(&id); err != nil {
			return 0, false, err
		}
		for _, assetID := range unique {
			if _, err := tx.Exec(`INSERT INTO exploration_anchors(node_id,asset_id) VALUES($1,$2)`, id, assetID); err != nil {
				return 0, false, err
			}
		}
	}
	if intentID > 0 {
		if _, err := tx.Exec(`INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) SELECT $1,$2,$3,$4 WHERE NOT EXISTS(SELECT 1 FROM exploration_edges WHERE exploration_id=$1 AND src_id=$2 AND rel=$3 AND dst_id=$4)`, s.expID, intentID, RelYields, id); err != nil {
			return 0, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return id, created, nil
}
