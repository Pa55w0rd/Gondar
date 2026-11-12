package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	actool "github.com/Autumn-27/norma/tool"
	"gondar/db"
)

// ArtifactRecorder 由 host（server）实现：校验路径边界与限额、复制到任务附件
// 目录并落库。agent 只负责"把工作目录里的文件交出去"，不直接写附件库。
type ArtifactRecorder interface {
	RecordArtifact(taskID, findingID int64, runDir, relPath, name, kind, sourceURL, note string) (*db.TaskArtifact, error)
}

// ArtifactProgressRecorder returns an atomic host receipt. Only newly saved
// content counts as progress; duplicate registration or metadata edits do not.
type ArtifactProgressRecorder interface {
	RecordArtifactWithProgress(taskID, findingID int64, runDir, relPath, name, kind, sourceURL, note string) (*db.TaskArtifact, bool, error)
}

// ListTaskArtifacts 是 host 注入的附件列表读取（agent 工具用）。nil = 不可用。
var ListTaskArtifacts func(taskID string) ([]db.TaskArtifact, error)

// FindingTaskID 由 host 注入：按漏洞 ID 反查所属任务（v0.4.20）。
// reporter 的 chat 工具集没有任务上下文，靠模型传 finding_id 定位任务。
var FindingTaskID func(findingID int64) (string, error)

// artifactGuidance 注入 worker system prompt：最小样本、原件保真与明确证据关系。
const artifactGuidance = `

━━ 证据附件与最小样本 ━━
- 仅对当前任务明确对象与操作范围取证。企业归属、同根域名、发现凭据或拿到访问点，不会自动增加测试范围；不要默认声称第一方或官方递交。
- 保存实际能支持影响判断的最小材料：来源、时间、请求/响应或代码位置、控制组、实际状态变化。版本命中、200响应、扫描器输出或调用尝试本身不证明漏洞成立。
- 可用浏览器时截取关键页面并用 save_evidence 登记；无法截图或非HTTP场景保留相应工具/代码证据，写清缺口，不伪造截图或为凑数量多取个人数据。
- 只有任务明确需要并允许时才保存受控原件。已保存原件保持字节和SHA不变；分享、报告与通用经验使用最小必要摘录或单独脱敏副本，并引用原件ID/哈希，不覆盖原件制造新证据。
- 身份证件、账号、口令、Cookie和令牌不强制明文铺进普通fact/经验/报告。认证设置和受控附件可保存任务明确需要的值；普通描述记录凭据引用、适用主机、验证结果，平台自身API key不得进入目标证据。
- 存储桶/文件读取问题优先使用已授权的测试文件、对象清单或最小样本证明，不默认下载5–9张证件、收据或整个数据集。样本数、可见记录数与推测总量分别说明；截断/不可读不代表安全。
- save_evidence的name、source_url、note写清来源与证明关系；只有材料确实属于某个finding时才传finding_id，多漏洞混合或不确定时保持任务级材料，后续人工核对。
- 附件上限仍为300件/500MB；大文件先用可用的preview_evidence有界查看，truncated不能当全量已读。报告用artifact:<id>引用已登记附件，图片缩放/拼图只作展示副本，保持原件可复核。
- 临时文件、账号、会话等在工具可用时登记资源和清理计划；收尾列出已清理/失败/未确认项，不把台账回执当真实操作成功。
`

