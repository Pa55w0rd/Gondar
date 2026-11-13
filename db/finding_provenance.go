package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/norma/toolcall"
)

var ErrFindingEvidenceIncomplete = errors.New("漏洞出处尚未满足自动登记要求")

type FindingVerificationRef struct {
	ID               string             `json:"id"`
	ExpectedRevision int64              `json:"expected_revision"`
	Target           VerificationTarget `json:"target"`
}

// Automatic and RunID are assigned by the host, never unmarshaled from a tool's
// input. Mode describes source provenance, not proof that a vulnerability exists.
type FindingProvenanceInput struct {
	Mode           string                    `json:"mode"`
	Verification   *FindingVerificationRef   `json:"verification_ref,omitempty"`
	Evidence       []VerificationEvidence    `json:"evidence_refs,omitempty"`
	TrafficRefs    []TrafficRef              `json:"-"`
	CheckedTraffic []PreparedTrafficEvidence `json:"-"`
}

type FindingSourceReceipt struct {
	Role         string `json:"role"`
	Kind         string `json:"kind"`
	ActivityID   int64  `json:"activity_id,omitempty"`
	ArtifactID   int64  `json:"artifact_id,omitempty"`
	Digest       string `json:"sha256"`
	RunID        string `json:"run_id,omitempty"`
	InvocationID string `json:"invocation_id,omitempty"`
	TrafficID    string `json:"traffic_id,omitempty"`
}

type FindingProvenance struct {
	Version         int                     `json:"version"`
	Mode            string                  `json:"mode"`
	Producer        string                  `json:"producer"`
	RunID           string                  `json:"run_id,omitempty"`
	Verification    *FindingVerificationRef `json:"verification_ref,omitempty"`
	Sources         []FindingSourceReceipt  `json:"sources"`
	MaterialFactIDs []int64                 `json:"material_fact_ids,omitempty"`
	CheckedAt       time.Time               `json:"checked_at"`
	Digest          string                  `json:"digest"`
}

func incompleteFinding(format string, args ...any) error {
	return fmt.Errorf("%w：%s；请保存待核验fact/hint，引用已有原件补证或继续其它方向，不要为凑出处重放外部操作", ErrFindingEvidenceIncomplete, fmt.Sprintf(format, args...))
}

