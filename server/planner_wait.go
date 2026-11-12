package server

import (
	"context"
	"encoding/json"
	"log"
	"strconv"

	"gondar/agent"
	"gondar/db"
)

func (e *Engine) pauseForPlannerWait(ctx context.Context, t *Task, wait agent.PlannerWait) {
	id, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil || ctx.Err() != nil {
		return
	}
	metadata, _ := json.Marshal(map[string]any{"planner_wait": wait})
	event := db.Activity{Worker: "system", Kind: "text", Summary: "等待具体条件，已有成果和未完成步骤已保留", Detail: wait.Reason + "；此为等待交接，不是规划故障，也不代表范围已验证完。条件具备后可继续。", Metadata: metadata}
	e.m.taskStateMu.Lock()
	won, err := e.m.pg.PauseTaskForPlannerWait(ctx, id, &event)
	if won {
		t.updateLifecycle(func(s *taskLifecycleState) { s.Paused = true; s.CompletedAt = 0 })
	}
	e.m.taskStateMu.Unlock()
	if err != nil || !won {
		if err != nil {
			log.Printf("[planner] task %s 等待交接未保存: %v", t.ID, err)
		}
		return
	}
	t.clearSettle()
	e.Pause(t.ID, agent.AbortContinuousPause)
	e.bc.Publish(t.ID, event)
	e.touch(t.ID)
}
