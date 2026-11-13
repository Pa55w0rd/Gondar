package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

var ErrFindingReportSource = errors.New("报告来源身份与当前漏洞不匹配")

// FindingReportSource is a host-authored receipt for this exact report body.
// Numeric identities serialize as strings so browser roundoff cannot relink it.
// It is not a claim that the report's impact has been independently verified.
type FindingReportSource struct {
	Version               int    `json:"version"`
	Kind                  string `json:"kind"`
	FindingID             int64  `json:"finding_id,string"`
	NodeID                int64  `json:"finding_node_id,string"`
	TaskID                int64  `json:"task_id,string"`
	EvidenceVersion       int64  `json:"evidence_version"`
	ReportSHA256          string `json:"report_sha256"`
	ConversationID        int64  `json:"conversation_id,string,omitempty"`
	ConversationCreatedAt string `json:"conversation_created_at,omitempty"`
	ConversationAgentKey  string `json:"conversation_agent_key,omitempty"`
	RunID                 string `json:"run_id,omitempty"`
	PromptID              string `json:"prompt_id,omitempty"`
	InvocationID          string `json:"invocation_id,omitempty"`
	ToolUseID             string `json:"tool_use_id,omitempty"`
	RecoveryID            string `json:"recovery_id,omitempty"`
	RecoveryAttempt       int    `json:"recovery_attempt,omitempty"`
	SessionID             string `json:"session_id,omitempty"`
	RecordedAt            string `json:"recorded_at"`
	Archived              bool   `json:"archived,omitempty"`
	Available             bool   `json:"available"`
	UnavailableReason     string `json:"unavailable_reason,omitempty"`
}

func reportBodySHA256(report string) string {
	sum := sha256.Sum256([]byte(report))
	return hex.EncodeToString(sum[:])
}

func FindingReportRecoverySourceID(job *FindingReportRecovery) string {
	if job == nil {
		return ""
	}
	return reportBodySHA256(fmt.Sprintf("%d:%d:%d:%s", job.TaskID, job.FindingID, job.Attempt, job.Token))
}

func recoveryReportSource(job *FindingReportRecovery, report string, now time.Time) *FindingReportSource {
	return &FindingReportSource{Version: 1, Kind: "recovery_job", FindingID: job.FindingID, NodeID: job.NodeID, TaskID: job.TaskID, EvidenceVersion: job.EvidenceVersion, ReportSHA256: reportBodySHA256(report), RecoveryID: FindingReportRecoverySourceID(job), RecoveryAttempt: job.Attempt, SessionID: fmt.Sprintf("exp%d-report-%d-%d", job.ExplorationID, job.FindingID, job.Attempt), RecordedAt: now.UTC().Format(time.RFC3339Nano)}
}