// saveEvidence 把工作目录里的文件登记为任务附件。
func (t *ToolSet) saveEvidence() actool.CoreTool {
	return writeTool("save_evidence",
		"把工作目录里的证据文件登记为任务附件（截图 / 数据样本 / 日志 / HTML / JSON），随报告导出成证据包。仅保存授权所需的最小材料，受控原件保真并可引用独立脱敏副本（附件上限300件/500MB）；不默认全量采集。同一文件重复登记会去重。",
		obj(map[string]any{
			"path":       str("工作目录内的相对路径（如 shots/login-userlist.png）"),
			"name":       str("可选：展示名（含扩展名）；省略用文件名"),
			"kind":       str("可选：image|data|log|html|auto（默认按扩展名推断）"),
			"source_url": str("可选：采集来源 URL"),
			"note":       str("说明：证明什么（截图会作为报告图注，务必具体）"),
			"finding_id": map[string]any{"type": "integer", "description": "可选：这块证据证明哪个漏洞（report_finding 返回的 finding_id）。带上后该漏洞详情页与单漏洞报告会展示这张图"},
		}, "path"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Path, Name, Kind, SourceURL, Note string
				FindingID                         int64 `json:"finding_id"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if t.artifactRecorder == nil {
				return actool.Errorf("证据附件存储不可用（当前运行环境未启用）"), nil
			}
			if t.taskID <= 0 {
				return actool.Errorf("save_evidence 需要任务上下文"), nil
			}
			if t.runDir == "" {
				return actool.Errorf("save_evidence 需要当前运行目录（内部未注入）"), nil
			}
			var rec *db.TaskArtifact
			var err error
			var created bool
			if host, ok := t.artifactRecorder.(ArtifactProgressRecorder); ok {
				rec, created, err = host.RecordArtifactWithProgress(t.taskID, a.FindingID, t.runDir, a.Path, a.Name, a.Kind, a.SourceURL, a.Note)
			} else {
				rec, err = t.artifactRecorder.RecordArtifact(t.taskID, a.FindingID, t.runDir, a.Path, a.Name, a.Kind, a.SourceURL, a.Note)
			}
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if rec == nil || rec.ID <= 0 {
				return actool.Errorf("证据存储没有返回有效的附件回执；未计入进展"), nil
			}
			progress := false
			if created {
				key := rec.SHA256
				if key == "" {
					key = "id:" + strconv.FormatInt(rec.ID, 10)
				}
				if t.artifactProgress == nil {
					t.artifactProgress = make(map[string]bool)
				}
				if !t.artifactProgress[key] {
					t.artifactProgress[key] = true
					t.writes.Artifacts++
					progress = true
					if t.notifyProgress != nil {
						t.notifyProgress()
					}
				}
			}
			out, _ := json.Marshal(map[string]any{
				"artifact_id": rec.ID, "name": rec.Name, "kind": rec.Kind,
				"size_bytes": rec.SizeBytes, "note": rec.Note, "new_content": progress,
			})
			return actool.Text(fmt.Sprintf("evidence saved: artifact %d\n%s\n报告撰写时可用 ![说明](artifact:%d) 引用这张图。", rec.ID, out, rec.ID)), nil
		})
}

// listArtifacts 列出本任务已登记的证据附件（worker 核对、reporter 引用用）。
// reporter 没有任务上下文，可传 finding_id 让工具反查所属任务（v0.4.20）。
func (t *ToolSet) listArtifacts() actool.CoreTool {
	return readTool("list_artifacts",
		"列出本任务已登记的证据附件（id/名称/类型/大小/说明/来源）。撰写报告时用 ![图注](artifact:<id>) 把图片插进报告。reporter 可传 finding_id 以定位该漏洞所属任务的附件。",
		obj(map[string]any{
			"finding_id": map[string]any{"type": "integer", "description": "可选：当前漏洞 ID；reporter 用它定位所属任务的附件清单"},
		}), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				FindingID int64 `json:"finding_id"`
			}
			_ = json.Unmarshal(in, &a)
			taskID := t.taskID
			if taskID <= 0 && a.FindingID > 0 && FindingTaskID != nil {
				if tid, err := FindingTaskID(a.FindingID); err == nil && tid != "" {
					if n, err := strconv.ParseInt(tid, 10, 64); err == nil {
						taskID = n
					}
				}
			}
			if ListTaskArtifacts == nil || taskID <= 0 {
				return actool.Text("[]"), nil
			}
			list, err := ListTaskArtifacts(fmt.Sprint(taskID))
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out, _ := json.Marshal(map[string]any{"artifacts": list})
			return actool.Text(string(out)), nil
		})
}
