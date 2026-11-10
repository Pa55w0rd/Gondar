package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"gondar/db"
)

var ErrPlannerNoProgress = errors.New("planner_no_progress")

// One in-session correction is allowed; prose, TodoWrite and local drafts do
// not satisfy an idle frontier's dispatch requirement.
type plannerProgressGuard struct {
	inner      harness.HookRunner
	store      *db.ExplorationStore
	before     db.PlannerProgress
	goalMet    func() bool
	corrected  bool
	continuous bool
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
		advanced = after.ActiveIntents > 0 || after.LatestIntent > g.before.LatestIntent || after.LatestFinding > g.before.LatestFinding
	}
	if (!g.continuous && after.OpenGoals == 0) || advanced {
		return nil
	}
	return fmt.Errorf("%w：仍有%d个未完成目标，没有待执行/运行中的意图，也没有新意图、漏洞或目标判定；草稿和模型文字不能作为派发回执", ErrPlannerNoProgress, after.OpenGoals)
}

func (g *plannerProgressGuard) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if g.inner != nil {
		return g.inner.PreToolUse(ctx, name, input)
	}
	return false, "", nil
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
		if !errors.Is(err, ErrPlannerNoProgress) || g.corrected {
			return true, nil, err.Error()
		}
		g.corrected = true
		if g.continuous {
			return false, []string{"【持续挖掘：只纠偏一次】当前没有待执行/运行中的意图，也没有新的真实派发。旧目标met、待办、文字或脚本不代表整个探索完成。请在原授权范围内真实提交一小批未重复、前置条件齐全的 add_intent；确实无法推进则说明阻塞，不调用 goal_met、不重复证明旧目标。宿主会保留成果并按故障/空转规则停止。"}, ""
		}
		return false, []string{"【宿主规划校验：立即纠偏一次】目标尚未完成，当前没有待执行或运行中的意图。本轮文字、待办或生成脚本没有形成实际派发。请依据原目标和操作约束，真实调用 add_intent 提交一个小批次，或用真实证据完成目标判定。不要仅声称已派发；无法推进时明确说明阻塞。本轮只允许这一次纠偏。"}, ""
	}
	return false, nil, ""
}