// SetFindingReportWithSourceByNodeID keeps the established node-id API distinct
// from the standalone finding PK. A missing source clears the previous binding;
// it never borrows an earlier writer's receipt for a newly written report.
func (d *DB) SetFindingReportWithSourceByNodeID(ctx context.Context, nodeID int64, report string, version *int64, input *FindingReportSource) (n int64, err error) {
	report = utf8Clean(report)
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var id, taskID, actualNodeID, ev int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM findings WHERE node_id=$1`, nodeID).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if err := LockFindingEvidenceTx(tx, id, version); err != nil {
			return err
		}
		// Read the version under the same evidence lock used by every writer.
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(task_id,0),COALESCE(node_id,0),evidence_version FROM findings WHERE id=$1`, id).Scan(&taskID, &actualNodeID, &ev); err != nil {
			return err
		}
		if actualNodeID != nodeID {
			return ErrFindingReportSource
		}
		reportVersion := int64(0)
		if version != nil {
			reportVersion = *version
		} else if ev != 0 {
			reportVersion = -1
		}
		var source *FindingReportSource
		if input != nil {
			if input.Kind != "conversation" || input.ConversationID <= 0 || input.RunID == "" || input.PromptID == "" || input.InvocationID == "" || input.ToolUseID == "" ||
				input.FindingID != 0 && input.FindingID != id || input.NodeID != 0 && input.NodeID != nodeID || input.TaskID != 0 && input.TaskID != taskID {
				return ErrFindingReportSource
			}
			copy := *input
			source = &copy
			var created time.Time
			if err := tx.QueryRowContext(ctx, `SELECT created_at,agent_key FROM conversations WHERE id=$1 FOR SHARE`, source.ConversationID).Scan(&created, &source.ConversationAgentKey); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrFindingReportSource
				}
				return err
			}
			source.Version, source.FindingID, source.NodeID, source.TaskID = 1, id, nodeID, taskID
			source.EvidenceVersion, source.ReportSHA256 = reportVersion, reportBodySHA256(report)
			source.ConversationCreatedAt = created.UTC().Format(time.RFC3339Nano)
			source.RecordedAt = time.Now().UTC().Format(time.RFC3339Nano)
			source.SessionID = "conv-" + strconv.FormatInt(source.ConversationID, 10)
			source.Archived, source.Available, source.UnavailableReason = false, false, ""
			source.RecoveryID, source.RecoveryAttempt = "", 0
		}
		if err := storeFindingReportSourceTx(ctx, tx, id, source); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE findings SET report=$2,report_evidence_version=$3 WHERE id=$1`, id, report, reportVersion)
		if err != nil {
			return err
		}
		n, err = result.RowsAffected()
		return err
	})
	return
}

func storeFindingReportSourceTx(ctx context.Context, tx *sql.Tx, id int64, source *FindingReportSource) error {
	var previous []byte
	if err := tx.QueryRowContext(ctx, `SELECT report_source FROM findings WHERE id=$1 FOR UPDATE`, id).Scan(&previous); err != nil {
		return err
	}
	if len(previous) > 0 && string(previous) != "null" {
		metadata, err := json.Marshal(map[string]any{"finding_id": id, "previous_report_source": json.RawMessage(previous), "activity_origin": "runtime"})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO activity(exploration_id,node_id,worker,kind,summary,metadata)
SELECT t.exploration_id,f.node_id,'reporter','report_source','报告来源已变更；旧来源保留为历史记录',$2
FROM findings f JOIN tasks t ON t.id=f.task_id WHERE f.id=$1`, id, metadata); err != nil {
			return err
		}
	}
	var raw any
	if source != nil {
		encoded, err := json.Marshal(source)
		if err != nil {
			return err
		}
		raw = string(encoded)
	}
	_, err := tx.ExecContext(ctx, `UPDATE findings SET report_source=$2::jsonb WHERE id=$1`, id, raw)
	return err
}

// Reads never backfill by title, prose, ILIKE or a provider-supplied tool ID.
func (d *DB) currentFindingReportSource(f *DBFinding) (*FindingReportSource, error) {
	var raw []byte
	if err := d.QueryRow(`SELECT report_source FROM findings WHERE id=$1`, f.ID).Scan(&raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var source FindingReportSource
	if json.Unmarshal(raw, &source) != nil || source.Version != 1 || source.FindingID != f.ID || source.ReportSHA256 != reportBodySHA256(f.Report) || source.EvidenceVersion != f.ReportEvidenceVersion {
		return nil, nil
	}
	source.Available = false
	if source.Archived {
		source.UnavailableReason = "归档来源仅保留回执，会话关联未恢复"
		return &source, nil
	}
	if f.TaskID == nil {
		source.UnavailableReason = "原任务或探索节点已删除"
		return &source, nil
	}
	nodeID := int64(0)
	if f.NodeID != nil {
		nodeID = *f.NodeID
	}
	if source.TaskID != *f.TaskID || source.NodeID != nodeID {
		return nil, nil
	}
	if source.Kind == "recovery_job" {
		if source.RecoveryID == "" || source.RecoveryAttempt < 1 {
			return nil, nil
		}
		source.Available = true
		return &source, nil
	}
	if source.Kind != "conversation" || source.ConversationID <= 0 {
		return nil, nil
	}
	var created time.Time
	var key string
	err := d.QueryRow(`SELECT created_at,agent_key FROM conversations WHERE id=$1`, source.ConversationID).Scan(&created, &key)
	if errors.Is(err, sql.ErrNoRows) {
		source.UnavailableReason = "来源会话已删除，报告仍保留"
		return &source, nil
	}
	if err != nil {
		return nil, err
	}
	if created.UTC().Format(time.RFC3339Nano) != source.ConversationCreatedAt || key != source.ConversationAgentKey {
		source.UnavailableReason = "来源会话身份无法核对"
		return &source, nil
	}
	source.Available = true
	source.UnavailableReason = ""
	return &source, nil
}

func normalizeArchivedFindingReportSource(row map[string]any) {
	if source, ok := row["report_source"].(map[string]any); ok {
		// Conversations are not part of task packages. A bare numeric ID in a
		// restored package must not point at an unrelated local conversation.
		source["archived"], source["available"] = true, false
		source["unavailable_reason"] = "归档来源仅保留回执，会话关联未恢复"
	} else {
		row["report_source"] = nil
	}
}
