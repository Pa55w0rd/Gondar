package server

import (
	"fmt"
	"strings"

	"gondar/db"
)

// Only an exact, attachment-free operator command is handled by the host.
// Questions, negations, quoted text and directions still go to the main agent.
func isTaskResumeCommand(message string) bool {
	message = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(message), "。.!！"))
	switch message {
	case "继续", "继续任务", "继续执行", "恢复任务", "恢复执行":
		return true
	}
	return false
}

func (s *Server) resumeTaskFromChat(t *Task) (string, error) {
	state := t.lifecycleSnapshot()
	if state.Queued {
		return "任务已在等待队列中，无需重复继续。", nil
	}
	if !state.Paused && !isTerminalStatus(state.Status) {
		return "任务已在运行，无需重复继续。", nil
	}
	var queued bool
	var err error
	if state.Paused {
		var result taskControlResult
		result, err = s.applyTaskControl(t, "resume")
		queued = result.Queued
	} else {
		if state.CompletionMode != db.CompletionContinuous {
			total, unmet, readErr := t.Store.GoalCompletionCounts(s.ctx)
			if readErr != nil {
				return "", readErr
			}
			active, readErr := t.Store.HasActiveIntent()
			if readErr != nil {
				return "", readErr
			}
			if total > 0 && unmet == 0 && !active {
				return "现有目标均已完成，未重新启动。继续挖掘可先选择持续挖掘策略，或补充新的目标。", nil
			}
		}
		queued, err = s.admitTask(t, s.resumeAdmissionMode(t))
	}
	if err != nil {
		return "", fmt.Errorf("未能继续任务：%w", err)
	}
	if queued {
		return "已保留成果并加入继续队列，获得运行条件后开始；防空转和故障保护仍有效。", nil
	}
	return "已保留成果并继续任务，规划保护计数已重新开始；防空转和故障保护仍有效。", nil
}
