package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"
)

// Daily plans create independent, bounded tasks through the existing admission
// queue. The snapshot is private: credentials are stored like tasks.auth_config
// and must never be serialized in an API response.
type dailySnapshot struct {
	Description string
	Goal        string
	Options     TaskCreateOptions
	Auth        json.RawMessage
	Scopes      []TaskScope
	Constraints []Constraint
}

type DailySchedule struct {
	ID             int64     `json:"id"`
	SourceTaskID   int64     `json:"source_task_id"`
	DailyAt        string    `json:"daily_at"`
	Timezone       string    `json:"timezone"`
	Enabled        bool      `json:"enabled"`
	MaxRuns        int       `json:"max_runs"`
	RunsCreated    int       `json:"runs_created"`
	TimeoutSeconds int       `json:"timeout_seconds"`
	NextRunAt      time.Time `json:"next_run_at"`
	LastTaskID     *int64    `json:"last_task_id,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	Skipped        int       `json:"skipped"`
	CreatedAt      time.Time `json:"created_at"`
}

const dailyScheduleCols = `id,source_task_id,daily_at,timezone,enabled,max_runs,runs_created,timeout_seconds,next_run_at,last_task_id,last_error,skipped,created_at`

func scanDaily(sc interface{ Scan(...any) error }) (*DailySchedule, error) {
	var s DailySchedule
	err := sc.Scan(&s.ID, &s.SourceTaskID, &s.DailyAt, &s.Timezone, &s.Enabled, &s.MaxRuns, &s.RunsCreated, &s.TimeoutSeconds, &s.NextRunAt, &s.LastTaskID, &s.LastError, &s.Skipped, &s.CreatedAt)
	return &s, err
}

// NextDaily uses calendar days, not 24h durations. Nonexistent local times are
// skipped. On a repeated DST clock, only its first occurrence is used.
func NextDaily(after time.Time, clock, timezone string) (time.Time, error) {
	parsed, err := time.Parse("15:04", clock)
	if err != nil || parsed.Format("15:04") != clock {
		return time.Time{}, errors.New("每日时间必须是 HH:mm")
	}
	if strings.TrimSpace(timezone) == "" || timezone == "Local" {
		return time.Time{}, errors.New("必须选择明确的 IANA 时区")
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, errors.New("时区无效")
	}
	a := after.In(loc)
	for day := 0; day < 370; day++ {
		date := time.Date(a.Year(), a.Month(), a.Day()+day, 12, 0, 0, 0, loc)
		candidate := time.Date(date.Year(), date.Month(), date.Day(), parsed.Hour(), parsed.Minute(), 0, 0, loc)
		matches := func(t time.Time) bool {
			v := t.In(loc)
			return v.Year() == date.Year() && v.YearDay() == date.YearDay() && v.Hour() == parsed.Hour() && v.Minute() == parsed.Minute()
		}
		if !matches(candidate) {
			continue
		}
		// time.Date may choose the later instance of an ambiguous local time.
		for earlier := candidate.Add(-26 * time.Hour); earlier.Before(candidate); earlier = earlier.Add(time.Minute) {
			if matches(earlier) {
				candidate = earlier
				break
			}
		}
		if candidate.After(after) {
			return candidate, nil
		}
	}
	return time.Time{}, errors.New("无法计算下一次计划时间")
}

func (d *DB) CreateDailySchedule(sourceID int64, clock, zone string, maxRuns, timeout int, now time.Time) (*DailySchedule, error) {
	if maxRuns < 1 || maxRuns > 366 {
		return nil, errors.New("执行次数必须为 1 到 366")
	}
	if timeout < 60 || timeout > 86400 {
		return nil, errors.New("每次任务预算必须为 60 到 86400 秒")
	}
	next, err := NextDaily(now, clock, zone)
	if err != nil {
		return nil, err
	}
	tx, err := d.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	t, err := scanTask(tx.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, sourceID))
	if err != nil {
		return nil, fmt.Errorf("源任务不存在：%w", err)
	}
	if err := ensureTaskExtensionMutable(tx, sourceID); err != nil {
		return nil, err
	}
	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM daily_task_schedules WHERE source_task_id=$1 AND enabled`, sourceID).Scan(&count); err != nil {
		return nil, err
	}
	if count >= 8 {
		return nil, errors.New("每个源任务最多启用 8 个日历计划")
	}
	opts := TaskCreateOptions{Name: t.Name, CategoryID: t.CategoryID, TimeoutSeconds: timeout, PlanHeartbeatSeconds: t.PlanHeartbeatSeconds, CoverageEnabled: &t.CoverageEnabled, CompletionMode: t.CompletionMode, AssetContextMode: t.AssetContextMode}
	rows, err := tx.Query(`SELECT profile_id FROM task_llm_profiles WHERE task_id=$1 ORDER BY position`, sourceID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		opts.LLMProfileIDs = append(opts.LLMProfileIDs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(opts.LLMProfileIDs) == 0 && t.LLMProfileID != nil {
		opts.LLMProfileIDs = []int64{*t.LLMProfileID}
	}
	rows, err = tx.Query(`SELECT enabled,action,kind,pattern,note FROM task_intercept_rules WHERE task_id=$1 ORDER BY id`, sourceID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r TaskInterceptRuleInput
		if err := rows.Scan(&r.Enabled, &r.Action, &r.Kind, &r.Pattern, &r.Note); err != nil {
			rows.Close()
			return nil, err
		}
		opts.InterceptRules = append(opts.InterceptRules, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	auth, err := t.AuthConfig.DBValue()
	if err != nil {
		return nil, err
	}
	scopes := []TaskScope{}
	rows, err = tx.Query(`SELECT kind,company_id,COALESCE(domain,''),COALESCE(net::text,''),COALESCE(value,''),source,COALESCE(reason,'') FROM task_scope WHERE task_id=$1 ORDER BY id LIMIT 1001`, sourceID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var scope TaskScope
		if err := rows.Scan(&scope.Kind, &scope.CompanyID, &scope.Domain, &scope.Net, &scope.Value, &scope.Source, &scope.Reason); err != nil {
			rows.Close()
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(scopes) > 1000 {
		return nil, errors.New("源任务范围超过 1000 条，请拆分计划")
	}
	// A repeatable snapshot keeps the size check and the full constraint read
	// consistent. Oversized boundaries reject the plan, never keep only a prefix.
	var constraintCount, constraintBytes int64
	if err := tx.QueryRow(`SELECT count(*),COALESCE(sum(octet_length(text)+octet_length(COALESCE(origin,''))),0) FROM task_constraints WHERE exploration_id=$1`, t.ExplorationID).Scan(&constraintCount, &constraintBytes); err != nil {
		return nil, err
	}
	if constraintCount > 1000 || constraintBytes > 1<<20 {
		return nil, errors.New("源任务操作约束超过1000条或1MiB，请整理后创建计划；未截断或保存部分约束")
	}
	constraints := []Constraint{}
	rows, err = tx.Query(`SELECT kind,text,COALESCE(origin,'') FROM task_constraints WHERE exploration_id=$1 ORDER BY id`, t.ExplorationID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c Constraint
		if err := rows.Scan(&c.Kind, &c.Text, &c.Origin); err != nil {
			rows.Close()
			return nil, err
		}
		constraints = append(constraints, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	snapshot, err := json.Marshal(dailySnapshot{Description: t.Description, Goal: t.Goal, Options: opts, Auth: auth, Scopes: scopes, Constraints: constraints})
	if err != nil {
		return nil, err
	}
	s, err := scanDaily(tx.QueryRow(`INSERT INTO daily_task_schedules(source_task_id,daily_at,timezone,max_runs,timeout_seconds,next_run_at,snapshot)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+dailyScheduleCols, sourceID, clock, zone, maxRuns, timeout, next, string(snapshot)))
	if err != nil {
		return nil, err
	}
	return s, tx.Commit()
}

func (d *DB) ListDailySchedules(sourceID int64) ([]*DailySchedule, error) {
	rows, err := d.Query(`SELECT `+dailyScheduleCols+` FROM daily_task_schedules WHERE source_task_id=$1 ORDER BY id DESC`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*DailySchedule{}
	for rows.Next() {
		s, err := scanDaily(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CancelDailySchedule serializes with materialization on the schedule row. Once
// this returns, no later task can be created by this plan. Already-created tasks
// retain their own explicit pause/cancel controls.
func (d *DB) CancelDailySchedule(sourceID, id int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockTaskExtension(tx, sourceID, 0); err != nil {
		return err
	}
	r, err := tx.Exec(`UPDATE daily_task_schedules SET enabled=false WHERE source_task_id=$1 AND id=$2`, sourceID, id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// MaterializeDailySchedule handles one due occurrence atomically. Concurrent
// schedulers cannot duplicate it, and restart sees the persistent bootstrap row.
func (d *DB) MaterializeDailySchedule(now time.Time) (*Task, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id, sourceID int64
	var clock, zone string
	var raw []byte
	var last sql.NullInt64
	var due time.Time
	err = tx.QueryRow(`SELECT s.id,s.source_task_id
FROM daily_task_schedules s WHERE s.enabled AND s.next_run_at<=$1 AND s.runs_created<s.max_runs
AND EXISTS(SELECT 1 FROM tasks t WHERE t.id=s.source_task_id AND t.deleted_at IS NULL)
AND NOT EXISTS(SELECT 1 FROM task_archives a WHERE a.task_id=s.source_task_id AND a.state!='archive_failed')
ORDER BY s.next_run_at,s.id LIMIT 1`, now).Scan(&id, &sourceID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Match archive/delete lock ordering: source task first, schedule second.
	if err := lockTaskExtension(tx, sourceID, 0); err != nil {
		if errors.Is(err, ErrTaskExtensionFrozen) || errors.Is(err, ErrTaskExtensionUnavailable) {
			return nil, nil
		}
		return nil, err
	}
	err = tx.QueryRow(`SELECT daily_at,timezone,snapshot,last_task_id,next_run_at FROM daily_task_schedules WHERE id=$1 AND enabled AND next_run_at<=$2 AND runs_created<max_runs FOR UPDATE`, id, now).Scan(&clock, &zone, &raw, &last, &due)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	next, err := NextDaily(now, clock, zone)
	if err != nil {
		return nil, err
	}
	if last.Valid {
		var status string
		err = tx.QueryRow(`SELECT status FROM tasks WHERE id=$1 AND deleted_at IS NULL`, last.Int64).Scan(&status)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if err == nil && !IsTerminal(status) {
			_, err = tx.Exec(`UPDATE daily_task_schedules SET next_run_at=$2,skipped=skipped+1,last_error='上次任务尚未结束，本次跳过' WHERE id=$1`, id, next)
			if err != nil {
				return nil, err
			}
			return nil, tx.Commit()
		}
	}
	var snap dailySnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, err
	}
	if snap.Constraints == nil {
		if _, err := tx.Exec(`UPDATE daily_task_schedules SET enabled=false,last_error='旧计划缺少操作约束快照，已停用；请核对范围后新建计划' WHERE id=$1`, id); err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}
	snap.Options.AuthConfig = ParseTaskAuth(snap.Auth)
	if _, err := tx.Exec(`SAVEPOINT schedule_task`); err != nil {
		return nil, err
	}
	t, err := d.createTaskWithOptionsTx(tx, snap.Description, snap.Goal, snap.Options)
	if err == nil {
		for _, constraint := range snap.Constraints {
			_, err = tx.Exec(`INSERT INTO task_constraints(exploration_id,kind,text,origin) VALUES($1,$2,$3,$4)`, t.ExplorationID, constraint.Kind, constraint.Text, constraint.Origin)
			if err != nil {
				break
			}
		}
	}
	if err == nil {
		for _, scope := range snap.Scopes {
			_, err = tx.Exec(`INSERT INTO task_scope(task_id,kind,company_id,domain,net,value,source,reason) VALUES($1,$2,$3,NULLIF($4,''),NULLIF($5,'')::cidr,NULLIF($6,''),$7,NULLIF($8,'')) ON CONFLICT DO NOTHING`, t.ID, scope.Kind, scope.CompanyID, scope.Domain, scope.Net, scope.Value, scope.Source, scope.Reason)
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		if _, rb := tx.Exec(`ROLLBACK TO SAVEPOINT schedule_task`); rb != nil {
			return nil, rb
		}
		if _, e := tx.Exec(`UPDATE daily_task_schedules SET enabled=false,last_error='创建任务失败，计划已停用；请核对模型、分类与源配置后重建' WHERE id=$1`, id); e != nil {
			return nil, e
		}
		return nil, tx.Commit()
	}
	// Never inherit old findings as if freshly verified. Only configuration is
	// snapshotted; bootstrap extracts and seeds this run's target graph normally.
	if _, err := tx.Exec(`UPDATE tasks SET queued=true,queued_at=$2,queue_mode='bootstrap' WHERE id=$1`, t.ID, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO daily_task_occurrences(schedule_id,scheduled_at,task_id) VALUES($1,$2,$3)`, id, due, t.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE daily_task_schedules SET runs_created=runs_created+1,last_task_id=$2,next_run_at=$3,last_error='',enabled=(runs_created+1<max_runs) WHERE id=$1`, id, t.ID, next); err != nil {
		return nil, err
	}
	t.Queued = true
	t.QueueMode = "bootstrap"
	t.QueuedAt = &now
	return t, tx.Commit()
}
