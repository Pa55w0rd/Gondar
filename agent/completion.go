package agent

import (
	"context"
	"gondar/db"
)

func (t *ToolSet) completionMode(ctx context.Context) (string, error) {
	if t.ts == nil {
		return db.CompletionObjective, nil
	}
	return t.ts.TaskCompletionMode(ctx)
}

func completionGuidance(mode string) string {
	if mode == db.CompletionContinuous {
		return `
【宿主完成策略：持续挖掘，不能由模型更改】
已有事实、漏洞、证据和目标证明全部保留。目标 met 只代表该项已有交付，不代表整个探索结束；新增一个漏洞、零漏洞清单、接口返回401/403、缺账号或已有较多发现，都不能证明授权范围已经测完。
禁止调用 goal_met 结束任务。prove_goal 只记录单项目标的证据，不会结束持续任务；不要反复证明旧目标。每轮从未验证面、尚未闭合的线索、已有工作真实缺口中选择少量互不重复、前置条件齐全的核验卡，真实调用 add_intent 交给 worker。在跑方向不重复派发。
工具必须以本轮可见工具或实际搜索结果为准，不臆造工具名；资源台账ID、事实节点ID和成功工具结果活动ID不能混用。没有合法凭据就记录登录后覆盖缺口，继续评估范围内其他可执行方向，不猜账号或跨出原授权边界。
无新方向时，纯文本说明已经检查的候选、阻塞及还缺的条件，允许宿主按防空转规则暂停/停止，不能用“完成任务”掩盖阻塞。禁止照抄“最近六小时无产出”等示例；时间和进展必须来自宿主实际态势。预算、重复读取、工具错误及连续故障保护仍然有效。`
	}
	return `
【宿主完成策略：目标达成后结束】
仅用与当前目标定义一致的真实事实/漏洞证据调用 prove_goal。目标包含范围、数量、权限或覆盖要求时，必须核对全部条件；一条发现不能证明宽泛测试已穷尽。所有目标已被证明后，宿主才会结束任务。
goal_met 只核对全部目标已经met，不能绕过逐项证明，也不能用于结束当前规划轮、表达暂不收官或处理阻塞。没有可执行的新方向时如实报告，由宿主防空转保护停止；不得编造停滞时长或完成证据。`
}
