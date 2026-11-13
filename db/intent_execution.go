package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// IntentExecution is host-owned state. It travels with the intent in existing
// archives; model-supplied payload fields are replaced when the intent is claimed.
type IntentExecution struct {
	ID                string    `json:"id"`
	Phase             string    `json:"phase"`
	ClaimedFrom       string    `json:"claimed_from,omitempty"`
	PriorUncertainRun string    `json:"prior_uncertain_run_id,omitempty"`
	Uncertain         bool      `json:"uncertain,omitempty"`
	StartedAt         time.Time `json:"started_at"`
	FinishedAt        time.Time `json:"finished_at,omitempty"`
	State             string    `json:"state,omitempty"`
	Reason            string    `json:"reason,omitempty"`
	WallMS            int64     `json:"wall_ms,omitempty"`
}

func ExecutionFromNode(n *Node) IntentExecution {
	var value struct {
		Execution IntentExecution `json:"_execution"`
	}
	if n != nil {
		_ = json.Unmarshal(n.Payload, &value)
	}
	return value.Execution
}

// ClaimIntentExecution atomically claims a local open/paused intent and records
// its execution identity before any model or target operation can start.
func (s *ExplorationStore) ClaimIntentExecution(ctx context.Context, id int64, owner, expected string) (*Node, error) {
	if expected != "open" && expected != "paused" {
		return nil, ErrIntentStateConflict
	}
	run := IntentExecution{ID: uuid.NewString(), Phase: "claimed", ClaimedFrom: expected, StartedAt: time.Now().UTC()}
	raw, _ := json.Marshal(run)
	node, err := scanNode(s.db.QueryRowContext(ctx, `UPDATE exploration_nodes
SET state='running', owner=$1, completed_at=NULL, blocked_reason=NULL,
payload=jsonb_set(COALESCE(payload,'{}'::jsonb),'{_execution}',
 $2::jsonb || CASE WHEN payload#>>'{_execution,uncertain}'='true'
 OR payload#>>'{_execution,phase}' IN ('executing','unknown')
 OR COALESCE(blocked_reason,'') LIKE 'execution_result_unknown:%'
 THEN jsonb_build_object('prior_uncertain_run_id',COALESCE(payload#>>'{_execution,id}','legacy_unknown')) ELSE '{}'::jsonb END,true),
content_version=content_version+1
WHERE id=$3 AND exploration_id=$4 AND kind='intent' AND state=$5
RETURNING `+nodeCols, owner, string(raw), id, s.expID, expected))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrIntentStateConflict
	}
	if err != nil {
		// The UPDATE might have committed even if its receipt was lost. Return
		// only the host fence for cleanup; the caller must not execute this node.
		payload, _ := json.Marshal(map[string]any{"_execution": run})
		return &Node{ID: id, Kind: KindIntent, Payload: payload}, err
	}
	return node, err
}

func (s *ExplorationStore) AbortUnstartedClaim(ctx context.Context, id int64, runID string) error {
	return s.AbortUnstartedClaimToState(ctx, id, runID, "open")
}

