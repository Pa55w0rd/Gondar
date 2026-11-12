package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

func decodeVerification(nodeID int64, raw []byte, created time.Time, state ...string) (*VerificationRecord, error) {
	var payload struct {
		Verification *VerificationRecord `json:"verification"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Verification == nil || payload.Verification.SchemaVersion != 1 || payload.Verification.Revision < 1 || payload.Verification.ID == "" {
		return nil, fmt.Errorf("旧验证记录格式异常，未覆盖原证据")
	}
	payload.Verification.NodeID, payload.Verification.CreatedAt = nodeID, created
	if len(state) > 0 {
		payload.Verification.NodeState = state[0]
	}
	return payload.Verification, nil
}

// UpsertVerification serializes writers on the task/exploration, compares the
// semantic digest before the CAS, and appends a fact only for a real change.
// A retry of a committed revision remains idempotent even with a stale CAS.
func (s *ExplorationStore) UpsertVerification(ctx context.Context, input VerificationUpdate, origin string) (*VerificationWrite, error) {
	in, stableID, digest, err := normalizeVerificationUpdate(input)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.lockPlannerTask(tx); err != nil {
		return nil, err
	}
	var expID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM explorations WHERE id=$1 FOR UPDATE`, s.expID).Scan(&expID); err != nil {
		return nil, err
	}
	if in.IntentID > 0 {
		var valid bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='intent')`, in.IntentID, s.expID).Scan(&valid); err != nil {
			return nil, err
		}
		if !valid {
			return nil, fmt.Errorf("intent_id必须是本任务的意图，关联任务仅供读取")
		}
	}
	if err := s.validateVerificationEvidence(ctx, tx, in); err != nil {
		return nil, err
	}
	var nodeID int64
	var raw []byte
	var created time.Time
	var nodeState string
	err = tx.QueryRowContext(ctx, `SELECT id,payload,created_at,state FROM exploration_nodes
		WHERE exploration_id=$1 AND kind='fact' AND payload->'verification'->>'id'=$2
		ORDER BY id DESC LIMIT 1`, s.expID, stableID).Scan(&nodeID, &raw, &created, &nodeState)
	var previous *VerificationRecord
	if err == nil {
		previous, err = decodeVerification(nodeID, raw, created, nodeState)
	} else if err == sql.ErrNoRows {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if previous != nil && previous.ContentDigest == digest {
		if previous.NodeState == "dismissed" {
			return nil, fmt.Errorf("该验证结论已撤销；不能重放旧内容恢复，须先读取历史并提交新观察与修改原因")
		}
		return &VerificationWrite{Changed: false, Record: previous}, tx.Commit()
	}
	if previous == nil {
		if in.ExpectedRevision != nil && *in.ExpectedRevision != 0 {
			return nil, ErrVerificationRevisionConflict
		}
	} else {
		if in.ExpectedRevision == nil || *in.ExpectedRevision != previous.Revision {
			return nil, ErrVerificationRevisionConflict
		}
		if in.ChangeReason == "" && (previous.State == VerificationConfirmed || previous.State == VerificationNegative || previous.NodeState == "dismissed") {
			return nil, fmt.Errorf("修改已有证实/阴性结论必须说明change_reason，旧修订将继续保留")
		}
	}
	record := &VerificationRecord{
		SchemaVersion: 1, ID: stableID, Revision: 1, ContentDigest: digest,
		Target: in.Target, State: in.State, Summary: in.Summary, Observation: in.Observation,
		Evidence: in.Evidence, PrerequisitesMet: in.PrerequisitesMet, Blocker: in.Blocker,
		MissingPrerequisites: in.MissingPrerequisites, IntentID: in.IntentID,
		ChangeReason: in.ChangeReason, ObservedAt: in.ObservedAt,
		Progress: in.State != VerificationNotTested,
	}
	if previous != nil {
		record.Revision, record.PreviousNodeID = previous.Revision+1, previous.NodeID
	}
	confidence := "observed"
	if len(in.Evidence) == 0 {
		confidence = "inferred"
	}
	payload, err := json.Marshal(map[string]any{
		"summary": "验证记账：" + in.Summary, "detail": in.Observation,
		"confidence": confidence, "verification": record,
	})
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin)
		VALUES($1,'fact',$2,5,'confirmed',$3) RETURNING id,created_at`, s.expID, string(payload), utf8Clean(origin)).Scan(&record.NodeID, &record.CreatedAt); err != nil {
		return nil, err
	}
	if in.IntentID > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id)
			VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, s.expID, in.IntentID, RelYields, record.NodeID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	record.NodeState = "confirmed"
	return &VerificationWrite{Changed: true, Record: record}, nil
}

