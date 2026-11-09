package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrResourceConflict = errors.New("资源版本已变化，请刷新后重试")

func resourceBookkeepingTool(tool string) bool {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "", "register_resource", "update_resource", "list_resources", "propose_lesson", "recall_lessons", "save_evidence", "list_artifacts", "preview_evidence", "record_fact", "report_finding", "insert_assets", "list_assets", "graph_overview", "node_detail", "add_hint", "add_task_hint", "add_intent", "prove_goal", "goal_met":
		return true
	}
	return false
}

// TaskResource is an audit ledger, not a second session engine. Registering an
// item or recording a successful tool call never proves a goal/vulnerability.
type TaskResource struct {
	ID          int64     `json:"id"`
	TaskID      int64     `json:"task_id"`
	AssetID     *int64    `json:"asset_id,omitempty"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	State       string    `json:"state"`
	Revision    int64     `json:"revision"`
	Note        string    `json:"note"`
	Cleanup     string    `json:"cleanup"`
	EvidenceIDs []int64   `json:"evidence_ids"`
	Origin      string    `json:"origin"`
	ExpiresAt   time.Time `json:"expires_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const resourceCols = `id,task_id,asset_id,kind,name,state,revision,note,cleanup,evidence_ids,origin,expires_at,updated_at`

func scanResource(row interface{ Scan(...any) error }) (*TaskResource, error) {
	var r TaskResource
	var refs []byte
	if err := row.Scan(&r.ID, &r.TaskID, &r.AssetID, &r.Kind, &r.Name, &r.State, &r.Revision, &r.Note, &r.Cleanup, &refs, &r.Origin, &r.ExpiresAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(refs, &r.EvidenceIDs); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *ExplorationStore) RegisterTaskResource(taskID, assetID int64, kind, name, cleanup, origin string, hours int) (*TaskResource, error) {
	switch kind {
	case "local_file", "upload", "account", "session", "listener":
	default:
		return nil, errors.New("资源类型无效")
	}
	name = strings.TrimSpace(utf8Clean(name))
	cleanup = strings.TrimSpace(utf8Clean(cleanup))
	if name == "" || utf8.RuneCountInString(name) > 200 || cleanup == "" || utf8.RuneCountInString(cleanup) > 1200 {
		return nil, errors.New("资源需要200字以内名称与1200字以内清理计划；不要填写密码或令牌")
	}
	if hours < 1 || hours > 168 {
		return nil, errors.New("资源复核期限必须为1到168小时")
	}
	if kind != "local_file" && assetID <= 0 {
		return nil, errors.New("目标资源必须关联当前任务的明确资产ID")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var alive int64
	if err := lockTaskExtension(tx, taskID, s.expID); err != nil {
		return nil, err
	}
	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM task_resources WHERE task_id=$1`, taskID).Scan(&count); err != nil {
		return nil, err
	}
	if count >= 300 {
		return nil, errors.New("每任务最多登记300项资源")
	}
	var asset any
	if assetID > 0 {
		if err := tx.QueryRow(`SELECT id FROM assets WHERE id=$1 AND $2=ANY(task_ids) FOR SHARE`, assetID, taskID).Scan(&alive); err != nil {
			return nil, errors.New("资产未明确关联当前任务，不能按同根域名或URL子串认领")
		}
		asset = assetID
	}
	r, err := scanResource(tx.QueryRow(`INSERT INTO task_resources(task_id,asset_id,kind,name,cleanup,origin,expires_at)
VALUES($1,$2,$3,$4,$5,$6,now()+make_interval(hours=>$7))
ON CONFLICT(task_id,kind,name) DO UPDATE SET name=EXCLUDED.name WHERE task_resources.asset_id IS NOT DISTINCT FROM EXCLUDED.asset_id RETURNING `+resourceCols, taskID, asset, kind, name, cleanup, utf8Clean(origin), hours))
	if err == sql.ErrNoRows {
		return nil, errors.New("同名资源已关联其他资产，请使用明确的新名称")
	}
	if err != nil {
		return nil, err
	}
	return r, tx.Commit()
}

func validResourceTransition(from, to string) bool {
	switch from {
	case "planned":
		return to == "created" || to == "failed" || to == "cleanup_pending"
	case "created":
		return to == "active" || to == "failed" || to == "cleanup_pending"
	case "active":
		return to == "failed" || to == "cleanup_pending"
	case "failed":
		return to == "cleanup_pending"
	case "cleanup_pending":
		return to == "cleaned" || to == "failed"
	}
	return false
}

func (s *ExplorationStore) UpdateTaskResource(taskID, id, revision int64, state, note string, evidenceIDs []int64) (*TaskResource, error) {
	if utf8.RuneCountInString(note) > 1200 || strings.TrimSpace(note) == "" {
		return nil, errors.New("资源状态需要1200字以内的实际结果说明")
	}
	if len(evidenceIDs) > 8 {
		return nil, errors.New("最多引用8条工具结果")
	}
	needsProof := state == "created" || state == "active" || state == "cleaned"
	if needsProof && len(evidenceIDs) == 0 {
		return nil, errors.New("创建、活跃或已清理状态必须引用实际成功的工具结果；登记不等于成功")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := lockTaskExtension(tx, taskID, s.expID); err != nil {
		return nil, err
	}
	r, err := scanResource(tx.QueryRow(`SELECT `+resourceCols+` FROM task_resources WHERE id=$1 AND task_id=$2 FOR UPDATE`, id, taskID))
	if err == sql.ErrNoRows {
		return nil, errors.New("资源不存在")
	}
	if err != nil {
		return nil, err
	}
	if r.Revision != revision {
		return nil, ErrResourceConflict
	}
	if !validResourceTransition(r.State, state) {
		return nil, fmt.Errorf("不能从%s直接变更为%s", r.State, state)
	}
	seen := map[int64]bool{}
	for _, aid := range evidenceIDs {
		if aid <= 0 || seen[aid] {
			return nil, errors.New("工具结果编号无效或重复")
		}
		seen[aid] = true
		var failed bool
		var text, tool string
		if err := tx.QueryRow(`SELECT is_error,COALESCE(NULLIF(detail,''),summary),COALESCE(tool,'') FROM activity WHERE id=$1 AND exploration_id=$2 AND kind='tool_result' FOR SHARE`, aid, s.expID).Scan(&failed, &text, &tool); err != nil {
			return nil, errors.New("引用不是当前任务的真实工具结果")
		}
		if needsProof && (failed || strings.TrimSpace(text) == "") {
			return nil, errors.New("失败或空工具结果不能确认资源成功状态")
		}
		if needsProof && resourceBookkeepingTool(tool) {
			return nil, errors.New("登记、台账或查询回执不能证明资源创建、活跃或清理成功，请引用实际操作与验证结果")
		}
	}
	if evidenceIDs == nil {
		evidenceIDs = []int64{}
	}
	refs, _ := json.Marshal(evidenceIDs)
	if _, err := tx.Exec(`INSERT INTO task_resource_events(resource_id,revision,old_state,new_state,note,evidence_ids) VALUES($1,$2,$3,$4,$5,$6)`, id, revision+1, r.State, state, utf8Clean(note), string(refs)); err != nil {
		return nil, err
	}
	r, err = scanResource(tx.QueryRow(`UPDATE task_resources SET state=$3,note=$4,evidence_ids=$5,revision=revision+1,updated_at=now() WHERE id=$1 AND task_id=$2 RETURNING `+resourceCols, id, taskID, state, utf8Clean(note), string(refs)))
	if err != nil {
		return nil, err
	}
	return r, tx.Commit()
}

func (d *DB) ListTaskResources(taskID int64) ([]*TaskResource, error) {
	rows, err := d.Query(`SELECT `+resourceCols+` FROM task_resources WHERE task_id=$1 ORDER BY (state!='cleaned') DESC,expires_at,id LIMIT 300`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TaskResource{}
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *ExplorationStore) TaskResources(taskID int64) ([]*TaskResource, error) {
	var alive bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND exploration_id=$2 AND deleted_at IS NULL)`, taskID, s.expID).Scan(&alive); err != nil {
		return nil, err
	}
	if !alive {
		return nil, errors.New("资源查询需要当前有效任务")
	}
	return s.db.ListTaskResources(taskID)
}
