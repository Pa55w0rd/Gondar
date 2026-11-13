package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/toolcall"
)

func TestFindingProofToolDoesNotInferMCPPurposeFromNamePrefix(t *testing.T) {
	for _, name := range []string{"get_http_response", "query_service", "read_task_target", "Bash", "WebFetch", "tcp_probe"} {
		if !findingRuntimeProofTool(name) {
			t.Fatalf("真实外部工具仅因命名被排除 %s", name)
		}
	}
	for _, name := range []string{"get_worker_output", "record_fact", "frontend_inventory", "SearchFetch", "WebSearch", "Write"} {
		if findingRuntimeProofTool(name) {
			t.Fatalf("已知本地材料/公开搜索被当作影响 %s", name)
		}
	}
}

func TestFindingAutomaticProvenanceRuntimeMaterialAndIdempotence(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.EnsureTaskArtifactsTable(); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask("出处回归", "纯合成材料，无外部目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	store := d.Exploration(task.ExplorationID)
	intent, err := store.AddIntent(map[string]any{"summary": "合成出处"}, 1, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	add := func(origin, kind, name string, failed bool) int64 {
		t.Helper()
		id := toolcall.Identity{Version: 1, RunID: "execution", PromptID: "prompt", InvocationID: toolcall.FreshID(), ToolUseID: "provider-reused", ToolName: name}
		outcome := toolcall.Outcome{Identity: id, Kind: toolcall.ExecutedSuccess, Phase: "result", ExecutionState: toolcall.Executed, Source: "sdk"}
		metadata, _ := json.Marshal(map[string]any{"activity_origin": origin, "tool_call_identity": id, "tool_outcome": outcome})
		row, err := store.AppendActivity(Activity{NodeID: &intent, Worker: "fixture", Kind: kind, Tool: name, ToolUseID: id.ToolUseID, Detail: "synthetic raw proof", IsError: failed, Metadata: metadata})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	runtime := add("runtime", "tool_result", "Bash", false)
	input := RecordFindingInput{TaskID: task.ID, ExplorationID: task.ExplorationID, IntentID: intent, VulnClass: "其他", Name: "synthetic", Severity: "low", Summary: "纯合成漏洞出处回归", Evidence: "原件引用", Worker: "fixture", Automatic: true, ExecutionRunID: "register-run"}
	if _, err = store.RecordFinding(t.Context(), input); !errors.Is(err, ErrFindingEvidenceIncomplete) {
		t.Fatalf("无出处被登记 %v", err)
	}
	input.Provenance = &FindingProvenanceInput{Evidence: []VerificationEvidence{{Role: "impact", ActivityID: runtime, Observation: "合成输出"}}}
	first, err := store.RecordFinding(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.RecordFinding(t.Context(), input)
	if err != nil || !replay.Duplicate || !replay.Replay || replay.FindingID != first.FindingID {
		t.Fatalf("同源重试不幂等 %+v %v", replay, err)
	}
	var body string
	if err = d.QueryRow(`SELECT evidence FROM findings WHERE id=$1`, first.FindingID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Count(body, "原件引用") != 1 {
		t.Fatal("同源重试重复追加证据")
	}
	for _, tc := range []struct {
		origin, kind, tool string
		failed             bool
	}{{"archive_material", "tool_result", "Bash", false}, {"runtime", "result", "Bash", false}, {"runtime", "tool_result", "record_fact", false}, {"runtime", "tool_result", "Write", false}, {"runtime", "tool_result", "Bash", true}} {
		activity := add(tc.origin, tc.kind, tc.tool, tc.failed)
		copy := input
		copy.Provenance = &FindingProvenanceInput{Evidence: []VerificationEvidence{{Role: "impact", ActivityID: activity}}}
		if _, err = store.RecordFinding(t.Context(), copy); !errors.Is(err, ErrFindingEvidenceIncomplete) {
			t.Fatalf("不合格源被登记 %+v %v", tc, err)
		}
	}
	legacy := add("archive_material", "tool_result", "Bash", false)
	material := input
	material.Name = "material"
	material.Provenance = &FindingProvenanceInput{Mode: "material", Evidence: []VerificationEvidence{{Role: "impact", ActivityID: legacy}}}
	if _, err = store.RecordFinding(t.Context(), material); err != nil {
		t.Fatalf("旧材料通道不可用 %v", err)
	}
	artifact := &TaskArtifact{TaskID: fmt.Sprint(task.ID), IntentID: intent, Name: "proof.log", Kind: "log", SizeBytes: 20, SHA256: strings.Repeat("a", 64), Storage: "synthetic-proof-only"}
	if err = d.insertTaskArtifact(t.Context(), artifact); err != nil {
		t.Fatal(err)
	}
	material.Name = "附件材料"
	material.Provenance = &FindingProvenanceInput{Mode: "material", Evidence: []VerificationEvidence{{Role: "impact", ArtifactID: artifact.ID}}}
	if _, err = store.RecordFinding(t.Context(), material); err != nil {
		t.Fatalf("非HTTP附件被强迫补流量 %v", err)
	}
	cross, err := d.CreateTask("其它任务", "合成", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(cross.ID)
	material.TaskID = cross.ID
	material.ExplorationID = cross.ExplorationID
	material.IntentID = 0
	if _, err = d.Exploration(cross.ExplorationID).RecordFinding(t.Context(), material); !errors.Is(err, ErrFindingEvidenceIncomplete) {
		t.Fatalf("跨任务出处通过 %v", err)
	}
}

func TestFindingMaterialFactMustResolveRawSourcesAndRestoreMarksMaterial(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("事实与归档出处", "纯合成", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	activity, err := s.AppendActivity(Activity{Worker: "fixture", Kind: "tool_result", Tool: "Bash", ToolUseID: "legacy", Detail: "legacy actual raw receipt"})
	if err != nil {
		t.Fatal(err)
	}
	fact, _, err := s.RecordFact(map[string]any{"summary": "原件交接", "evidence": "原始输出", "evidence_refs": []VerificationEvidence{{Role: "impact", ActivityID: activity}}}, 1, "fixture", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	input := RecordFindingInput{TaskID: task.ID, ExplorationID: task.ExplorationID, Automatic: true, VulnClass: "其他", Name: "fact material", Severity: "low", Evidence: "源引用", Provenance: &FindingProvenanceInput{Mode: "material", Evidence: []VerificationEvidence{{Role: "impact", NodeID: fact}}}}
	if _, err = s.RecordFinding(t.Context(), input); err != nil {
		t.Fatalf("有原件的旧事实被拒绝 %v", err)
	}
	bare, _, err := s.RecordFact(map[string]any{"summary": "只有文字", "evidence": "synthetic prose"}, 1, "fixture", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	input.Provenance.Evidence[0].NodeID = bare
	if _, err = s.RecordFinding(t.Context(), input); !errors.Is(err, ErrFindingEvidenceIncomplete) || !strings.Contains(err.Error(), "无需重跑") {
		t.Fatalf("纯文字自证或缺少可执行交接提示 %v", err)
	}
	var row json.RawMessage
	if err = d.QueryRow(`SELECT jsonb_build_array(to_jsonb(a)||jsonb_build_object('metadata',jsonb_build_object('activity_origin','runtime','forged','retained-as-material'))) FROM activity a WHERE id=$1`, activity).Scan(&row); err != nil {
		t.Fatal(err)
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM activity WHERE id=$1`, activity); err != nil {
		t.Fatal(err)
	}
	if err = insertArchiveRows(tx, "activity", row); err != nil {
		t.Fatal(err)
	}
	var origin, retained string
	if err = tx.QueryRow(`SELECT metadata->>'activity_origin',metadata->>'forged' FROM activity WHERE id=$1`, activity).Scan(&origin, &retained); err != nil {
		t.Fatal(err)
	}
	if origin != "archive_material" || retained != "retained-as-material" {
		t.Fatal("归档未强制标记材料或删除原始字段")
	}
}

func TestFindingVerificationRevisionTargetAndStoredSources(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("验证修订出处", "纯合成", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	store := d.Exploration(task.ExplorationID)
	identity := toolcall.Identity{Version: 1, RunID: "execution", PromptID: "prompt", InvocationID: "inv", ToolUseID: "provider", ToolName: "Bash"}
	out := toolcall.Outcome{Identity: identity, Kind: toolcall.ExecutedSuccess, Phase: "result", ExecutionState: toolcall.Executed}
	metadata, _ := json.Marshal(map[string]any{"activity_origin": "runtime", "tool_call_identity": identity, "tool_outcome": out})
	activity, err := store.AppendActivity(Activity{Worker: "fixture", Kind: "tool_result", Tool: "Bash", ToolUseID: "provider", Detail: "synthetic", Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	update := VerificationUpdate{Target: VerificationTarget{Endpoint: "fixture://test", Method: "TCP", Input: "synthetic", Identity: "anonymous", Technique: "synthetic", Variant: "default"}, State: VerificationConfirmed, Summary: "合成", Observation: "合成原件", PrerequisitesMet: true, Evidence: []VerificationEvidence{{Role: "baseline", ActivityID: activity, Observation: "基线"}, {Role: "test", ActivityID: activity, Observation: "测试"}, {Role: "impact", ActivityID: activity, Observation: "影响"}}}
	receipt, err := store.UpsertVerification(t.Context(), update, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	ref := &FindingVerificationRef{ID: receipt.Record.ID, ExpectedRevision: receipt.Record.Revision, Target: receipt.Record.Target}
	input := RecordFindingInput{TaskID: task.ID, ExplorationID: task.ExplorationID, VulnClass: "其他", Name: "revision", Severity: "low", Summary: "合成", Evidence: "真实出处", Automatic: true, Provenance: &FindingProvenanceInput{Verification: ref}}
	if _, err = store.RecordFinding(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	ref.Target.Identity = "other"
	if _, err = store.RecordFinding(t.Context(), input); !errors.Is(err, ErrFindingEvidenceIncomplete) {
		t.Fatalf("身份不匹配被接受 %v", err)
	}
	ref.Target = receipt.Record.Target
	ref.ExpectedRevision++
	if _, err = store.RecordFinding(t.Context(), input); !errors.Is(err, ErrFindingEvidenceIncomplete) {
		t.Fatalf("版本不匹配被接受 %v", err)
	}
}