// validateFindingProvenanceTx validates current task ownership and immutable
// source receipts inside the finding transaction. It never interprets target
// prose as a host runtime fact or infers exploit success from an HTTP status.
func validateFindingProvenanceTx(ctx context.Context, tx *sql.Tx, in RecordFindingInput, prepared []PreparedTrafficEvidence) (*FindingProvenance, error) {
	if !in.Automatic {
		return nil, nil
	}
	if in.TaskID <= 0 || in.ExplorationID <= 0 {
		return nil, incompleteFinding("自动登记缺少宿主任务归属")
	}
	source := in.Provenance
	if source == nil {
		return nil, incompleteFinding("缺少verification_ref或evidence_refs")
	}
	mode := source.Mode
	if mode == "" {
		mode = "runtime"
	}
	if mode != "runtime" && mode != "material" {
		return nil, incompleteFinding("出处模式须为runtime或material")
	}
	if len(source.Evidence) > 16 || len(source.CheckedTraffic) > 16 {
		return nil, incompleteFinding("每次最多16项出处")
	}
	out := &FindingProvenance{Version: 1, Mode: mode, Producer: in.Worker, RunID: in.ExecutionRunID, CheckedAt: time.Now().UTC()}
	refs := append([]VerificationEvidence(nil), source.Evidence...)
	if ref := source.Verification; ref != nil {
		if ref.ID == "" || ref.ExpectedRevision < 1 {
			return nil, incompleteFinding("验证编号与expected_revision无效")
		}
		var raw []byte
		var nodeID int64
		var created time.Time
		var state string
		err := tx.QueryRowContext(ctx, `SELECT id,payload,created_at,state FROM exploration_nodes WHERE exploration_id=$1 AND kind='fact' AND payload->'verification'->>'id'=$2 ORDER BY id DESC LIMIT 1 FOR SHARE`, in.ExplorationID, ref.ID).Scan(&nodeID, &raw, &created, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, incompleteFinding("验证记录不属于当前任务")
		}
		if err != nil {
			return nil, err
		}
		record, err := decodeVerification(nodeID, raw, created, state)
		if err != nil {
			return nil, err
		}
		if record.Revision != ref.ExpectedRevision {
			return nil, incompleteFinding("验证修订已改变，请读取最新版本")
		}
		if record.Target != ref.Target {
			return nil, incompleteFinding("验证端点、方法、输入、身份或技术标签不匹配")
		}
		if record.State != VerificationConfirmed || record.NodeState == "dismissed" || !record.PrerequisitesMet || record.Blocker != "" || len(record.MissingPrerequisites) > 0 {
			return nil, incompleteFinding("验证尚未证实或仍缺前提")
		}
		if in.IntentID > 0 && record.IntentID > 0 && record.IntentID != in.IntentID {
			return nil, incompleteFinding("验证属于其它意图")
		}
		out.Verification = ref
		refs = append(refs, record.Evidence...)
	}
	if len(refs) > 32 {
		return nil, incompleteFinding("合并后的出处过多，请引用具体验证修订")
	}
	var expandErr error
	refs, out.MaterialFactIDs, expandErr = expandFindingFactSources(ctx, tx, in.ExplorationID, mode, refs)
	if expandErr != nil {
		return nil, expandErr
	}
	proof := false
	seen := map[string]bool{}
	for _, ref := range refs {
		if ref.Role != "baseline" && ref.Role != "test" && ref.Role != "control" && ref.Role != "impact" && ref.Role != "supporting" {
			return nil, incompleteFinding("证据用途无效")
		}
		if ref.ActivityID <= 0 && ref.ArtifactID <= 0 {
			return nil, incompleteFinding("事实文字不能自证影响，请引用原始activity_id或artifact_id")
		}
		if ref.ActivityID > 0 && ref.ArtifactID > 0 {
			return nil, incompleteFinding("一项出处只能引用一个原件")
		}
		receipt := FindingSourceReceipt{Role: ref.Role, ActivityID: ref.ActivityID, ArtifactID: ref.ArtifactID}
		if ref.ActivityID > 0 {
			var kind, name, toolUseID, detail string
			var metadata []byte
			var failed bool
			var intent sql.NullInt64
			err := tx.QueryRowContext(ctx, `SELECT kind,COALESCE(tool,''),COALESCE(tool_use_id,''),COALESCE(detail,summary,''),metadata,is_error,node_id FROM activity WHERE id=$1 AND exploration_id=$2 FOR SHARE`, ref.ActivityID, in.ExplorationID).Scan(&kind, &name, &toolUseID, &detail, &metadata, &failed, &intent)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, incompleteFinding("活动不属于当前任务")
			}
			if err != nil {
				return nil, err
			}
			if failed || (kind != "tool_result" && kind != "tool_late_result") || strings.TrimSpace(detail) == "" || !findingRuntimeProofTool(name) {
				return nil, incompleteFinding("模型文字、查询列表、草稿或失败结果不能作为实际影响原件")
			}
			if in.IntentID > 0 && intent.Valid && intent.Int64 != in.IntentID {
				return nil, incompleteFinding("活动来自其它意图")
			}
			var m struct {
				Origin   string            `json:"activity_origin"`
				Identity toolcall.Identity `json:"tool_call_identity"`
				Outcome  toolcall.Outcome  `json:"tool_outcome"`
			}
			_ = json.Unmarshal(metadata, &m)
			runtime := m.Origin == "runtime" && m.Identity.Valid() && m.Identity == m.Outcome.Identity && m.Identity.ToolUseID == toolUseID && m.Identity.ToolName == name && m.Outcome.ExecutionState == toolcall.Executed && m.Outcome.Kind == toolcall.ExecutedSuccess && ((kind == "tool_result" && m.Outcome.Phase == "result") || (kind == "tool_late_result" && m.Outcome.Phase == "late_receipt"))
			// Selecting material cannot launder a known host denial, partial or
			// unknown execution into proof. The compatibility path is for old
			// successful raw receipts that predate trusted metadata.
			if m.Outcome.Version == toolcall.Version && (m.Outcome.ExecutionState != toolcall.Executed || m.Outcome.Kind != toolcall.ExecutedSuccess) {
				return nil, incompleteFinding("该宿主回执明确未执行或结果未知，material不能改变已知执行事实；请引用其它已保存原件")
			}
			if mode == "runtime" && !runtime {
				return nil, incompleteFinding("旧记录、归档材料或未知执行状态不能冒充当前运行证据；可明确选择material通道复核已有原件")
			}
			receipt.Kind = "material_activity"
			if runtime {
				receipt.Kind = "runtime_activity"
				receipt.RunID = m.Identity.RunID
				receipt.InvocationID = m.Identity.InvocationID
			}
			hash := sha256.Sum256([]byte(detail))
			receipt.Digest = hex.EncodeToString(hash[:])
		} else {
			if mode != "material" {
				return nil, incompleteFinding("附件通过material通道登记，不能声称是新运行")
			}
			var size int64
			var hash string
			var intent sql.NullInt64
			err := tx.QueryRowContext(ctx, `SELECT COALESCE(sha256,''),size_bytes,intent_id FROM task_artifacts WHERE id=$1 AND task_id=$2 FOR SHARE`, ref.ArtifactID, fmt.Sprint(in.TaskID)).Scan(&hash, &size, &intent)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, incompleteFinding("附件不属于当前任务")
			}
			if err != nil {
				return nil, err
			}
			decoded, err := hex.DecodeString(hash)
			if err != nil || len(decoded) != 32 || size <= 0 {
				return nil, incompleteFinding("附件缺少原件摘要或内容为空")
			}
			if in.IntentID > 0 && intent.Valid && intent.Int64 != in.IntentID {
				return nil, incompleteFinding("附件来自其它意图")
			}
			receipt.Kind, receipt.Digest = "material_artifact", hash
		}
		key := fmt.Sprintf("%s:%d:%d:%s", receipt.Kind, receipt.ActivityID, receipt.ArtifactID, receipt.Role)
		if seen[key] {
			continue
		}
		seen[key] = true
		out.Sources = append(out.Sources, receipt)
		if ref.Role == "test" || ref.Role == "impact" {
			proof = true
		}
	}
	allTraffic := append(append([]PreparedTrafficEvidence(nil), prepared...), source.CheckedTraffic...)
	for _, item := range allTraffic {
		if item.Snapshot.ReqHash == "" || item.Snapshot.RespHash == "" {
			continue
		}
		key := "traffic:" + item.Ref.TrafficID + ":" + item.Ref.Role
		if seen[key] {
			continue
		}
		seen[key] = true
		hash := sha256.Sum256([]byte(item.Snapshot.ReqHash + ":" + item.Snapshot.RespHash))
		out.Sources = append(out.Sources, FindingSourceReceipt{Role: item.Ref.Role, Kind: "captured_traffic", TrafficID: item.Ref.TrafficID, Digest: hex.EncodeToString(hash[:])})
		// The global capture store has no task-owner column. A checked packet
		// is supplementary evidence, never sufficient on its own to establish
		// current-task execution or impact from a host/time match.
	}
	if !proof {
		return nil, incompleteFinding("缺少本任务test/impact执行或材料原件；全局抓包与基线仅作补充，不能按域名/时间推定归属")
	}
	sort.Slice(out.Sources, func(i, j int) bool {
		a, _ := json.Marshal(out.Sources[i])
		b, _ := json.Marshal(out.Sources[j])
		return string(a) < string(b)
	})
	stable, _ := json.Marshal(struct {
		Severity, Summary, Evidence string
		Verification                *FindingVerificationRef
		Sources                     []FindingSourceReceipt
	}{in.Severity, in.Summary, in.Evidence, out.Verification, out.Sources})
	hash := sha256.Sum256(stable)
	out.Digest = hex.EncodeToString(hash[:])
	return out, nil
}

