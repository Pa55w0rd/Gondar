package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

const MaxIntentContinuationDepth = 3

// addContinuationIntent validates the live source state in the same transaction
// as the insert. Locking sources in ID order also serializes concurrent branches
// of one continuation, without treating unrelated directions as duplicates.
func (s *ExplorationStore) addContinuationIntent(payload map[string]any, priority int, anchors []int64, origin string) (int64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	var in struct {
		Continues []int64 `json:"continues"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return 0, fmt.Errorf("continues 必须为意图id数组: %w", err)
	}
	if len(in.Continues) == 0 {
		return s.AddNode(KindIntent, payload, priority, "open", origin, anchors)
	}
	if len(in.Continues) > 32 {
		return 0, fmt.Errorf("continues 最多包含32条已收场的意图")
	}
	sort.Slice(in.Continues, func(i, j int) bool { return in.Continues[i] < in.Continues[j] })
	ids := make([]int64, 0, len(in.Continues))
	for _, id := range in.Continues {
		if id <= 0 {
			return 0, fmt.Errorf("continues 包含无效意图id")
		}
		if len(ids) == 0 || ids[len(ids)-1] != id {
			ids = append(ids, id)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	depth := 1
	for _, cid := range ids {
		var sourceExp int64
		var kind, state string
		var prior json.RawMessage
		if err := tx.QueryRow(`SELECT exploration_id,kind,state,payload FROM exploration_nodes WHERE id=$1 FOR UPDATE`, cid).Scan(&sourceExp, &kind, &state, &prior); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("continues %d 不存在或已删除", cid)
			}
			return 0, fmt.Errorf("continues %d 状态校验失败: %w", cid, err)
		}
		if sourceExp != s.expID {
			var allowed bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks owner JOIN task_relations rel ON rel.task_id=owner.id JOIN tasks source ON source.id=rel.source_task_id WHERE owner.exploration_id=$1 AND source.exploration_id=$2 AND owner.deleted_at IS NULL AND source.deleted_at IS NULL)`, s.expID, sourceExp).Scan(&allowed); err != nil {
				return 0, err
			}
			if !allowed {
				return 0, fmt.Errorf("continues %d 不属于本任务或直接关联任务", cid)
			}
		}
		if kind != KindIntent || !inheritedIntentTerminal(state) {
			return 0, fmt.Errorf("continues %d 当前为 %s/%s：只能接续已收场的意图；未完成意图请等待、纠偏或恢复原意图", cid, kind, state)
		}
		var p struct {
			Depth int `json:"continuation_depth"`
		}
		if err := json.Unmarshal(prior, &p); err != nil {
			return 0, err
		}
		if p.Depth < 1 {
			p.Depth = 1
		}
		if p.Depth >= MaxIntentContinuationDepth {
			return 0, fmt.Errorf("接续代数已达上限（%d代），请换方向或恢复原意图", MaxIntentContinuationDepth)
		}
		if p.Depth+1 > depth {
			depth = p.Depth + 1
		}
		var active int64
		if err := tx.QueryRow(`SELECT COALESCE(MIN(id),0) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state IN ('open','running','paused') AND payload @> jsonb_build_object('continues',jsonb_build_array($2::bigint))`, s.expID, cid).Scan(&active); err != nil {
			return 0, err
		}
		if active > 0 {
			return 0, fmt.Errorf("continues %d 已有未完成的接续意图 #%d，请推进原接续方向", cid, active)
		}
	}
	// Ignore caller-supplied depth; authoritative source rows determine it.
	payloadCopy := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		payloadCopy[k] = v
	}
	payloadCopy["continues"], payloadCopy["continuation_depth"] = ids, depth
	raw, err = json.Marshal(payloadCopy)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow(`INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin) VALUES ($1,'intent',$2,$3,'open',$4) RETURNING id`, s.expID, string(raw), priority, origin).Scan(&id); err != nil {
		return 0, err
	}
	for _, a := range s.existingAssetIDs(tx, anchors) {
		if _, err := tx.Exec(`INSERT INTO exploration_anchors(node_id,asset_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, a); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}
