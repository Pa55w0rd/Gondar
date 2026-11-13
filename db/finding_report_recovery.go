package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	FindingReportRecoveryMaxAttempts = 6
	FindingReportRecoveryMinAge      = 20 * time.Minute
)

var ErrFindingReportRecoveryStale = errors.New("报告补写已失效：任务、报告、证据或领取状态已变化")

// FindingReportRecovery is an immutable, server-owned delivery claim. NodeID is
// provenance only: every mutation is keyed by the standalone FindingID/TaskID.
type FindingReportRecovery struct {
	FindingID, TaskID, ExplorationID, NodeID int64
	Attempt                                  int
	Token                                    string
	ReviewVersion                            json.RawMessage
	Name, VulnClass, Severity                string
	Summary, Evidence                        string
	EvidenceVersion                          int64
	Traffic                                  *FindingTraffic
	ProfileID                                *int64
	TaskChainConfigured                      bool
}

// Failure backoff is persistent and bounded; restarting cannot reset the budget.
func FindingReportRecoveryBackoff(attempt int) time.Duration {
	return time.Hour << uint(min(max(attempt-1, 0), 3))
}

const reportRecoveryEligibleTaskSQL = `EXISTS (
 SELECT 1 FROM tasks t WHERE t.id=f.task_id AND t.deleted_at IS NULL
 AND NOT t.paused AND NOT t.queued
 AND NOT EXISTS (SELECT 1 FROM task_archives a WHERE a.task_id=t.id AND a.state<>'archive_failed')
)`

const reportRecoveryEmptySQL = `btrim(COALESCE(f.report,''), E' \t\n\r')=''
 AND f.status NOT IN ('false_positive','ignored','duplicate')`

// ClaimFindingReportRecovery serializes with evidence writers and archive
// admission. It charges the attempt before any model call; a crash consumes that
// attempt and leaves a lease/cooldown rather than silently replaying work.
func (d *DB) ClaimFindingReportRecovery(ctx context.Context, now time.Time, lease time.Duration) (out *FindingReportRecovery, err error) {
	if lease <= 0 {
		return nil, errors.New("报告补写租约必须为正数")
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var id, taskID int64
		err := tx.QueryRowContext(ctx, `SELECT f.id,f.task_id FROM findings f
 WHERE `+reportRecoveryEmptySQL+` AND `+reportRecoveryEligibleTaskSQL+`
 AND f.created_at<=$1 AND f.report_recovery_attempts<$2
 AND (f.report_recovery_next_at IS NULL OR f.report_recovery_next_at<=$3)
 AND (f.report_recovery_lease_until IS NULL OR f.report_recovery_lease_until<=$3)
 ORDER BY f.report_recovery_next_at NULLS FIRST,f.created_at,f.id LIMIT 1`,
			now.Add(-FindingReportRecoveryMinAge), FindingReportRecoveryMaxAttempts, now).Scan(&id, &taskID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := LockTaskEvidenceTx(tx, taskID); err != nil {
			if errors.Is(err, ErrTaskArchiveState) {
				return nil
			}
			return err
		}
		job := &FindingReportRecovery{FindingID: id, TaskID: taskID, Token: hex.EncodeToString(token[:])}
		var profile sql.NullInt64
		var reviewVersion string
		err = tx.QueryRowContext(ctx, `UPDATE findings f SET
 report_recovery_attempts=f.report_recovery_attempts+1,
 report_recovery_token=$3,report_recovery_lease_until=$4,report_recovery_error=''
 FROM tasks t WHERE f.id=$1 AND f.task_id=$2 AND t.id=f.task_id
 AND `+reportRecoveryEmptySQL+` AND `+reportRecoveryEligibleTaskSQL+`
 RETURNING COALESCE(f.node_id,0),t.exploration_id,f.report_recovery_attempts,
 f.name,f.vulnclass,f.severity,f.summary,f.evidence,f.evidence_version,
 (`+findingReviewVersionSQL+`)::text,COALESCE(t.active_llm_profile_id,t.llm_profile_id),
 EXISTS(SELECT 1 FROM task_llm_profiles p WHERE p.task_id=t.id)`,
			id, taskID, job.Token, now.Add(lease)).Scan(&job.NodeID, &job.ExplorationID, &job.Attempt,
			&job.Name, &job.VulnClass, &job.Severity, &job.Summary, &job.Evidence, &job.EvidenceVersion, &reviewVersion, &profile, &job.TaskChainConfigured)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		job.ReviewVersion = json.RawMessage(reviewVersion)
		if profile.Valid {
			job.ProfileID = &profile.Int64
		}
		if _, err := tx.ExecContext(ctx, `UPDATE findings SET report_recovery_next_at=$2 WHERE id=$1`,
			id, now.Add(lease).Add(FindingReportRecoveryBackoff(job.Attempt))); err != nil {
			return err
		}
		job.Traffic, err = FindingTrafficTx(tx, id)
		if err != nil {
			return err
		}
		out = job
		return nil
	})
	return
}

