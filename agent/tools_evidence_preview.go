package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
	"gondar/filepreview"
)

// ArtifactPreviewer is optional and scoped to this run's host; no process-global
// callback can route one server's evidence to a different server instance.
type ArtifactPreviewer interface {
	PreviewArtifactContent(taskID, artifactID int64, entry string) (*filepreview.Preview, error)
}

func (t *ToolSet) previewEvidence() actool.CoreTool {
	return readTool("preview_evidence", "有界读取当前任务已登记的文本、CSV、ZIP或Gzip证据，最多64KiB/60行/32列；ZIP先列目录，再传entry查看成员。只预览，不执行或修改原件。truncated=true必须明确还有未读内容，不得据预览宣称已完整核验。",
		obj(map[string]any{
			"artifact_id": map[string]any{"type": "integer", "description": "list_artifacts返回的附件ID"},
			"entry":       str("可选：ZIP内的成员名称；省略则列目录"),
		}, "artifact_id"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var args struct {
				ArtifactID int64  `json:"artifact_id"`
				Entry      string `json:"entry"`
			}
			if err := json.Unmarshal(in, &args); err != nil {
				return actool.Errorf("预览参数无效"), nil
			}
			if t.taskID <= 0 || args.ArtifactID <= 0 {
				return actool.Errorf("必须在任务上下文中指定附件ID"), nil
			}
			p, ok := t.artifactRecorder.(ArtifactPreviewer)
			if !ok {
				return actool.Errorf("当前运行环境未提供证据预览"), nil
			}
			view, err := p.PreviewArtifactContent(t.taskID, args.ArtifactID, args.Entry)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(view)
		})
}