func (s *ExplorationStore) validateVerificationEvidence(ctx context.Context, tx *sql.Tx, in VerificationUpdate) error {
	proof := in.State == VerificationNegative || in.State == VerificationConfirmed
	for _, item := range in.Evidence {
		var valid bool
		var err error
		switch {
		case item.ActivityID > 0:
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM activity WHERE id=$1 AND exploration_id=$2 AND kind IN ('tool_result','result') AND (NOT $3 OR NOT is_error) AND (COALESCE(summary,'')<>'' OR COALESCE(detail,'')<>''))`, item.ActivityID, s.expID, proof).Scan(&valid)
		case item.NodeID > 0:
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='fact' AND state='confirmed' AND payload->'verification' IS NULL AND (NOT $3 OR COALESCE(payload->>'confidence','')<>'inferred') AND COALESCE(payload->>'evidence','')<>'')`, item.NodeID, s.expID, proof).Scan(&valid)
		case item.ArtifactID > 0:
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_artifacts artifact JOIN tasks task ON artifact.task_id=task.id::text WHERE artifact.id=$1 AND task.exploration_id=$2 AND task.deleted_at IS NULL)`, item.ArtifactID, s.expID).Scan(&valid)
		}
		if err != nil {
			return err
		}
		if !valid {
			return fmt.Errorf("验证证据必须是本任务已保存的工具结果、带证据事实或附件；不能引用关联任务或验证结论自证，执行错误/推断不能作为阴性或影响证明")
		}
	}
	return nil
}

const verificationLatestSQL = `(SELECT DISTINCT ON (payload->'verification'->>'id') id,payload,created_at,state AS node_state
	FROM exploration_nodes WHERE exploration_id=$1 AND kind='fact' AND payload->'verification'->>'id' IS NOT NULL
	ORDER BY payload->'verification'->>'id',id DESC) latest`

// ListVerifications counts only the latest revision per recorded case. The
// denominator is this ledger, never all endpoints or all possible techniques.
func (s *ExplorationStore) ListVerifications(ctx context.Context, filter VerificationFilter) (*VerificationPage, error) {
	if filter.State != "" && !validVerificationState(filter.State) {
		return nil, fmt.Errorf("验证状态无效")
	}
	if filter.IncludeHistory && filter.ID == "" {
		return nil, fmt.Errorf("读取历史必须指定验证id")
	}
	if filter.Limit <= 0 {
		filter.Limit = 20
	}
	if filter.Limit > 100 {
		filter.Limit = 100
	}
	page := &VerificationPage{Cases: []*VerificationRecord{}, LatestStates: map[string]int{}, Scope: "recorded_cases_only; unrecorded_cases_unknown"}
	condition := ` WHERE ($2='' OR payload->'verification'->>'id'=$2) AND ($3='' OR payload->'verification'->>'state'=$3) AND ($4='' OR payload->'verification'->'target'->>'endpoint'=$4)`
	latestCondition := condition + ` AND node_state<>'dismissed'`
	args := []any{s.expID, filter.ID, filter.State, filter.Endpoint}
	rows, err := s.db.QueryContext(ctx, `SELECT payload->'verification'->>'state',count(*) FROM `+verificationLatestSQL+latestCondition+` GROUP BY 1`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return nil, err
		}
		page.LatestStates[state], page.RecordedCases = count, page.RecordedCases+count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	from := verificationLatestSQL
	rowCondition := latestCondition
	if filter.IncludeHistory {
		from = `(SELECT id,payload,created_at,state AS node_state FROM exploration_nodes WHERE exploration_id=$1 AND kind='fact' AND payload->'verification'->>'id' IS NOT NULL) history`
		rowCondition = condition
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+from+rowCondition, args...).Scan(&page.Total); err != nil {
		return nil, err
	}
	args = append(args, filter.Before, filter.Limit+1)
	rows, err = s.db.QueryContext(ctx, `SELECT id,payload,created_at,node_state FROM `+from+rowCondition+` AND ($5<=0 OR id<$5) ORDER BY id DESC LIMIT $6`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw []byte
		var created time.Time
		var state string
		if err := rows.Scan(&id, &raw, &created, &state); err != nil {
			return nil, err
		}
		record, err := decodeVerification(id, raw, created, state)
		if err != nil {
			return nil, err
		}
		page.Cases = append(page.Cases, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Cases) > filter.Limit {
		page.Cases, page.HasMore = page.Cases[:filter.Limit], true
	}
	if page.HasMore {
		page.NextBefore = page.Cases[len(page.Cases)-1].NodeID
	}
	return page, nil
}
