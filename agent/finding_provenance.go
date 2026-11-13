package agent

import (
	"context"
	"errors"

	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/toolcall"
	"gondar/db"
)

func findingVerificationRefSchema() map[string]any {
	target := obj(map[string]any{"endpoint": str("已验证具体端点/文件或服务"), "method": str("真实协议/方法"), "input": str("输入位置"), "identity": str("实际身份"), "technique": str("验证技术"), "variant": str("验证变体，与原记录一致")}, "endpoint", "method", "input", "identity", "technique", "variant")
	return obj(map[string]any{"id": str("本任务已保存验证编号"), "expected_revision": map[string]any{"type": "integer"}, "target": target}, "id", "expected_revision", "target")
}

func findingEvidenceRefsSchema() map[string]any {
	return map[string]any{"type": "array", "description": "每项引用当前任务真实原件，最多16项；test或impact支撑实际测试，不用模型result/查询列表/草稿自证。无新运行但有旧工具回执或附件时选择material并如实说明，勿重跑外部目标。", "items": obj(map[string]any{"role": str("baseline|test|control|impact|supporting"), "observation": str("该原件实际显示什么，推断另写"), "activity_id": idp("已保存工具回执活动ID，非提供商call ID"), "artifact_id": idp("material通道中的真实附件ID"), "node_id": idp("material中已有事实节点，须含可继续核对的evidence_refs，纯文字不能自证")}, "role", "observation")}
}

func findingEvidenceError(ctx context.Context, err error) tool.Result {
	if errors.Is(err, db.ErrFindingEvidenceIncomplete) {
		toolcall.Mark(ctx, toolcall.EvidenceIncomplete, toolcall.NotExecuted, "finding_evidence", "finding_source_incomplete", 0)
	}
	return tool.Errorf(err.Error())
}
