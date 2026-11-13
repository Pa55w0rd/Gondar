package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// DiscoverGoal only appends task-local hypotheses. It does not replace goals,
// mark them proved, modify scope, or claim progress on the underlying work.
func (s *ExplorationStore) DiscoverGoal(ctx context.Context, text string, parents []int64) (id int64, created bool, err error) {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > 512 || len(parents) == 0 || len(parents) > 8 {
		return 0, false, fmt.Errorf("需1至512字的可验证目标及1至8个本任务事实/发现依据")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	if err = s.lockPlannerTask(tx); err != nil {
		return 0, false, err
	}
	for _, parent := range parents {
		var exists bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind IN ('fact','finding'))`, parent, s.expID).Scan(&exists)
		if err != nil {
			return 0, false, err
		}
		if !exists {
			return 0, false, fmt.Errorf("依据%d不是本任务已保存的事实或发现", parent)
		}
	}
	key := strings.ToLower(strings.Join(strings.Fields(text), " "))
	err = tx.QueryRowContext(ctx, `SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal' AND lower(regexp_replace(btrim(payload->>'text'),'[[:space:]]+',' ','g'))=$2 ORDER BY id LIMIT 1`, s.expID, key).Scan(&id)
	if err == nil {
		return id, false, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}
	raw, _ := json.Marshal(map[string]any{"text": text, "discovery_parent_ids": parents, "discovery": true})
	err = tx.QueryRowContext(ctx, `INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin) VALUES($1,'goal',$2,0,'open','planner') RETURNING id`, s.expID, string(raw)).Scan(&id)
	if err != nil {
		return 0, false, err
	}
	for _, parent := range parents {
		if _, err = tx.ExecContext(ctx, `INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, s.expID, parent, RelSpawns, id); err != nil {
			return 0, false, err
		}
	}
	return id, true, tx.Commit()
}
