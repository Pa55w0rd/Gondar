package agent

import (
	"context"
	"encoding/json"
	"fmt"

	actool "github.com/Autumn-27/norma/tool"
	"gondar/db"
)

type progressNotifyContextKey struct{}

func WithProgressNotify(ctx context.Context, notify func()) context.Context {
	return context.WithValue(ctx, progressNotifyContextKey{}, notify)
}

func ProgressNotifyFromContext(ctx context.Context) func() {
	if ctx == nil {
		return nil
	}
	notify, _ := ctx.Value(progressNotifyContextKey{}).(func())
	return notify
}

// SetNotifyProgress is a fact/evidence notification, not a vulnerability report
// or a request to resume a user-paused task.
func (t *ToolSet) SetNotifyProgress(notify func()) { t.notifyProgress = notify }

func (t *ToolSet) RecordVerificationTool() actool.CoreTool { return t.recordVerification() }
func (t *ToolSet) ListVerificationsTool() actool.CoreTool  { return t.listVerifications() }

func verificationTargetSchema() map[string]any {
	return obj(map[string]any{
		"endpoint":  str("当前确切端点或服务地址，保留编码、对象及路径差异；不填全部接口等笼统范围"),
		"method":    str("实际HTTP方法或协议操作，如GET、POST、TCP"),
		"input":     str("具体输入位置与名字，如query.search、json.user_id、path.id；无输入写none"),
		"identity":  str("受控账号/角色与资源对象标签，不填密码、Cookie或令牌；匿名写anonymous"),
		"technique": str("当前具体检验技术，如对象权限、查询表达式、URL解析差异"),
		"variant":   str("具体表示法及真假对照格，如single_quote.true；省略为default，不合并其他变体"),
	}, "endpoint", "method", "input", "identity", "technique")
}

func (t *ToolSet) recordVerification() actool.CoreTool {
	return t.writeExpTool("record_verification",
		"把单个端点×方法×输入×身份/对象×技术×变体的验证状态增量记入本任务事实图，不发送请求。"+
			"state区分not_tested未测、blocked被防护阻断、missing_prerequisite缺前提、negative有效阴性、confirmed影响已证实、inconclusive证据不足。"+
			"一次阴性仅覆盖当前格，不能排除同端点其他参数/身份/表示法或整类漏洞。"+
			"证据先用实际工具结果、record_fact或save_evidence保存，再引用本任务activity_id/node_id/artifact_id；缺回执不能凑造。"+
			"negative需要正常baseline、实际test和control对照；confirmed需要baseline、test和真实impact，并明确prerequisites_met=true。"+
			"这里只记录验证事实，不代替report_finding创建漏洞。缺账号、WAF/限流、超时、会话失效应保留受阻/不确定。"+
			"首次返回稳定id和revision=1；更新前list_verifications(id)取最新expected_revision。旧修订保留，相同观察/证据含义重试或仅时间变化不算进展；改变已证实/阴性结论必须解释change_reason。",
		verificationInputSchema(obj(map[string]any{
			"target":            verificationTargetSchema(),
			"state":             map[string]any{"type": "string", "enum": []string{"not_tested", "blocked", "missing_prerequisite", "negative", "confirmed", "inconclusive"}},
			"summary":           str("简短展示摘要；不靠修改标题制造新进展"),
			"observation":       str("实际观察及仅对当前格成立的结论，不粘大段原始响应；非未测状态必填"),
			"prerequisites_met": map[string]any{"type": "boolean", "description": "账号/登录态、目标对象、环境和对照已有效；阴性和证实状态必须为true"},
			"evidence": map[string]any{"type": "array", "maxItems": 16, "items": obj(map[string]any{
				"role":        map[string]any{"type": "string", "enum": []string{"baseline", "test", "control", "impact", "blocker"}},
				"observation": str("本项证据实际证明的简短观察"),
				"activity_id": idp("已保存的本任务tool_result/result活动ID，三个来源ID只填一个"),
				"node_id":     idp("本任务带evidence的record_fact节点，不能用验证结论自证"),
				"artifact_id": idp("save_evidence已登记的本任务附件ID"),
			}, "role", "observation")},
			"blocker":               str("实际防护阻断或环境故障原因；原因不明用inconclusive，不认定整类安全"),
			"missing_prerequisites": map[string]any{"type": "array", "maxItems": 16, "items": map[string]any{"type": "string"}, "description": "明确缺失的账号、登录态、对象、工具或环境"},
			"intent_id":             idp("本任务当前意图ID；worker省略时从运行上下文取"),
			"expected_revision":     idp("首次可省略/0；修改已有记录必须传最新revision，相同内容重试保持幂等"),
			"change_reason":         str("改变已有证实或阴性结论时解释原因；旧证据继续保留"),
			"observed_at":           str("可选RFC3339观察时间；仅刷新时间不新增修订或刷新进展"),
		}, "target", "state", "summary")),
		func(ctx context.Context, input json.RawMessage) (actool.Result, error) {
			var in db.VerificationUpdate
			if err := json.Unmarshal(input, &in); err != nil {
				return actool.Errorf(fmt.Sprintf("验证记账参数无效：%v", err)), nil
			}
			if in.IntentID == 0 && t.ownerNode > 0 {
				owner, err := t.ts.GetNode(t.ownerNode)
				if err != nil {
					return actool.Errorf(fmt.Sprintf("读取当前意图失败：%v", err)), nil
				}
				if owner != nil && owner.Kind == db.KindIntent {
					in.IntentID = owner.ID
				}
			}
			result, err := t.ts.UpsertVerification(ctx, in, t.worker)
			if err != nil {
				return verificationRepairResult(in.State, err), nil
			}
			if result.Changed && result.Record.Progress {
				t.writes.Facts++
				if t.notifyProgress != nil {
					t.notifyProgress()
				} else if t.notify != nil {
					t.notify()
				}
			}
			return jsonResult(map[string]any{
				"saved": true, "receipt_type": "verification",
				"changed": result.Changed, "id": result.Record.ID, "node_id": result.Record.NodeID,
				"revision": result.Record.Revision, "previous_node_id": result.Record.PreviousNodeID,
				"content_digest": result.Record.ContentDigest, "state": result.Record.State,
				"progress": result.Changed && result.Record.Progress,
				"scope":    "仅当前验证格；未登记的其他条件仍未知；旧修订证据保留",
			})
		})
}

