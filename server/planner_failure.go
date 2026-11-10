package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"time"

	"gondar/agent"
	"gondar/db"
)

func plannerFailureCode(err error, roundTimedOut bool) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, agent.ErrTaskBudgetExpired):
		return "task_deadline"
	case roundTimedOut:
		return "planner_round_timeout"
	case errors.Is(err, agent.ErrPlannerModelError) && (errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)):
		return "planner_response_interrupted"
	case errors.Is(err, agent.ErrPlannerModelError):
		return "planner_model_unavailable"
	case errors.Is(err, agent.ErrPlannerNoProgress):
		return "planner_no_progress"
	case errors.Is(err, agent.ErrPlannerReadLoop):
		return "planner_read_loop"
	case errors.Is(err, agent.ErrToolFailureLoop):
		return "planner_tool_failure_loop"
	default:
		return "other"
	}
}

func (e *Engine) recordPlannerOutcome(ctx context.Context, t *Task, round int64, code string) {
	if ctx.Err() != nil {
		return // user pause/shutdown/task deadline never counts as a planner failure
	}
	metadata, _ := json.Marshal(map[string]any{"planner_outcome": db.PlannerOutcome{Version: 1, Round: round, Code: code}})
	summary := "宿主已核对本轮规划结果"
	if code != "" {
		summary = "本轮规划未成功：" + code
	}
	if code == "task_deadline" {
		summary = "任务总时限已到，当前规划转入收尾"
	}
	if code == "planner_response_interrupted" {
		summary = "模型响应提前中断，本轮规划未完成：" + code
	}
	e.emitActivity(t, db.Activity{Worker: "planner", Kind: "text", Summary: summary, IsError: code != "" && code != "task_deadline", Metadata: metadata})
	if code == "task_deadline" {
		return
	}
	streakCode, streak, err := t.Store.PlannerFailureStreak(ctx)
	if err != nil {
		log.Printf("[planner] task %s 读取故障计数失败: %v", t.ID, err)
		return
	}
	if streak < 3 || ctx.Err() != nil || e.IsPaused(t.ID) || e.IsDeleting(t.ID) {
		return
	}
	if t.lifecycleSnapshot().CompletionMode == db.CompletionContinuous && streakCode == "planner_no_progress" {
		e.pauseContinuousRun(t, "planner_no_progress", "连续三轮没有提交真实的新方向，已保留成果，等待补充前置条件或人工继续")
		return
	}
	won, err := e.m.failPlannerTask(t)
	if err != nil || !won {
		return
	}
	metadata, _ = json.Marshal(map[string]any{"runtime_failure": map[string]any{"code": streakCode, "consecutive_runs": streak, "resumable": true}})
	e.emitActivity(t, db.Activity{Worker: "system", Kind: "text", IsError: true,
		Summary: "连续3轮规划故障，已停止任务并保留进度",
		Detail:  fmt.Sprintf("宿主连续记录到%d轮规划故障：%s。每轮已受预算及调用层重试约束，任务不会被标记为完成。目标、意图、事实、漏洞、证据和规划交接均保留；修复模型服务或具体阻塞后可人工恢复。", streak, streakCode), Metadata: metadata})
	e.cancelExec(t.ID, agent.AbortPlannerFailure)
}

const plannerIdleRetryDelay = 30 * time.Second

func (e *Engine) plannerRetryLimit() time.Duration {
	if e.plannerRetryDelay > 0 {
		return e.plannerRetryDelay
	}
	return plannerIdleRetryDelay
}

func (e *Engine) shouldRetryIdlePlanner(ctx context.Context, t *Task, code string) bool {
	switch code {
	case "planner_no_progress", "planner_read_loop", "planner_tool_failure_loop", "planner_model_unavailable", "planner_round_timeout", "planner_response_interrupted":
	default:
		return false
	}
	if ctx.Err() != nil || e.IsPaused(t.ID) || e.IsDeleting(t.ID) || e.isSettling(t.ID) || isTerminalStatus(t.lifecycleSnapshot().Status) {
		return false
	}
	if deadline := e.taskDeadline(t); deadline > 0 && time.Until(time.Unix(deadline, 0)) <= e.plannerRetryLimit() {
		return false
	}
	active, err := t.Store.HasActiveIntent()
	if err != nil || active {
		return false
	}
	open, err := t.Store.HasOpenGoal()
	return err == nil && (open || t.lifecycleSnapshot().CompletionMode == db.CompletionContinuous)
}

func (m *Manager) failPlannerTask(t *Task) (bool, error) {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	id, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil {
		return false, err
	}
	won, err := m.pg.FailPlannerTask(id)
	if err != nil || !won {
		return won, err
	}
	t.updateLifecycle(func(state *taskLifecycleState) {
		state.Status = "failed"
		state.CompletedAt = time.Now().Unix()
	})
	return true, nil
}
