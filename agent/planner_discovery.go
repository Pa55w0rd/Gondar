package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

func (t *ToolSet) addExplorationGoal() actool.CoreTool {
	return t.writeExpTool("add_exploration_goal", "在原任务范围内，根据本任务已保存的事实/发现，追加一个可验证的新探索目标。只新增并去重，保留原目标、证明和成果；不改变授权范围，不宣称漏洞成立。目标不是具体攻击步骤；保存后仍需add_intent实际派发，单独新增目标不算执行进展。",
		obj(map[string]any{
			"text":       str("一个可验证的目标，最多512字；说明需要确认的结果，不写已完成"),
			"parent_ids": map[string]any{"type": "array", "minItems": 1, "maxItems": 8, "items": map[string]any{"type": "integer", "minimum": 1}, "description": "本任务事实或发现节点ID数组，含原任务起点事实；必须实际存在，不能填意图或目标ID"},
		}, "text", "parent_ids"),
		func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var input struct {
				Text      string  `json:"text"`
				ParentIDs []int64 `json:"parent_ids"`
			}
			if err := json.Unmarshal(raw, &input); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			id, created, err := t.ts.DiscoverGoal(ctx, input.Text, input.ParentIDs)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{"saved": true, "goal_id": id, "created": created, "next_step": "仍需根据真实前提调用add_intent；目标本身不是执行或完成证明"})
		})
}
