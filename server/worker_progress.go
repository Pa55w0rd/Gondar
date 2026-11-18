package server

import (
	"encoding/json"
	"fmt"
	"gondar/agent"
)

const workerProgressSetting = "worker_progress_policy"

func (s *Server) workerProgressPolicy() agent.WorkerProgressPolicy {
	p := agent.DefaultWorkerProgressPolicy()
	if s != nil && s.m != nil && s.m.pg != nil {
		if value, ok, err := s.m.pg.GetSetting(workerProgressSetting); err == nil && ok {
			if json.Unmarshal([]byte(value), &p) == nil && p.Validate() == nil {
				return p
			}
		}
	}
	return agent.DefaultWorkerProgressPolicy()
}

func runtimeLimits(runSeconds, wrapupTurns int, wrapupCustom bool) map[string]any {
	callSeconds, source := runSeconds, "跟随Agent运行配置；任务剩余时限可进一步缩短"
	if callSeconds <= 0 {
		callSeconds, source = 600, "主探索不限时，单次模型调用使用内置10分钟故障保护"
	}
	wrapupSource := "内置默认"
	if wrapupCustom {
		wrapupSource = "Agent收尾配置"
	}
	return map[string]any{
		"run_seconds": runSeconds, "run_source": "Agent运行配置，0表示主探索不限时",
		"model_call_seconds": callSeconds, "model_call_source": source,
		"wrapup_turns": wrapupTurns, "wrapup_source": wrapupSource,
		"task_timeout_note": "任务总时限单独配置；0表示不限，不会取消Worker单轮预算",
		"tool_timeout_note": "单次工具按调用参数及工具自身上限执行；预算收尾只保存已有材料",
	}
}

func (s *Server) saveWorkerProgressPolicy(p agent.WorkerProgressPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if s == nil || s.m == nil || s.m.pg == nil {
		return fmt.Errorf("数据库不可用")
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.m.pg.SetSetting(workerProgressSetting, string(data))
}
