package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"gondar/db"
)

var ErrPlannerNoProgress = errors.New("planner_no_progress")

// One in-session correction is allowed; prose, TodoWrite and local drafts do
// not satisfy an idle frontier's dispatch requirement.
type plannerProgressGuard struct {
	inner         harness.HookRunner
	store         *db.ExplorationStore
	before        db.PlannerProgress
	goalMet       func() bool
	corrected     bool
	continuous    bool
	wait          func() *PlannerWait
	recoverWait   func(context.Context, PlannerWait) string
	waitRecovered bool
	reads         int
	readBaseline  *db.PlannerProgress
	mu            sync.Mutex
}

func (g *plannerProgressGuard) validate(ctx context.Context) error {
	if !g.continuous && g.goalMet != nil && g.goalMet() {
		return nil
	}
	after, err := g.store.PlannerProgress(ctx)
	if err != nil {
		return fmt.Errorf("读取规划实际进展失败：%w", err)
	}
	advanced := after.AdvancedSince(g.before)
	if g.continuous {
		advanced = after.ActiveIntents > 0 || after.LatestIntent > g.before.LatestIntent || after.LatestFinding > g.before.LatestFinding || after.EvidenceAdvancedSince(g.before)
	}
	if after.ActiveIntents > 0 || advanced {
		return nil
	}
	if g.wait != nil && g.wait() != nil {
		wait := *g.wait()
		if err := wait.Validate(ctx, g.store); err != nil {
			return fmt.Errorf("等待条件已变化：%w", err)
		}
		return &PlannerWaitError{Wait: wait}
	}
	if !g.continuous && after.OpenGoals == 0 {
		return nil
	}
	return fmt.Errorf("%w：本轮未形成新执行或实质证据进展（待核对目标%d个）；当前队列为空不代表范围已测完。缺前提时先保存验证格并调用record_planner_wait交接具体等待条件", ErrPlannerNoProgress, after.OpenGoals)
}

func (g *plannerProgressGuard) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if plannerLookupTool(name) && !g.allowLookup(ctx) {
		return true, "本轮已查阅12次且没有形成新执行或实质证据。请停止反复检索，依据已有材料提交少量具体意图；确实缺前提时保存阻塞并用record_planner_wait交接。不要猜测已覆盖或仅写待办。", nil
	}
	if g.inner != nil {
		return g.inner.PreToolUse(ctx, name, input)
	}
	return false, "", nil
}

func (g *plannerProgressGuard) allowLookup(ctx context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reads++
	if g.reads <= 12 {
		return true
	}
	baseline := g.before
	if g.readBaseline != nil {
		baseline = *g.readBaseline
	}
	if after, err := g.store.PlannerProgress(ctx); err == nil && (after.LatestIntent > baseline.LatestIntent || after.LatestFinding > baseline.LatestFinding || after.EvidenceAdvancedSince(baseline)) {
		g.readBaseline = &after
		g.reads = 1
		return true
	}
	return false
}

func (g *plannerProgressGuard) PostToolUse(ctx context.Context, name string, input, result []byte, failed bool) {
	if g.inner != nil {
		g.inner.PostToolUse(ctx, name, input, result, failed)
	}
}

func (g *plannerProgressGuard) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if g.inner != nil {
		if stop, blocking, text := g.inner.Stop(ctx, messages); stop || len(blocking) > 0 {
			return stop, blocking, text
		}
	}
	if ctx.Err() != nil {
		return false, nil, ""
	}
	if err := g.validate(ctx); err != nil {
		if errors.Is(err, ErrPlannerWaiting) {
			var waiting *PlannerWaitError
			if g.recoverWait != nil && !g.waitRecovered && errors.As(err, &waiting) {
				g.waitRecovered, g.corrected = true, true
				g.mu.Lock()
				g.reads = 0
				g.mu.Unlock()
				return false, []string{g.recoverWait(ctx, waiting.Wait)}, ""
			}
			return true, nil, err.Error()
		}
		if !errors.Is(err, ErrPlannerNoProgress) || g.corrected {
			return true, nil, err.Error()
		}
		g.corrected = true
		automatic := ""
		if g.recoverWait != nil && !g.waitRecovered {
			g.waitRecovered = true
			g.mu.Lock()
			g.reads = 0
			g.mu.Unlock()
			automatic = "\n\n" + g.recoverWait(ctx, PlannerWait{})
		}
		if g.continuous {
			return false, []string{"【持续挖掘：只纠偏一次】当前没有待执行/运行中的意图，也没有新的真实派发。旧目标met、待办、文字或脚本不代表整个探索完成。请在原授权范围内真实提交一小批未重复、前置条件齐全的 add_intent；确实无法推进则说明阻塞，不调用 goal_met、不重复证明旧目标。宿主会保留成果并按故障/空转规则停止。" + automatic}, ""
		}
		return false, []string{"【宿主规划校验：立即纠偏一次】目标尚未完成，当前没有待执行或运行中的意图。本轮文字、待办或生成脚本没有形成实际派发。请依据原目标和操作约束，真实调用 add_intent 提交一个小批次，或用真实证据完成目标判定。不要仅声称已派发；无法推进时明确说明阻塞。本轮只允许这一次纠偏。" + automatic}, ""
	}
	return false, nil, ""
}

// A model/turn cutoff must not bypass the automatic review by saving a wait
// immediately before termination. Such a round remains an unfinished plan.
func (g *plannerProgressGuard) validateResult(ctx context.Context) error {
	err := g.validate(ctx)
	if errors.Is(err, ErrPlannerWaiting) && g.recoverWait != nil && !g.waitRecovered {
		return fmt.Errorf("%w：本轮等待请求尚未经过自动推进复核，不能据此暂停任务", ErrPlannerNoProgress)
	}
	return err
}

func plannerLookupTool(name string) bool {
	switch name {
	case "graph_overview", "list_facts", "list_findings", "list_assets", "list_untested_assets", "list_goals", "node_detail", "get_worker_output", "get_worker_trace", "search_all_worker_traces", "list_worker_traces", "list_verifications", "expand_digest":
		return true
	}
	return false
}
