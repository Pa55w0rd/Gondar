package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"gondar/agent"
	"gondar/db"
)

func (m *Manager) updateTaskExecution(id string, patch db.TaskExecutionPatch) (*Task, error) {
	m.taskStateMu.Lock()
	defer m.taskStateMu.Unlock()
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, err
	}
	pt, err := m.pg.UpdateTaskExecution(n, patch)
	if err != nil || pt == nil {
		return nil, err
	}
	t, ok := m.Task(id)
	if !ok {
		return nil, nil
	}
	t.updateLifecycle(func(state *taskLifecycleState) {
		state.CompletionMode = pt.CompletionMode
		state.TimeoutSeconds = pt.TimeoutSeconds
		state.FirstRunAt = unixOrZero(pt.FirstRunAt)
		state.DeadlineAt = unixOrZero(pt.DeadlineAt)
	})
	return t, nil
}

func (s *Server) updateTaskExecution(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var request struct {
		CompletionMode *string `json:"completion_mode"`
		TimeoutSeconds *int    `json:"timeout_seconds"`
	}
	if err := decode(r, &request); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if request.CompletionMode == nil && request.TimeoutSeconds == nil {
		writeErr(w, 400, "至少提供完成策略或时限")
		return
	}
	if request.CompletionMode != nil {
		mode, err := db.NormalizeCompletionMode(*request.CompletionMode)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		request.CompletionMode = &mode
	}
	if request.TimeoutSeconds != nil && (*request.TimeoutSeconds < 0 || *request.TimeoutSeconds > 2147483647) {
		writeErr(w, 400, "任务时限必须为0至2147483647秒，0表示不限时")
		return
	}
	s.concMu.Lock()
	defer s.concMu.Unlock()
	if _, ok := s.m.Task(id); !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if s.engine.IsDeleting(id) || !s.engine.beginTaskOperation(id) {
		writeErr(w, 409, "任务正在删除")
		return
	}
	defer s.engine.decInflight(id)
	// This handler owns one operation; other executions must have drained before
	// mutable budgets/config can change under a live agent.
	if s.engine.inflightCount(id) > 1 {
		writeErr(w, 409, "当前执行仍在收尾，请等待停止后保存")
		return
	}
	t, err := s.m.updateTaskExecution(id, db.TaskExecutionPatch{CompletionMode: request.CompletionMode, TimeoutSeconds: request.TimeoutSeconds})
	if errors.Is(err, db.ErrTaskExecutionActive) {
		writeErr(w, 409, err.Error())
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if t == nil {
		writeErr(w, 404, "task not found")
		return
	}
	if request.TimeoutSeconds != nil {
		s.engine.deadline.Delete(id)
		s.engine.stamped.Delete(id)
	}
	s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text", Summary: "已保存任务完成策略与时限，未启动任务"})
	writeJSON(w, 200, taskDTO(t, s.resolvedTaskStatus(t)))
}

// continueTask starts a human-requested follow-up in the same exploration. It
// retains all proven goals and never reopens completed/blocked intents en masse.
func (s *Server) continueTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	previous := t.lifecycleSnapshot().Status
	queued, err := s.admitTaskWhen(t, s.resumeAdmissionMode(t), false, true)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	t.NotifyHint([]string{"人工要求继续持续挖掘：保留既有事实、漏洞、附件与单项目标证明；严格沿用原授权范围和操作约束。旧目标met不代表新一轮探索完成。先核对未验证面与已执行记录，真实派发少量不重复的可执行方向；缺账号或环境的部分如实记录，不伪造漏洞或时间，不批量重放旧操作。"})
	metadata, _ := json.Marshal(map[string]any{"human_continue": map[string]any{"previous_status": previous, "completion_mode": db.CompletionContinuous, "queued": queued}})
	s.engine.emitActivity(t, db.Activity{Worker: "system", Kind: "text", Summary: "已保留已有成果并继续持续挖掘", Metadata: metadata})
	writeJSON(w, 200, map[string]any{"id": t.ID, "queued": queued, "status": map[bool]string{true: "queued", false: "running"}[queued]})
}

func (e *Engine) pauseContinuousRun(t *Task, reason, detail string, failures ...string) {
	n, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil {
		return
	}
	e.m.taskStateMu.Lock()
	won, err := e.m.pg.PauseContinuousTask(n)
	if won {
		t.updateLifecycle(func(state *taskLifecycleState) { state.Paused = true; state.CompletedAt = 0 })
	}
	e.m.taskStateMu.Unlock()
	if err != nil || !won {
		return
	}
	t.clearSettle()
	e.Pause(t.ID, agent.AbortContinuousPause)
	metadata, _ := json.Marshal(map[string]any{"continuous_pause": map[string]any{"reason": reason, "detail": detail, "consecutive_reasons": failures, "resumable": true}})
	e.emitActivity(t, db.Activity{Worker: "system", Kind: "text", Summary: "持续挖掘触发保护，已暂停并保留成果", Detail: detail + "；暂停不代表授权范围已测完。检查阻塞后可人工继续。", Metadata: metadata})
}