func provenanceReplayJSON(p *FindingProvenance) []byte {
	raw, _ := json.Marshal([]map[string]string{{"digest": p.Digest}})
	return raw
}

func findingRuntimeProofTool(name string) bool {
	switch name {
	case "Write", "Edit", "NotebookEdit", "Read", "Glob", "Grep", "TodoWrite", "ToolSearch", "WebSearch", "web_search", "SearchFetch", "traffic_search", "traffic_get", "graph_overview", "node_detail", "get_task_node_detail", "report_finding", "save_evidence":
		return false
	case "add_exploration_goal", "record_planner_wait", "retire_open_intents", "frontend_inventory",
		"list_intents", "list_findings", "list_facts", "list_goals", "list_assets", "list_untested_assets", "asset_neighbors",
		"add_intent", "prove_goal", "goal_met", "record_fact", "record_verification", "list_verifications",
		"set_goals", "set_constraints", "add_hint", "add_task_hint", "kill_work", "steer_work",
		"get_worker_output", "get_worker_trace", "search_all_worker_traces", "list_worker_traces",
		"list_artifacts", "preview_evidence", "expand_digest", "propose_lesson", "recall_lessons",
		"register_resource", "update_resource", "list_resources", "insert_assets", "upsert_asset", "search_recent_critical":
		return false
	}
	// An unknown MCP/external tool is not a local lookup merely because its
	// name starts with get_/query_. Its real receipt and interpretation still
	// need the same ownership, identity, source and evidence checks above.
	return name != ""
}

