package agent

import (
	"strings"

	actool "github.com/Autumn-27/norma/tool"
)

// sceneToolGuidance is appended after final tool resolution. Guidance never
// grants a tool, changes its schema, or substitutes for task/approval checks.
func sceneToolGuidance(role string, tools []actool.CoreTool) string {
	if len(tools) == 0 || (role != "planner" && role != "worker" && role != "mainagent") {
		return ""
	}
	has := make(map[string]bool, len(tools))
	for _, tool := range tools {
		has[tool.Name()] = true
	}
	var out strings.Builder
	out.WriteString("\n\n**场景核验卡与工具前置**：遵守本任务描述中选定的场景、目标清单和预算。核验卡写清对象、前置、假设、最小证明、停止条件和结果出处；批量任务逐目标记录，不能把同类方法复用当成整批已验证。")
	if role == "planner" {
		out.WriteString("你负责把卡拆为少量有前置依赖的意图；等待中的依赖不重复派发，未执行的卡不能计为已测。")
	} else if role == "worker" {
		out.WriteString("你只执行当前意图的卡；前置缺失先记录阻塞，预算将尽时保存已有事实与未验证项，不用重复请求凑数量。")
	}
	if has["SearchExtraTools"] && has["ExecuteExtraTool"] {
		out.WriteString("需要延迟工具时先用 SearchExtraTools 确认名称/参数，再用 ExecuteExtraTool 调用已找到的工具；搜索不到就记录能力缺口，不虚构名称。")
	}
	if has["Read"] && has["Grep"] {
		out.WriteString("源码/文件先用 Grep 定位再用 Read 核对上下文，引用实际读取的路径和行号；不要只凭命中词判断漏洞。")
	}
	if has["traffic_search"] && has["traffic_get"] {
		out.WriteString("HTTP验证优先用 traffic_search 和 traffic_get 复用/核对已有流量；域名或时间接近不等于同一问题的证明。")
	}
	if has["report_finding"] {
		out.WriteString("report_finding 只提交已确认影响及证据；缺工具、不可达或权限不足是未验证原因，不是已修复/安全结论。")
	}
	if has["recall_lessons"] {
		out.WriteString("新任务或遇到相似工具错误时按需 recall_lessons；历史经验只作待核验建议，不能当当前目标证据，也不能变更授权、审批或输出平台密钥。")
	}
	if has["preview_evidence"] {
		out.WriteString("大文件或归档优先用 preview_evidence 有界查看，truncated 表示未完整读取，不得据片段宣称全量已核验。")
	}
	if has["register_resource"] && has["update_resource"] {
		out.WriteString("临时账号、上传、会话或listener用register_resource/update_resource保留归属和清理计划；成功状态须有实际工具结果，登记不能代替目标成功证明，收尾列出未清理项。")
	}
	return out.String()
}
