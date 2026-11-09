package agent

import (
	"context"
	"encoding/json"
	actool "github.com/Autumn-27/norma/tool"
)

func (t *ToolSet) registerResource() actool.CoreTool {
	return writeTool("register_resource", "登记需跟踪/清理的本地文件、上传、临时账号、会话、listener，初始planned。目标资源必须明确关联本任务资产ID。只记台账，不执行创建/清理；登记不是目标达成或漏洞证明，不要填写密码或令牌。",
		obj(map[string]any{"kind": str("local_file|upload|account|session|listener"), "asset_id": intp("明确关联本任务的资产ID；local_file可省略"), "name": str("资源名称或路径，200字以内"), "cleanup": str("清理步骤、责任和验证方式，1200字以内"), "valid_hours": intp("重新核验期限，1到168小时，默认24")}, "kind", "name", "cleanup"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil || t.taskID <= 0 {
				return actool.Errorf("资源登记需要任务上下文"), nil
			}
			var a struct {
				Kind, Name, Cleanup string
				AssetID             int64 `json:"asset_id"`
				Hours               int   `json:"valid_hours"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("资源参数无效"), nil
			}
			if a.Hours == 0 {
				a.Hours = 24
			}
			r, err := t.ts.RegisterTaskResource(t.taskID, a.AssetID, a.Kind, a.Name, a.Cleanup, t.worker, a.Hours)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(r)
		})
}

func (t *ToolSet) updateResource() actool.CoreTool {
	return writeTool("update_resource", "按revision更新资源实际状态并保留历史。created/active/cleaned须引用本任务成功工具结果；cleanup_pending只表示计划，失败记failed。不会执行远端命令，不证明目标完成。",
		obj(map[string]any{"resource_id": intp("资源ID"), "revision": intp("刚读取到的版本"), "state": str("created|active|cleanup_pending|cleaned|failed"), "note": str("实际结果，1200字以内"), "evidence_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "maxItems": 8}}, "resource_id", "revision", "state", "note"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil || t.taskID <= 0 {
				return actool.Errorf("资源更新需要任务上下文"), nil
			}
			var a struct {
				ID          int64 `json:"resource_id"`
				Revision    int64 `json:"revision"`
				State, Note string
				EvidenceIDs []int64 `json:"evidence_ids"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("资源参数无效"), nil
			}
			r, err := t.ts.UpdateTaskResource(t.taskID, a.ID, a.Revision, a.State, a.Note, a.EvidenceIDs)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(r)
		})
}

func (t *ToolSet) listResources() actool.CoreTool {
	return readTool("list_resources", "分页读取本任务资源摘要（默认20项）；resource_id可读取单项清理计划。期限到达需重新核验，登记不是漏洞、代码执行或目标达成证明。", obj(map[string]any{"resource_id": intp("可选：读取单项完整台账"), "offset": intp("可选：分页偏移"), "limit": intp("可选：分页数量，默认20，最多50")}), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		if t.ts == nil || t.taskID <= 0 {
			return actool.Errorf("资源查询需要任务上下文"), nil
		}
		var a struct {
			ID            int64 `json:"resource_id"`
			Offset, Limit int
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf("资源参数无效"), nil
		}
		r, err := t.ts.TaskResources(t.taskID)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if a.ID > 0 {
			for _, item := range r {
				if item.ID == a.ID {
					return jsonResult(item)
				}
			}
			return actool.Errorf("资源不存在"), nil
		}
		if a.Offset < 0 {
			a.Offset = 0
		}
		if a.Limit <= 0 {
			a.Limit = 20
		}
		if a.Limit > 50 {
			a.Limit = 50
		}
		items := []map[string]any{}
		end := a.Offset + a.Limit
		if end < 0 || end > len(r) {
			end = len(r)
		}
		for i := a.Offset; i < end; i++ {
			v := r[i]
			items = append(items, map[string]any{"id": v.ID, "asset_id": v.AssetID, "kind": v.Kind, "name": v.Name, "state": v.State, "revision": v.Revision, "expires_at": v.ExpiresAt})
		}
		return jsonResult(map[string]any{"resources": items, "total": len(r), "offset": a.Offset, "truncated": end < len(r)})
	})
}