func expandFindingFactSources(ctx context.Context, tx *sql.Tx, expID int64, mode string, refs []VerificationEvidence) ([]VerificationEvidence, []int64, error) {
	queue := append([]VerificationEvidence(nil), refs...)
	var out []VerificationEvidence
	var facts []int64
	seen := map[int64]bool{}
	for index := 0; index < len(queue); index++ {
		if len(queue) > 48 {
			return nil, nil, incompleteFinding("事实引用链过长，请直接引用原始活动或附件")
		}
		ref := queue[index]
		if ref.NodeID <= 0 {
			out = append(out, ref)
			continue
		}
		if mode != "material" || ref.ActivityID > 0 || ref.ArtifactID > 0 {
			return nil, nil, incompleteFinding("事实引用仅通过material通道解析原件，不能自报运行身份")
		}
		if seen[ref.NodeID] {
			return nil, nil, incompleteFinding("事实出处存在重复或循环，请直接给原件编号")
		}
		seen[ref.NodeID] = true
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT payload FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='fact' AND state='confirmed' FOR SHARE`, ref.NodeID, expID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, incompleteFinding("事实原件不属于本任务或已撤销")
		}
		if err != nil {
			return nil, nil, err
		}
		var p struct {
			Evidence     []VerificationEvidence `json:"evidence_refs"`
			Verification json.RawMessage        `json:"verification"`
		}
		if json.Unmarshal(raw, &p) != nil || len(p.Evidence) == 0 || len(p.Verification) > 0 {
			return nil, nil, incompleteFinding("旧事实仅保留文字，不能自动自证影响；请读取该事实对应意图的已存工具回执或附件补交evidence_refs，无需重跑目标")
		}
		facts = append(facts, ref.NodeID)
		queue = append(queue, p.Evidence...)
	}
	if len(out) > 32 {
		return nil, nil, incompleteFinding("展开原件超过32项，请缩小本次登记范围")
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i] < facts[j] })
	return out, facts, nil
}