func (s *ExplorationStore) AbortUnstartedClaimToState(ctx context.Context, id int64, runID, state string) error {
	if state != "open" && state != "paused" {
		return ErrIntentStateConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL FOR UPDATE`, s.expID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if IsTerminal(status) {
		state = "stopped"
	}
	_, err = tx.ExecContext(ctx, `UPDATE exploration_nodes SET state=$4,completed_at=CASE WHEN $4='stopped' THEN now() ELSE NULL END,
payload=jsonb_set(payload,'{_execution,phase}','"not_executed"'::jsonb),content_version=content_version+1
WHERE id=$1 AND exploration_id=$2 AND kind='intent' AND state='running'
AND payload->'_execution'->>'id'=$3 AND payload->'_execution'->>'phase'='claimed'`, id, s.expID, runID, state)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Compatibility for direct legacy callers that lack a host claim. Never touch
// a node which has since gained an execution identity, changed owner or payload.
func (s *ExplorationStore) AbortUnregisteredIntent(ctx context.Context, n *Node, state string) error {
	if n == nil || (state != "open" && state != "paused") {
		return ErrIntentStateConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL FOR UPDATE`, s.expID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if IsTerminal(status) {
		state = "stopped"
	}
	_, err = tx.ExecContext(ctx, `UPDATE exploration_nodes SET state=$1,completed_at=CASE WHEN $1='stopped' THEN now() ELSE NULL END,content_version=content_version+1
WHERE id=$2 AND exploration_id=$3 AND kind='intent' AND state='running' AND payload->'_execution' IS NULL
AND payload=$4::jsonb AND COALESCE(owner,'')=$5`, state, n.ID, s.expID, string(jsonbClean(n.Payload)), n.Owner)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// BeginIntentExecution also supports legacy/direct callers that already claimed
// an intent. Retrying the same identity is safe after an ambiguous DB receipt.
func (s *ExplorationStore) BeginIntentExecution(ctx context.Context, id int64, runID string) error {
	if _, err := uuid.Parse(runID); err != nil {
		return fmt.Errorf("invalid host execution identity: %w", err)
	}
	run := IntentExecution{ID: runID, Phase: "executing", StartedAt: time.Now().UTC()}
	raw, _ := json.Marshal(run)
	res, err := s.db.ExecContext(ctx, `UPDATE exploration_nodes
SET payload=CASE WHEN payload->'_execution'->>'id'=$1 THEN jsonb_set(payload,'{_execution,phase}','"executing"'::jsonb)
 ELSE jsonb_set(COALESCE(payload,'{}'::jsonb),'{_execution}',$2::jsonb,true) END
WHERE id=$3 AND exploration_id=$4 AND kind='intent' AND state='running'
AND (payload->'_execution' IS NULL OR (
 payload->'_execution'->>'id'=$1 AND payload->'_execution'->>'phase' IN ('claimed','executing')
))`, runID, string(raw), id, s.expID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrIntentStateConflict
	}
	return err
}

// SettleIntentExecution commits state, reason, telemetry and the host receipt
// together. A late execution cannot overwrite a newer run or human control.
// false,nil means that the current state/identity superseded this execution.
func (s *ExplorationStore) SettleIntentExecution(ctx context.Context, id int64, runID, state, reason string, wallMS int64) (bool, error) {
	switch state {
	case "done", "blocked", "exhausted", "stopped", "paused", "open":
	default:
		return false, fmt.Errorf("invalid execution settlement state %q", state)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var taskStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL FOR UPDATE`, s.expID).Scan(&taskStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	var current, payload string
	err = tx.QueryRowContext(ctx, `SELECT state,payload::text FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='intent' FOR UPDATE`, id, s.expID).Scan(&current, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	run := ExecutionFromNode(&Node{Payload: json.RawMessage(payload)})
	if run.ID != runID {
		return false, nil
	}
	if current != "running" {
		// An earlier successful commit may have lost its acknowledgement.
		return run.Phase == "settled" && run.State == current && run.State == state, nil
	}
	if IsTerminal(taskStatus) && state == "open" {
		state, reason = "stopped", IntentStopTaskDone
	}
	if wallMS < 0 {
		wallMS = 0
	}
	run.Phase, run.State, run.Reason, run.WallMS, run.FinishedAt = "settled", state, utf8Clean(reason), wallMS, time.Now().UTC()
	run.Uncertain = strings.HasPrefix(reason, "execution_result_unknown:")
	raw, _ := json.Marshal(run)
	fields, _ := json.Marshal(map[string]any{"_execution": run, "terminal": state, "wall_ms": wallMS})
	terminal := state == "done" || state == "blocked" || state == "exhausted" || state == "stopped"
	if _, err := tx.ExecContext(ctx, `UPDATE exploration_nodes SET state=$1,
blocked_reason=NULLIF($2,''),completed_at=CASE WHEN $3 THEN now() ELSE NULL END,
payload=COALESCE(payload,'{}'::jsonb)||$4::jsonb,content_version=content_version+1
WHERE id=$5 AND exploration_id=$6`, state, run.Reason, terminal, string(jsonbClean(fields)), id, s.expID); err != nil {
		return false, err
	}
	meta := `{"intent_execution":` + string(jsonbClean(raw)) + `}`
	if _, err := tx.ExecContext(ctx, `INSERT INTO activity(exploration_id,node_id,worker,kind,summary,metadata)
VALUES($1,$2,'system','text',$3,$4::jsonb)`, s.expID, id, "意图执行结果已保存："+state, meta); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
