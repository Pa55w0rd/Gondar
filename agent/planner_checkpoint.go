package agent

import (
	"context"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

// A terminal checkpoint cannot dispatch new target work, including via a
// hallucinated or newly unlocked tool. Existing evidence reads/writes stay live.
var plannerCheckpointTools = map[string]bool{
	"graph_overview": true, "list_goals": true, "node_detail": true, "list_facts": true, "list_findings": true,
	"get_worker_output": true, "get_worker_trace": true, "search_all_worker_traces": true, "list_worker_traces": true,
	"list_resources": true, "register_resource": true, "update_resource": true, "list_artifacts": true, "preview_evidence": true,
	"save_evidence": true, "record_fact": true, "report_finding": true, "prove_goal": true, "goal_met": true, "TodoWrite": true,
	"Read": true, "LS": true, "Glob": true, "Grep": true,
}

type plannerCheckpointHooks struct{ inner harness.HookRunner }

func (h *plannerCheckpointHooks) PreToolUse(ctx context.Context, name string, in []byte) (bool, string, []byte) {
	if !plannerCheckpointTools[name] {
		return true, "任务正在有限收尾：只允许读取既有材料及保存证据/交接，不再探测目标或派发新意图。", nil
	}
	if h.inner != nil {
		return h.inner.PreToolUse(ctx, name, in)
	}
	return false, "", nil
}
func (h *plannerCheckpointHooks) PostToolUse(ctx context.Context, name string, in, out []byte, failed bool) {
	if h.inner != nil {
		h.inner.PostToolUse(ctx, name, in, out, failed)
	}
}
func (h *plannerCheckpointHooks) Stop(ctx context.Context, msgs []llm.Message) (bool, []string, string) {
	if h.inner != nil {
		return h.inner.Stop(ctx, msgs)
	}
	return false, nil, ""
}
