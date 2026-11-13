package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const MaxIntentRetireBatch = 6

var ErrIntentRetirementInvalid = errors.New("意图退役参数或依据无效")

type IntentRetirement struct {
	IntentID     int64  `json:"intent_id"`
	Reason       string `json:"reason"`
	EvidenceID   int64  `json:"evidence_id,omitempty"`
	SupersededBy int64  `json:"superseded_by,omitempty"`
}

type IntentBacklog struct {
	Open    int `json:"open"`
	Running int `json:"running"`
}

func (s *ExplorationStore) IntentBacklog(ctx context.Context) (IntentBacklog, error) {
	var out IntentBacklog
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE state='open'), count(*) FILTER(WHERE state='running')
FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent'`, s.expID).Scan(&out.Open, &out.Running)
	return out, err
}

// RetireOpenIntents never kills workers or removes evidence, lineage or side
// conversations. Age alone is not a reason; the caller must identify a local
// fact/finding or a live replacement and explain why this pending work is stale.
func (s *ExplorationStore) RetireOpenIntents(ctx context.Context, items []IntentRetirement) ([]int64, error) {
	if len(items) == 0 || len(items) > MaxIntentRetireBatch {
		return nil, fmt.Errorf("%w: 一次只可退役1至%d个未执行意图", ErrIntentRetirementInvalid, MaxIntentRetireBatch)
	}
	ids := map[int64]bool{}
	for i := range items {
		item := &items[i]
		item.Reason = strings.TrimSpace(item.Reason)
		if item.IntentID <= 0 || ids[item.IntentID] || item.Reason == "" || len([]rune(item.Reason)) > 1000 {
			return nil, fmt.Errorf("%w: 退役意图、去重或原因无效", ErrIntentRetirementInvalid)
		}
		if (item.EvidenceID > 0) == (item.SupersededBy > 0) || item.SupersededBy == item.IntentID {
			return nil, fmt.Errorf("%w: 必须且只能指定一个本任务事实/发现或存活替代意图", ErrIntentRetirementInvalid)
		}
		ids[item.IntentID] = true
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var taskID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL FOR UPDATE`, s.expID).Scan(&taskID); err != nil {
		return nil, err
	}
	// Lock referenced nodes in one stable order, including replacement and proof.
	locks := map[int64]bool{}
	for _, item := range items {
		locks[item.IntentID] = true
		if item.EvidenceID > 0 {
			locks[item.EvidenceID] = true
		}
		if item.SupersededBy > 0 {
			locks[item.SupersededBy] = true
		}
		if ids[item.SupersededBy] {
			return nil, fmt.Errorf("%w: 替代意图不能同时被退役", ErrIntentRetirementInvalid)
		}
	}
	ordered := make([]int64, 0, len(locks))
	for id := range locks {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	nodes := map[int64]*Node{}
	for _, id := range ordered {
		n, err := scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM exploration_nodes WHERE exploration_id=$1 AND id=$2 FOR UPDATE`, s.expID, id))
		if err != nil {
			return nil, fmt.Errorf("读取本任务退役依据 #%d：%w", id, err)
		}
		nodes[id] = n
	}
	retired := []int64{}
	for _, item := range items {
		n := nodes[item.IntentID]
		if n.Kind != KindIntent || n.State != "open" {
			return nil, fmt.Errorf("%w: 意图 #%d 已不在待领取状态", ErrIntentStateConflict, item.IntentID)
		}
		if item.EvidenceID > 0 {
			proof := nodes[item.EvidenceID]
			if (proof.Kind != KindFact && proof.Kind != KindFinding) || proof.State == "deleted" {
				return nil, fmt.Errorf("%w: 退役依据必须为本任务现存事实或发现", ErrIntentRetirementInvalid)
			}
		} else {
			replacement := nodes[item.SupersededBy]
			if replacement.Kind != KindIntent || (replacement.State != "open" && replacement.State != "running" && replacement.State != "paused") {
				return nil, fmt.Errorf("%w: 替代意图必须仍然存活", ErrIntentRetirementInvalid)
			}
		}
		meta, _ := json.Marshal(map[string]any{"retirement": map[string]any{"reason": item.Reason, "evidence_id": item.EvidenceID, "superseded_by": item.SupersededBy, "retired_at": time.Now().UTC(), "actor": "planner"}})
		if _, err := tx.ExecContext(ctx, `UPDATE exploration_nodes SET state='deleted',delete_reason=$1,completed_at=now(),content_version=content_version+1,
payload=COALESCE(payload,'{}'::jsonb)||$2::jsonb WHERE id=$3 AND exploration_id=$4`, utf8Clean(item.Reason), string(jsonbClean(meta)), item.IntentID, s.expID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO activity(exploration_id,node_id,worker,kind,summary,metadata) VALUES($1,$2,'planner','text',$3,$4::jsonb)`, s.expID, item.IntentID, "待执行意图有据退役："+item.Reason, string(jsonbClean(meta))); err != nil {
			return nil, err
		}
		retired = append(retired, item.IntentID)
	}
	return retired, tx.Commit()
}
