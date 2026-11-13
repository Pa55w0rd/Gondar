package db

import (
	"context"
	"encoding/json"
)

// PlannerProgress describes committed task-local effects, never draft text.
type PlannerProgress struct {
	OpenGoals        int
	ActiveIntents    int
	LatestIntent     int64
	LatestFinding    int64
	FactCount        int
	FactRevision     string
	ArtifactCount    int
	ArtifactRevision string
}

func (s *ExplorationStore) PlannerProgress(ctx context.Context) (PlannerProgress, error) {
	var p PlannerProgress
	err := s.db.QueryRowContext(ctx, `WITH latest_verification AS (
SELECT DISTINCT ON (payload#>>'{verification,id}') payload->'verification' AS v
FROM exploration_nodes WHERE exploration_id=$1 AND kind='fact' AND payload ? 'verification'
ORDER BY payload#>>'{verification,id}',id DESC
), facts AS (
SELECT DISTINCT 'fact:'||md5((payload - 'timestamp' - 'updated_at' - 'observed_at' - 'created_at' - 'last_seen_at')::text) AS fingerprint
FROM exploration_nodes WHERE exploration_id=$1 AND kind='fact' AND NOT (payload ? 'verification')
AND COALESCE(payload->>'confidence','observed')<>'inferred' AND btrim(COALESCE(payload->>'evidence',''))<>''
UNION
SELECT 'case:'||COALESCE(v->>'id','')||':'||COALESCE(v->>'content_digest','') FROM latest_verification
WHERE v->>'progress'='true'
) SELECT
	count(*) FILTER (WHERE kind='goal' AND (state='open' OR (state='met' AND NOT `+provenGoalSQL+`))),
count(*) FILTER (WHERE kind='intent' AND state IN ('open','running')),
coalesce(max(id) FILTER (WHERE kind='intent' AND state<>'deleted'),0),
coalesce(max(id) FILTER (WHERE kind='finding'),0),
(SELECT count(*) FROM facts),
(SELECT md5(coalesce(string_agg(fingerprint,',' ORDER BY fingerprint),'')) FROM facts)
FROM exploration_nodes WHERE exploration_id=$1`, s.expID).
		Scan(&p.OpenGoals, &p.ActiveIntents, &p.LatestIntent, &p.LatestFinding,
			&p.FactCount, &p.FactRevision)
	if err != nil {
		return p, err
	}
	// Attachment storage is initialized by the host; graph-only callers may not
	// have it. Do not require an optional host table to plan ordinary graph work.
	var hasArtifacts bool
	if err = s.db.QueryRowContext(ctx, `SELECT to_regclass('task_artifacts') IS NOT NULL`).Scan(&hasArtifacts); err != nil {
		return p, err
	}
	if hasArtifacts {
		err = s.db.QueryRowContext(ctx, `SELECT count(*),md5(coalesce(string_agg(fingerprint,',' ORDER BY fingerprint),'')) FROM (
SELECT DISTINCT CASE WHEN COALESCE(sha256,'')<>'' THEN sha256 ELSE 'id:'||id::text END AS fingerprint
FROM task_artifacts WHERE task_id=(SELECT id::text FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL LIMIT 1)
) a`, s.expID).Scan(&p.ArtifactCount, &p.ArtifactRevision)
	}
	return p, err
}

func (p PlannerProgress) AdvancedSince(before PlannerProgress) bool {
	return p.OpenGoals < before.OpenGoals || p.ActiveIntents > 0 || p.LatestIntent > before.LatestIntent || p.LatestFinding > before.LatestFinding || p.EvidenceAdvancedSince(before)
}

// EvidenceAdvancedSince counts committed content, not retries, clock updates or
// deletion. A changed verification result can advance an existing fact revision.
func (p PlannerProgress) EvidenceAdvancedSince(before PlannerProgress) bool {
	return p.FactCount > before.FactCount ||
		(p.FactCount > 0 && p.FactCount == before.FactCount && p.FactRevision != before.FactRevision) ||
		p.ArtifactCount > before.ArtifactCount ||
		(p.ArtifactCount > 0 && p.ArtifactCount == before.ArtifactCount && p.ArtifactRevision != before.ArtifactRevision)
}

func (d *DB) FailPlannerTask(taskID int64) (bool, error) {
	res, err := d.Exec(`UPDATE tasks SET status='failed',completed_at=COALESCE(completed_at,now())
WHERE id=$1 AND status NOT IN ('done','failed','timeout') AND NOT paused AND NOT queued AND deleted_at IS NULL`, taskID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Outcomes reuse task-owned activity so their failure streak survives restart
// and follows the existing archive/restore contract without another state table.
type PlannerOutcome struct {
	Version int    `json:"version"`
	Round   int64  `json:"round"`
	Code    string `json:"code"`
}

func (s *ExplorationStore) PlannerFailureStreak(ctx context.Context) (string, int, error) {
	codes, err := s.PlannerFailureSequence(ctx)
	code := ""
	for _, current := range codes {
		if code == "" {
			code = current
		} else if code != current {
			code = "planner_repeated_failure"
		}
	}
	return code, len(codes), err
}

// PlannerFailureSequence returns the current bounded failure streak in execution
// order, so a mixed failure retains its actual causes in the terminal event.
func (s *ExplorationStore) PlannerFailureSequence(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.metadata->'planner_outcome' FROM activity a
WHERE a.exploration_id=$1 AND a.worker='planner' AND a.kind='text' AND a.metadata ? 'planner_outcome'
AND a.created_at >= COALESCE((SELECT COALESCE(t.first_run_at,t.created_at) FROM tasks t WHERE t.exploration_id=$1 AND t.deleted_at IS NULL LIMIT 1),'-infinity'::timestamptz)
ORDER BY a.id DESC LIMIT 3`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var codes []string
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var outcome PlannerOutcome
		if json.Unmarshal(raw, &outcome) != nil || outcome.Version != 1 {
			break
		}
		switch outcome.Code {
		case "planner_model_unavailable", "planner_model_timeout", "planner_no_progress", "planner_tool_failure_loop", "planner_input_loop", "planner_policy_loop", "planner_prerequisite_loop", "planner_tool_outcome_unknown_loop", "planner_read_loop", "planner_round_timeout", "planner_response_interrupted":
		default:
			goto complete
		}
		codes = append(codes, outcome.Code)
	}
complete:
	for i, j := 0, len(codes)-1; i < j; i, j = i+1, j-1 {
		codes[i], codes[j] = codes[j], codes[i]
	}
	return codes, rows.Err()
}
