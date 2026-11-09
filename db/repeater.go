package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	RepeaterRunLimit          = 200
	RepeaterStorageLimit      = 32 << 20
	RepeaterRequestLimit      = 128 << 10
	RepeaterResponseLimit     = 256 << 10
	RepeaterResponseHeadLimit = 64 << 10
)

var ErrRepeaterQuota = errors.New("本任务重放历史已达到保留配额；请在新任务继续，已有证据不会被删除")

// ReservedBytes measures bounded HTTP messages, not PostgreSQL's physical disk
// size. Pending runs reserve their maximum response head and body before send.
type RepeaterUsage struct {
	Count         int   `json:"run_count"`
	ReservedBytes int64 `json:"reserved_bytes"`
}

const repeaterUsageSQL = `SELECT count(*), COALESCE(sum(octet_length(request)+octet_length(response_head)+octet_length(response_body)+CASE WHEN state='running' THEN 327680 ELSE 0 END),0) FROM http_repeater_runs WHERE task_id=$1`

func (d *DB) RepeaterHistoryUsage(taskID int64) (RepeaterUsage, error) {
	var u RepeaterUsage
	err := d.QueryRow(repeaterUsageSQL, taskID).Scan(&u.Count, &u.ReservedBytes)
	return u, err
}

// RepeaterRun is a bounded, task-owned manual HTTP execution. Bodies remain raw
// bytes (JSON uses base64), so binary evidence is not silently re-encoded.
type RepeaterRun struct {
	ID              int64      `json:"id"`
	TaskID          int64      `json:"task_id"`
	SourceTrafficID string     `json:"source_traffic_id,omitempty"`
	TrafficID       string     `json:"traffic_id,omitempty"`
	URL             string     `json:"url"`
	Method          string     `json:"method"`
	Request         string     `json:"request,omitempty"`
	State           string     `json:"state"`
	Status          int        `json:"status"`
	ResponseHead    string     `json:"response_head,omitempty"`
	ResponseBody    []byte     `json:"response_body_base64,omitempty"`
	ResponseSHA256  string     `json:"response_sha256"`
	ResponseBytes   int        `json:"response_bytes"`
	Truncated       bool       `json:"truncated"`
	Error           string     `json:"error,omitempty"`
	CaptureNote     string     `json:"capture_note,omitempty"`
	ElapsedMS       int64      `json:"elapsed_ms"`
	CreatedAt       time.Time  `json:"created_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

func (d *DB) CreateRepeaterRun(ctx context.Context, r *RepeaterRun) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	// The same short row lock is used by QueueTaskArchive. Once this running row
	// exists, archiving waits for the manual execution to finish, not task status.
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, r.TaskID).Scan(&id); err != nil {
		return err
	}
	var archiving bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_archives WHERE task_id=$1 AND state<>'archive_failed')`, r.TaskID).Scan(&archiving); err != nil {
		return err
	}
	if archiving {
		return fmt.Errorf("%w: 任务正在归档或恢复，未发送", ErrTaskArchiveState)
	}
	var usage RepeaterUsage
	if err := tx.QueryRowContext(ctx, repeaterUsageSQL, r.TaskID).Scan(&usage.Count, &usage.ReservedBytes); err != nil {
		return err
	}
	if usage.Count >= RepeaterRunLimit || usage.ReservedBytes+int64(len(r.Request))+RepeaterResponseLimit+RepeaterResponseHeadLimit > RepeaterStorageLimit {
		return ErrRepeaterQuota
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO http_repeater_runs(task_id,source_traffic_id,url,method,request,state)
VALUES($1,$2,$3,$4,$5,'running') RETURNING id,created_at`, r.TaskID, r.SourceTrafficID, r.URL, r.Method, r.Request).Scan(&r.ID, &r.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func taskHasRunningRepeater(tx *sql.Tx, taskID int64) (bool, error) {
	var running bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM http_repeater_runs WHERE task_id=$1 AND state='running')`, taskID).Scan(&running)
	return running, err
}

// Called once when the owning server starts, never when a helper opens a DB.
func (d *DB) RecoverInterruptedRepeaterRuns() error {
	_, err := d.Exec(`UPDATE http_repeater_runs SET state='error',error='服务上次退出时请求尚未完成；不会自动重发',completed_at=now() WHERE state='running'`)
	return err
}

func (d *DB) FinishRepeaterRun(ctx context.Context, r *RepeaterRun) error {
	if r.ResponseBody == nil {
		r.ResponseBody = []byte{}
	}
	return d.QueryRowContext(ctx, `UPDATE http_repeater_runs SET state=$2,status=$3,response_head=$4,
response_body=$5,response_sha256=$6,truncated=$7,error=$8,capture_note=$9,elapsed_ms=$10,traffic_id=$11,completed_at=now()
WHERE id=$1 RETURNING completed_at`, r.ID, r.State, r.Status, r.ResponseHead, r.ResponseBody, r.ResponseSHA256, r.Truncated, r.Error, r.CaptureNote, r.ElapsedMS, r.TrafficID).Scan(&r.CompletedAt)
}

const repeaterSummaryCols = `id,task_id,source_traffic_id,traffic_id,url,method,state,status,response_sha256,
octet_length(response_body),truncated,error,capture_note,elapsed_ms,created_at,completed_at`

func scanRepeaterRun(row interface{ Scan(...any) error }, full bool) (*RepeaterRun, error) {
	r := &RepeaterRun{}
	args := []any{&r.ID, &r.TaskID, &r.SourceTrafficID, &r.TrafficID, &r.URL, &r.Method, &r.State, &r.Status, &r.ResponseSHA256, &r.ResponseBytes, &r.Truncated, &r.Error, &r.CaptureNote, &r.ElapsedMS, &r.CreatedAt, &r.CompletedAt}
	if full {
		args = append(args, &r.Request, &r.ResponseHead, &r.ResponseBody)
	}
	if err := row.Scan(args...); err != nil {
		return nil, err
	}
	return r, nil
}

func (d *DB) ListRepeaterRuns(taskID int64) ([]*RepeaterRun, error) {
	rows, err := d.Query(`SELECT `+repeaterSummaryCols+` FROM http_repeater_runs WHERE task_id=$1 ORDER BY id DESC LIMIT 50`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RepeaterRun{}
	for rows.Next() {
		r, err := scanRepeaterRun(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) GetRepeaterRun(taskID, id int64) (*RepeaterRun, error) {
	r, err := scanRepeaterRun(d.QueryRow(`SELECT `+repeaterSummaryCols+`,request,response_head,response_body
FROM http_repeater_runs WHERE task_id=$1 AND id=$2`, taskID, id), true)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}