func (t *ToolSet) listVerifications() actool.CoreTool {
	return t.readExpTool("list_verifications",
		"读取本任务验证账本，默认每个稳定id只取最新修订，统计仅指已登记格，不代表全部测试面覆盖。"+
			"默认返回紧凑状态/范围/观察，带id时读取单格完整证据与revision供增量更新；include_history=true需id，返回历史摘要与node_id，可用node_detail读旧证据。"+
			"not_tested、blocked、missing_prerequisite、inconclusive都不能当安全或已完成；单格negative不能扩大成整类安全。",
		obj(map[string]any{
			"id":              str("record_verification返回的稳定id；指定后可读取一格的最新证据"),
			"state":           str("可选按not_tested/blocked/missing_prerequisite/negative/confirmed/inconclusive过滤"),
			"endpoint":        str("可选精确匹配端点，保留编码和路径差异"),
			"before":          idp("分页游标，使用上一页next_before"),
			"limit":           intp("默认10，最多50；历史最多20"),
			"include_history": map[string]any{"type": "boolean", "description": "仅对指定id读取修订历史，默认false"},
		}),
		func(ctx context.Context, input json.RawMessage) (actool.Result, error) {
			var in struct {
				ID             string `json:"id"`
				State          string `json:"state"`
				Endpoint       string `json:"endpoint"`
				Before         int64  `json:"before"`
				Limit          int    `json:"limit"`
				IncludeHistory bool   `json:"include_history"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return actool.Errorf(fmt.Sprintf("验证列表参数无效：%v", err)), nil
			}
			if in.Limit <= 0 {
				in.Limit = 10
			}
			if in.Limit > 50 {
				in.Limit = 50
			}
			if in.IncludeHistory && in.Limit > 20 {
				in.Limit = 20
			}
			if in.ID != "" && !in.IncludeHistory {
				in.Limit = 1
			}
			page, err := t.ts.ListVerifications(ctx, db.VerificationFilter{ID: in.ID, State: in.State,
				Endpoint: in.Endpoint, Before: in.Before, Limit: in.Limit, IncludeHistory: in.IncludeHistory})
			if err != nil {
				return actool.Errorf(fmt.Sprintf("读取验证记录失败：%v", err)), nil
			}
			if in.ID != "" && !in.IncludeHistory {
				return jsonResult(page)
			}
			cases := make([]map[string]any, 0, len(page.Cases))
			for _, record := range page.Cases {
				observation := []rune(record.Observation)
				if len(observation) > 300 {
					observation = append(observation[:300], '…')
				}
				cases = append(cases, map[string]any{"id": record.ID, "node_id": record.NodeID,
					"revision": record.Revision, "target": record.Target, "state": record.State, "node_state": record.NodeState,
					"observation": string(observation), "evidence_count": len(record.Evidence),
					"progress": record.Progress})
			}
			return jsonResult(map[string]any{"cases": cases, "total": page.Total,
				"recorded_cases": page.RecordedCases, "latest_states": page.LatestStates,
				"has_more": page.HasMore, "next_before": page.NextBefore,
				"scope": page.Scope, "unrecorded_cases": "unknown"})
		})
}
