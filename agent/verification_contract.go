package agent

import (
	"fmt"

	actool "github.com/Autumn-27/norma/tool"
)

func verificationInputSchema(schema map[string]any) map[string]any {
	props := schema["properties"].(map[string]any)
	evidence := props["evidence"].(map[string]any)
	item := evidence["items"].(map[string]any)
	ids := item["properties"].(map[string]any)
	var choices []any
	for _, name := range []string{"activity_id", "node_id", "artifact_id"} {
		ids[name].(map[string]any)["minimum"] = 1
		choices = append(choices, map[string]any{"required": []string{name}})
	}
	item["oneOf"] = choices
	condition := func(states []string, required []string, properties map[string]any) map[string]any {
		then := map[string]any{"required": required}
		if properties != nil {
			then["properties"] = properties
		}
		return map[string]any{"if": map[string]any{"properties": map[string]any{"state": map[string]any{"enum": states}}}, "then": then}
	}
	schema["allOf"] = []any{
		condition([]string{"blocked", "missing_prerequisite", "negative", "confirmed", "inconclusive"}, []string{"observation"}, map[string]any{"observation": map[string]any{"minLength": 1}}),
		condition([]string{"negative", "confirmed"}, []string{"prerequisites_met", "evidence"}, map[string]any{"prerequisites_met": map[string]any{"const": true}, "evidence": map[string]any{"minItems": 1}}),
		condition([]string{"blocked"}, []string{"blocker", "evidence"}, map[string]any{"blocker": map[string]any{"minLength": 1}, "evidence": map[string]any{"minItems": 1}}),
		condition([]string{"missing_prerequisite"}, []string{"missing_prerequisites"}, map[string]any{"missing_prerequisites": map[string]any{"minItems": 1}}),
	}
	schema["examples"] = []any{map[string]any{
		"target": map[string]any{"endpoint": "https://owned.invalid/api/items", "method": "GET", "input": "none", "identity": "anonymous", "technique": "身份前提核对"},
		"state":  "missing_prerequisite", "summary": "等待有效测试账号", "observation": "当前只有匿名响应，登录后功能尚未验证",
		"missing_prerequisites": []string{"有效测试账号或登录态"},
		"evidence":              []any{map[string]any{"role": "blocker", "node_id": 123, "observation": "示例编号必须替换为record_fact实际返回的本任务节点编号"}},
	}}
	return schema
}

func verificationRepairResult(state string, err error) actool.Result {
	roles := []string{}
	if state == "negative" {
		roles = []string{"baseline", "test", "control"}
	}
	if state == "confirmed" {
		roles = []string{"baseline", "test", "impact"}
	}
	result, _ := jsonResult(map[string]any{
		"saved": false, "state_unchanged": true, "receipt_type": "verification",
		"error": fmt.Sprintf("验证记录未更改：%v", err), "required_evidence_roles": roles,
		"repair": "每项evidence只填一种真实来源：record_fact返回的id填node_id；save_evidence返回的id填artifact_id；活动详情返回的活动编号才填activity_id。意图ID不能充当活动ID。修正缺失字段后重试一次；证据不足时用inconclusive或missing_prerequisite，不编造基线、对照或编号。只有saved=true或查询到对应修订才可称记账成功。",
	})
	result.IsError = true
	return result
}

const verificationReceiptGuidance = `

【验证与交付回执】record_verification失败表示未保存，不能在总结中称已登记、阴性闭环或全部覆盖。record_fact的id是node_id，附件id是artifact_id，只有实际活动详情编号才是activity_id，三类编号不能互换。negative需要前提有效及baseline/test/control，confirmed另需真实impact；没有这些材料就如实记inconclusive、blocked或missing_prerequisite。错误只修正一次，不循环猜参数。总结引用真实保存编号和仍未验证步骤；已有证据可继续增量处理，不能因done、零产出或旧目标met扩大阴性范围。
`