// Renew also rechecks the complete finding snapshot and task lifecycle. A pause,
// deletion, archive or changed evidence cancels the delivery worker, not the task.
func (d *DB) RenewFindingReportRecovery(ctx context.Context, job *FindingReportRecovery, now time.Time, lease time.Duration) (bool, error) {
	valid := false
	err := d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		if err := LockTaskEvidenceTx(tx, job.TaskID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE findings f SET report_recovery_lease_until=$5
 WHERE f.id=$1 AND f.task_id=$2 AND f.report_recovery_token=$3
 AND f.report_recovery_lease_until>$4 AND `+reportRecoveryEmptySQL+`
 AND `+reportRecoveryEligibleTaskSQL+` AND (`+findingReviewVersionSQL+`)=$6::jsonb`,
			job.FindingID, job.TaskID, job.Token, now, now.Add(lease), string(job.ReviewVersion))
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		valid = n == 1
		return err
	})
	return valid, err
}

// Complete never overwrites even a one-character report. Snapshot equality
// covers text/metadata, triage, traffic and artifact evidence versions.
func (d *DB) CompleteFindingReportRecovery(ctx context.Context, job *FindingReportRecovery, report string, now time.Time) error {
	if strings.TrimSpace(report) == "" {
		return errors.New("报告补写正文为空")
	}
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		if err := LockTaskEvidenceTx(tx, job.TaskID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE findings f SET report=$4,report_evidence_version=$5,
 report_recovery_token='',report_recovery_lease_until=NULL,report_recovery_next_at=NULL,report_recovery_error=''
 WHERE f.id=$1 AND f.task_id=$2 AND f.report_recovery_token=$3 AND f.report_recovery_lease_until>$6
 AND `+reportRecoveryEmptySQL+` AND `+reportRecoveryEligibleTaskSQL+`
 AND (`+findingReviewVersionSQL+`)=$7::jsonb`,
			job.FindingID, job.TaskID, job.Token, utf8Clean(report), job.EvidenceVersion, now, string(job.ReviewVersion))
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err == nil && n != 1 {
			return ErrFindingReportRecoveryStale
		}
		if err != nil {
			return err
		}
		return storeFindingReportSourceTx(ctx, tx, job.FindingID, recoveryReportSource(job, utf8Clean(report), now))
	})
}

// Fail only releases its own claim. Callers pass a short diagnostic code, never
// raw provider errors or evidence. Archive admission freezes these fields too.
func (d *DB) FailFindingReportRecovery(ctx context.Context, job *FindingReportRecovery, reason string, now time.Time) error {
	if len(reason) > 200 {
		reason = "report_recovery_failed"
	}
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		if err := LockTaskEvidenceTx(tx, job.TaskID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE findings SET report_recovery_token='',report_recovery_lease_until=NULL,
 report_recovery_next_at=$4,report_recovery_error=$5 WHERE id=$1 AND task_id=$2 AND report_recovery_token=$3`,
			job.FindingID, job.TaskID, job.Token, now.Add(FindingReportRecoveryBackoff(job.Attempt)), reason)
		return err
	})
}

// Delivery usage stays on the existing task activity ledger. Archive admission
// uses the same task lock, so a late result cannot repopulate a cold task.
func (d *DB) AppendFindingReportRecoveryResult(ctx context.Context, job *FindingReportRecovery, a Activity) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		if err := LockTaskEvidenceTx(tx, job.TaskID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO activity
 (exploration_id,worker,kind,is_error,summary,metadata,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens)
 SELECT exploration_id,'reporter','result',$3,$4,$5,$6,$7,$8,$9 FROM tasks WHERE id=$1 AND exploration_id=$2`,
			job.TaskID, job.ExplorationID, a.IsError, a.Summary, []byte(a.Metadata), a.InputTokens, a.OutputTokens, a.CacheReadTokens, a.CacheWriteTokens)
		return err
	})
}

// Findings already participate in snapshots/deletion. Old archives lack these
// columns; new archives retain attempts/backoff but can never revive a lease.
func normalizeArchivedFindingReportRecovery(row map[string]any) {
	if row["report_recovery_attempts"] == nil {
		row["report_recovery_attempts"] = 0
	}
	if row["report_recovery_error"] == nil {
		row["report_recovery_error"] = ""
	}
	row["report_recovery_token"] = ""
	row["report_recovery_lease_until"] = nil
}
