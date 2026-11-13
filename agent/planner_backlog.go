package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/toolcall"
	"gondar/db"
)

func (t *ToolSet) retireIntents() actool.CoreTool {
	return t.writeExpTool("retire_open_intents", "仅退役本任务尚未领取、已被新事实否定或被存活新意图替代的旧方向，一次最多6条。必须引用真实事实/发现或替代意图并解释原因。保留全部节点、成果、血缘和会话；不杀运行中的worker，不以意图年龄或没有发现漏洞为由清队列。退役本身不是探索进展。",
		obj(map[string]any{"items": map[string]any{"type": "array", "minItems": 1, "maxItems": db.MaxIntentRetireBatch, "items": obj(map[string]any{
			"intent_id": idp("本任务open意图ID"), "reason": str("新事实如何否定原前提，或替代意图如何覆盖它"),
			"evidence_id": idp("本任务现存fact/finding节点ID，与superseded_by二选一"), "superseded_by": idp("仍open/running/paused的替代意图ID"),
		}, "intent_id", "reason")}}, "items"),
		func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var input struct {
				Items []db.IntentRetirement `json:"items"`
			}
			if err := json.Unmarshal(raw, &input); err != nil {
				toolcall.Mark(ctx, toolcall.SchemaInvalid, toolcall.NotExecuted, "intent_retirement", "invalid_input", 0)
				return actool.Errorf("退役参数无效：" + err.Error()), nil
			}
			ids, err := t.ts.RetireOpenIntents(ctx, input.Items)
			if err != nil {
				switch {
				case errors.Is(err, db.ErrIntentStateConflict):
					toolcall.Mark(ctx, toolcall.SchedulerDeferred, toolcall.NotExecuted, "intent_retirement", "claim_conflict", 0)
				case errors.Is(err, db.ErrIntentRetirementInvalid):
					toolcall.Mark(ctx, toolcall.SchemaInvalid, toolcall.NotExecuted, "intent_retirement", "invalid_basis", 0)
				case errors.Is(err, sql.ErrNoRows):
					toolcall.Mark(ctx, toolcall.PreconditionMissing, toolcall.NotExecuted, "intent_retirement", "basis_not_found", 0)
				}
				return actool.Errorf("未退役：" + err.Error() + "；请重读当前意图，不停止已领取的worker"), nil
			}
			return jsonResult(map[string]any{"saved": true, "retired_ids": ids, "exploration_progress": false, "preserved": "全部原成果、节点、血缘及会话"})
		})
}
