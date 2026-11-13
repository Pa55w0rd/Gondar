package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func reportSourceFixture(t *testing.T) (*DB, *Task, int64, int64, *Conversation) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("报告来源合成夹具", "不访问外部目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTaskCascade(task.ID, true, true) })
	node, err := d.Exploration(task.ExplorationID).AddNode(KindFinding, map[string]any{"summary": "fixture"}, 1, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	fid, err := d.AddFinding(task.ID, node, "source-fixture", "fixture", "low", "fixture", "saved fixture", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fid == node {
		next, err := d.Exploration(task.ExplorationID).AddNode(KindFinding, map[string]any{"summary": "distinct node"}, 1, "confirmed", "fixture", nil)
		if err != nil {
			t.Fatal(err)
		}
		node = next
		if _, err := d.Exec(`UPDATE findings SET node_id=$2 WHERE id=$1`, fid, node); err != nil {
			t.Fatal(err)
		}
	}
	conv, err := d.CreateConversation("reporter", fmt.Sprintf("finding#%d and #10 are only title text", fid), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM conversations WHERE id=$1`, conv.ID) })
	return d, task, fid, node, conv
}

func reportConversationSource(id int64) *FindingReportSource {
	return &FindingReportSource{Kind: "conversation", ConversationID: id, RunID: "host-report-run", PromptID: "host-prompt", InvocationID: "host-invocation", ToolUseID: "provider-reused-id"}
}

func TestFindingReportSourceExactIdentityAtomicityAndUnknownRewrite(t *testing.T) {
	d, task, fid, node, conv := reportSourceFixture(t)
	version := int64(0)
	if n, err := d.SetFindingReportWithSourceByNodeID(t.Context(), node, "exact report", &version, reportConversationSource(conv.ID)); err != nil || n != 1 {
		t.Fatalf("save %d %v", n, err)
	}
	f, err := d.GetFinding(fid)
	if err != nil {
		t.Fatal(err)
	}
	source := f.ReportSource
	if source == nil || !source.Available || source.Kind != "conversation" || source.FindingID != fid || source.NodeID != node || source.TaskID != task.ID || source.ConversationID != conv.ID || source.ReportSHA256 != reportBodySHA256(f.Report) || source.FindingID == source.NodeID {
		t.Fatalf("wrong exact source: %+v", source)
	}
	wrong := reportConversationSource(conv.ID)
	wrong.FindingID = fid + 10
	if _, err := d.SetFindingReportWithSourceByNodeID(t.Context(), node, "must not replace", &version, wrong); !errors.Is(err, ErrFindingReportSource) {
		t.Fatalf("wrong finding accepted: %v", err)
	}
	if _, err := d.Exec(`UPDATE findings SET evidence_version=1 WHERE id=$1`, fid); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetFindingReportWithSourceByNodeID(t.Context(), node, "stale writer", &version, reportConversationSource(conv.ID)); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("stale report accepted: %v", err)
	}
	f, err = d.GetFinding(fid)
	if err != nil || f.Report != "exact report" || f.ReportSource == nil || f.ReportSource.InvocationID != "host-invocation" {
		t.Fatal("failed transaction changed report/source")
	}
	version = 1
	if _, err := d.SetFindingReportVersionByNodeID(t.Context(), node, "legacy rewrite", &version); err != nil {
		t.Fatal(err)
	}
	f, err = d.GetFinding(fid)
	if err != nil || f.Report != "legacy rewrite" || f.ReportSource != nil {
		t.Fatal("unknown writer borrowed earlier report source")
	}
	var history int
	if err := d.QueryRow(`SELECT COUNT(*) FROM activity WHERE exploration_id=$1 AND kind='report_source' AND metadata->'previous_report_source'->>'invocation_id'='host-invocation'`, task.ExplorationID).Scan(&history); err != nil || history != 1 {
		t.Fatalf("historical receipt lost: %d %v", history, err)
	}
}

func TestFindingReportSourceNoTextBackfillDeletedConversationAndHashMismatch(t *testing.T) {
	d, _, fid, node, conv := reportSourceFixture(t)
	if _, err := d.SetFindingReportByNodeID(node, "historical report"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AppendConvActivity(conv.ID, Activity{Kind: "tool_result", Tool: "update_finding_report", Detail: fmt.Sprintf("finding %d report updated; #1 #10", node)}); err != nil {
		t.Fatal(err)
	}
	f, err := d.GetFinding(fid)
	if err != nil || f.ReportSource != nil {
		t.Fatal("historical text was guessed into provenance")
	}
	if _, err := d.SetFindingReportWithSourceByNodeID(t.Context(), node, "bound report", nil, reportConversationSource(conv.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DELETE FROM conversations WHERE id=$1`, conv.ID); err != nil {
		t.Fatal(err)
	}
	f, err = d.GetFinding(fid)
	if err != nil || f.Report != "bound report" || f.ReportSource == nil || f.ReportSource.Available || !strings.Contains(f.ReportSource.UnavailableReason, "删除") {
		t.Fatalf("deleted conversation lost report or stayed clickable: %+v %v", f, err)
	}
	if _, err := d.Exec(`UPDATE findings SET report='unattributed direct rewrite' WHERE id=$1`, fid); err != nil {
		t.Fatal(err)
	}
	f, err = d.GetFinding(fid)
	if err != nil || f.ReportSource != nil {
		t.Fatal("a mismatched report hash reused a receipt")
	}
}

func TestFindingReportRecoveryBindsLeaseSourceWithoutExposingToken(t *testing.T) {
	d, _, fid, now := reportRecoveryFixture(t)
	job := mustReportClaim(t, d, now)
	if job.FindingID != fid {
		t.Fatal("fixture claim mismatch")
	}
	if err := d.CompleteFindingReportRecovery(context.Background(), job, "## Recovered saved evidence", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	f, err := d.GetFinding(fid)
	if err != nil {
		t.Fatal(err)
	}
	if f.ReportSource == nil || !f.ReportSource.Available || f.ReportSource.Kind != "recovery_job" || f.ReportSource.RecoveryID != FindingReportRecoverySourceID(job) || f.ReportSource.RecoveryAttempt != job.Attempt || f.ReportSource.ConversationID != 0 {
		t.Fatalf("recovery receipt absent/misattributed: %+v", f.ReportSource)
	}
	raw, _ := json.Marshal(f.ReportSource)
	if strings.Contains(string(raw), job.Token) {
		t.Fatal("lease credential exposed in report provenance")
	}
	if err := d.CompleteFindingReportRecovery(t.Context(), job, "must preserve recovered report", now.Add(time.Minute)); !errors.Is(err, ErrFindingReportRecoveryStale) {
		t.Fatal("completed lease could replace old report")
	}
}

func TestFindingReportSourceArchiveDetachesConversationAndLegacyIsNullable(t *testing.T) {
	d, _, fid, node, conv := reportSourceFixture(t)
	if _, err := d.SetFindingReportWithSourceByNodeID(t.Context(), node, "archive report", nil, reportConversationSource(conv.ID)); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := d.QueryRow(`SELECT jsonb_build_array(to_jsonb(f)) FROM findings f WHERE id=$1`, fid).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizeArchivedFindingVersions(raw)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := decodeArchiveRows(normalized)
	if err != nil {
		t.Fatal(err)
	}
	sourceRaw, _ := json.Marshal(rows[0]["report_source"])
	if _, err := d.Exec(`UPDATE findings SET report_source=$2 WHERE id=$1`, fid, sourceRaw); err != nil {
		t.Fatal(err)
	}
	f, err := d.GetFinding(fid)
	if err != nil || f.ReportSource == nil || !f.ReportSource.Archived || f.ReportSource.Available || f.ReportSource.ReportSHA256 != reportBodySHA256("archive report") {
		t.Fatal("restored source linked a bare conversation ID or lost its receipt")
	}
	legacy, err := normalizeArchivedFindingVersions(json.RawMessage(`[{"id":1,"report":"old body"}]`))
	if err != nil {
		t.Fatal(err)
	}
	oldRows, err := decodeArchiveRows(legacy)
	if err != nil || oldRows[0]["report_source"] != nil || oldRows[0]["report"] != "old body" {
		t.Fatal("legacy archive source default/contents changed")
	}
}
