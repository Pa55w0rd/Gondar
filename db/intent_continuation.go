package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// 只限制没有新增证据的连续续派，不限制有实际进展的链深度。
const maxUnproductiveContinuations = 3
const continuationProgressHistory = 128

type continuationState struct {
	Depth          int               `json:"continuation_depth"`
	NoProgress     int               `json:"continuation_no_progress"`
	SeenProgress   []string          `json:"continuation_seen_progress"`
	SourceProgress map[string]string `json:"continuation_source_progress"`
}

// continuationProgress compares content, not node IDs or metadata timestamps.
// Re-registering an identical fact or artifact cannot restart a stalled chain.
func continuationProgress(tx *sql.Tx, sourceID int64) (string, error) {
	var facts string
	err := tx.QueryRow(`SELECT COALESCE(md5(string_agg(DISTINCT fingerprint,'' ORDER BY fingerprint)), '') FROM (
SELECT md5(CASE WHEN n.payload ? 'verification' THEN
COALESCE(n.payload#>>'{verification,id}','')||chr(10)||COALESCE(n.payload#>>'{verification,content_digest}','')
ELSE n.kind||chr(10)||COALESCE(n.payload->>'summary','')||chr(10)||COALESCE(n.payload->>'evidence','')||chr(10)||COALESCE(n.payload->>'detail','') END) AS fingerprint
FROM exploration_edges e JOIN exploration_nodes source ON source.id=e.src_id JOIN exploration_nodes n ON n.id=e.dst_id
WHERE e.src_id=$1 AND e.rel='yields' AND n.kind IN ('fact','finding') AND n.state<>'dismissed'
AND n.created_at>=source.created_at AND COALESCE(n.payload#>>'{verification,progress}','true')='true') evidence`, sourceID).Scan(&facts)
	if err != nil {
		return "", err
	}
	// The evidence table is optional in DB-only installations.
	var hasArtifacts bool
	if err := tx.QueryRow("SELECT to_regclass('task_artifacts') IS NOT NULL").Scan(&hasArtifacts); err != nil {
		return "", err
	}
	var artifacts string
	if hasArtifacts {
		if err := tx.QueryRow(`SELECT COALESCE(md5(string_agg(DISTINCT sha256,'' ORDER BY sha256)), '')
FROM task_artifacts WHERE intent_id=$1 AND COALESCE(sha256,'')<>''`, sourceID).Scan(&artifacts); err != nil {
			return "", err
		}
	}
	if facts == "" && artifacts == "" {
		return "", nil
	}
	hash := sha256.Sum256([]byte(facts + "|" + artifacts))
	return hex.EncodeToString(hash[:]), nil
}

// Sources are locked in ID order so concurrent branches from the same source
// cannot both pass the live-state and active-continuation checks.
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
	depth, noProgress := 1, 0
	anyNewProgress := false
	encodedSources, _ := json.Marshal(ids)
	seen := map[string]bool{}
	var history []string
	sourceProgress := make(map[string]string, len(ids))
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
		var p continuationState
		if err := json.Unmarshal(prior, &p); err != nil {
			return 0, err
		}
		if p.Depth < 1 {
			p.Depth = 1
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
		progress, err := continuationProgress(tx, cid)
		if err != nil {
			return 0, fmt.Errorf("continues %d 证据进展校验失败: %w", cid, err)
		}
		key := strconv.FormatInt(cid, 10)
		var previous json.RawMessage
		err = tx.QueryRow(`SELECT payload FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent'
AND state<>'deleted' AND payload @> jsonb_build_object('continues',jsonb_build_array($2::bigint))
AND payload->'continues'=$3::jsonb ORDER BY id DESC LIMIT 1`, s.expID, cid, string(encodedSources)).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if err == nil {
			var priorChild continuationState
			if err := json.Unmarshal(previous, &priorChild); err != nil {
				return 0, err
			}
			if priorChild.SourceProgress[key] == progress {
				return 0, fmt.Errorf("continues %d 已有接续且来源证据没有变化；请恢复原接续，或从最新已收场的意图继续，不要重放同一来源", cid)
			}
		}
		for _, hash := range p.SeenProgress {
			if !seen[hash] && hash != "" {
				seen[hash] = true
				history = append(history, hash)
			}
		}
		stalled := p.NoProgress + 1
		if progress != "" && !seen[progress] {
			anyNewProgress = true
			seen[progress] = true
			history = append(history, progress)
		}
		if stalled > noProgress {
			noProgress = stalled
		}
		sourceProgress[key] = progress
	}
	if anyNewProgress {
		noProgress = 0
	}
	if noProgress > maxUnproductiveContinuations {
		return 0, fmt.Errorf("该接续链已连续%d次没有新的证据内容；请先复核前置条件、恢复未完成步骤或改用有实质差异的方向", maxUnproductiveContinuations)
	}
	if len(history) > continuationProgressHistory {
		history = history[len(history)-continuationProgressHistory:]
	}
	// Ignore model-supplied bookkeeping; persisted source rows are authoritative.
	payloadCopy := make(map[string]any, len(payload)+4)
	for k, v := range payload {
		payloadCopy[k] = v
	}
	payloadCopy["continues"], payloadCopy["continuation_depth"] = ids, depth
	payloadCopy["continuation_no_progress"] = noProgress
	payloadCopy["continuation_seen_progress"] = history
	payloadCopy["continuation_source_progress"] = sourceProgress
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
