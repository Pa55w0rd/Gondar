package db

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func visibilityAssetIDs(items []*Asset) []int64 {
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func TestTaskAssetContextModePatchSnapshotAndArchiveCompatibility(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTaskWithOptions("https://mode.fixture.invalid", "仅合成配置与历史", TaskCreateOptions{AssetContextMode: AssetContextTaskSources})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	store := d.Exploration(task.ExplorationID)
	node, err := store.AddNode(KindFact, map[string]any{"summary": "保留的合成事实"}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	fid, err := d.AddFinding(task.ID, node, "fixture", "保留合成成果", SeverityLow, "摘要", "原有证据", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteFinding(fid)
	if _, err := d.SetFindingReportByNodeID(node, "合成报告正文，保留原有成果"); err != nil {
		t.Fatal(err)
	}
	oldFinding, err := d.GetFinding(fid)
	if err != nil {
		t.Fatal(err)
	}
	compat := AssetContextDeclaredScope
	updated, err := d.UpdateTask(task.ID, TaskPatch{AssetContextMode: &compat})
	if err != nil || !updated.Paused || updated.Status != task.Status {
		t.Fatalf("mode patch changed lifecycle: %+v err=%v", updated, err)
	}
	narrow := AssetContextTaskSources
	updated, err = d.UpdateTask(task.ID, TaskPatch{AssetContextMode: &narrow})
	if err != nil || !updated.Paused {
		t.Fatalf("mode switch lost pause: %+v err=%v", updated, err)
	}
	unchanged, err := d.GetFinding(fid)
	if err != nil || !reflect.DeepEqual(oldFinding, unchanged) {
		t.Fatalf("mode switch rewrote finding/report identity: old=%+v new=%+v err=%v", oldFinding, unchanged, err)
	}
	// A daily task captures its own explicit mode, even if the source changes
	// afterwards. Parent/source mode is never consulted implicitly at creation.
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	plan, err := d.CreateDailySchedule(task.ID, "09:00", "Asia/Shanghai", 1, 600, now)
	if err != nil {
		t.Fatal(err)
	}
	defer d.CancelDailySchedule(task.ID, plan.ID)
	if _, err := d.UpdateTask(task.ID, TaskPatch{AssetContextMode: &compat}); err != nil {
		t.Fatal(err)
	}
	child, err := d.MaterializeDailySchedule(plan.NextRunAt.Add(time.Second))
	if err != nil || child == nil || child.AssetContextMode != narrow {
		t.Fatalf("daily snapshot mode lost: %+v err=%v", child, err)
	}
	defer d.DeleteTask(child.ID)
	if _, err := d.UpdateTask(task.ID, TaskPatch{AssetContextMode: &narrow}); err != nil {
		t.Fatal(err)
	}
	// Complete a real database archive/restore cycle; no archive files or
	// running-task state are touched by this database-only regression.
	archive, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM task_archives WHERE id=$1`, archive.ID)
	claimed, err := d.ClaimTaskArchiveJob(t.Context())
	if err != nil || claimed == nil || claimed.ID != archive.ID {
		t.Fatalf("claim archive=%+v err=%v", claimed, err)
	}
	var llm bytes.Buffer
	snapshot, err := d.SnapshotTaskArchiveWithLLMRecords(task.ID, &llm)
	if err != nil {
		t.Fatal(err)
	}
	var taskRows []map[string]any
	if err := json.Unmarshal(snapshot.Tables["tasks"], &taskRows); err != nil || len(taskRows) != 1 || taskRows[0]["asset_context_mode"] != narrow {
		t.Fatalf("archive snapshot lost mode: %v err=%v", taskRows, err)
	}
	if err := d.CompleteTaskArchive(archive.ID, snapshot, "synthetic-db-only.tar.zst", "fixture", 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.QueueTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err = d.ClaimTaskArchiveJob(t.Context())
	if err != nil || claimed == nil || claimed.State != Restoring {
		t.Fatalf("claim restore=%+v err=%v", claimed, err)
	}
	if _, err := d.RestoreTaskArchiveWithLLMRecords(archive.ID, snapshot, 0, bytes.NewReader(llm.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	restored, err := d.GetTask(task.ID)
	if err != nil || restored == nil || restored.AssetContextMode != narrow || !restored.Paused {
		t.Fatalf("restored task mode/pause=%+v err=%v", restored, err)
	}
	if current, err := d.GetFinding(fid); err != nil || current == nil || current.Report != oldFinding.Report {
		t.Fatalf("archive changed preserved report: %+v err=%v", current, err)
	}
	for _, row := range []map[string]any{{}, {"asset_context_mode": nil}, {"asset_context_mode": "task_sources"}} {
		if err := normalizeArchivedAssetContextMode(row); err != nil {
			t.Fatal(err)
		}
		if row["asset_context_mode"] != compat && row["asset_context_mode"] != narrow {
			t.Fatalf("invalid normalized mode: %v", row)
		}
	}
	for _, value := range []any{true, 42, "all_assets"} {
		if err := normalizeArchivedAssetContextMode(map[string]any{"asset_context_mode": value}); !errors.Is(err, ErrTaskArchiveFormatMismatch) {
			t.Fatalf("invalid archived mode accepted: %v err=%v", value, err)
		}
	}
}

func TestTaskSourcesVisibilityIntersectsScopeAcrossEveryCandidateReader(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	as := d.Assets()
	root := fmt.Sprintf("visibility-%d.invalid", time.Now().UnixNano())
	var tasks []*Task
	newTask := func(mode string, sources ...int64) *Task {
		task, err := d.CreateTaskWithOptions(root, "合成可见性验证", TaskCreateOptions{AssetContextMode: mode, SourceTaskIDs: sources})
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
		if _, err := as.AddAgentScope(task.ID, "root_domain", root, "合成声明范围", "manual"); err != nil {
			t.Fatal(err)
		}
		return task
	}
	grand := newTask(AssetContextDeclaredScope)
	source := newTask(AssetContextTaskSources, grand.ID)
	task := newTask(AssetContextTaskSources, source.ID)
	peer := newTask("")
	var allIDs []int64
	defer func() {
		as.DeleteByIDs(allIDs)
		for i := len(tasks) - 1; i >= 0; i-- {
			d.DeleteTask(tasks[i].ID)
		}
	}()
	company, _, err := d.Companies().UpsertCompany(root+" visible", "")
	if err != nil {
		t.Fatal(err)
	}
	otherCompany, _, err := d.Companies().UpsertCompany(root+" hidden", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupCompany(d, company)
	defer cleanupCompany(d, otherCompany)
	add := func(label string, owner int64, companyID int64, outside bool) int64 {
		host := label + "." + root
		assetRoot := root
		if outside {
			host = label + ".outside.invalid"
			assetRoot = "outside.invalid"
		}
		var owners []int64
		if owner > 0 {
			owners = []int64{owner}
		} else {
			owners = []int64{}
		}
		var cid any
		if companyID > 0 {
			cid = companyID
		}
		var id int64
		if err := d.QueryRow(`INSERT INTO assets(type,url,domain,root_domain,task_ids,company_id) VALUES('service',$1,$2,$3,$4,$5) RETURNING id`, "https://"+host, host, assetRoot, owners, cid).Scan(&id); err != nil {
			t.Fatal(err)
		}
		allIDs = append(allIDs, id)
		return id
	}
	current := add("current", task.ID, company, false)
	direct := add("direct", source.ID, company, false)
	peerAsset := add("peer", peer.ID, otherCompany, false)
	grandAsset := add("grand", grand.ID, otherCompany, false)
	global := add("global", 0, otherCompany, false)
	outside := add("outside", task.ID, company, true)
	anchor := add("anchored", 0, 0, false)
	store := d.Exploration(task.ExplorationID)
	if _, err := store.AddIntent(map[string]any{"description": "合成意图锚点"}, 5, []int64{anchor}, "test"); err != nil {
		t.Fatal(err)
	}
	history, err := store.AddNode(KindFact, map[string]any{"summary": "既有范围外历史事实"}, 0, "confirmed", "test", []int64{outside})
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{current, direct, anchor}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	assertIDs := func(label string, items []*Asset, err error) {
		t.Helper()
		if err != nil || !reflect.DeepEqual(visibilityAssetIDs(items), want) {
			t.Fatalf("%s ids=%v want=%v err=%v", label, visibilityAssetIDs(items), want, err)
		}
	}
	items, err := as.QueryDSLInScope("", "service", task.ID, 100, 0)
	assertIDs("DSL", items, err)
	items, err = as.GetByIDsInScope(task.ID, allIDs)
	assertIDs("IDs", items, err)
	items, err = as.GetWorkerAssets(task.ID, allIDs)
	assertIDs("worker preload", items, err)
	if total, err := as.CountDSLInScope("", "service", task.ID); err != nil || total != 3 {
		t.Fatalf("count=%d err=%v", total, err)
	}
	if counts, err := as.CountsByTypeInScope(task.ID); err != nil || counts["service"] != 3 {
		t.Fatalf("counts=%v err=%v", counts, err)
	}
	or := fmt.Sprintf(`domain="current.%s" OR domain="peer.%s"`, root, root)
	orItems, err := as.QueryDSLInScope(or, "service", task.ID, 100, 0)
	if err != nil || len(orItems) != 1 || orItems[0].ID != current {
		t.Fatalf("top-level OR escaped source filter: %+v err=%v", orItems, err)
	}
	for _, hidden := range []int64{peerAsset, grandAsset, global, outside} {
		items, err := as.GetByIDsInScope(task.ID, []int64{hidden})
		if err != nil || len(items) != 0 {
			t.Fatalf("scalar id %d escaped: %+v err=%v", hidden, items, err)
		}
	}
	companies, err := d.Companies().ListCompaniesInTask(task.ID)
	if err != nil || len(companies) != 1 || companies[0].ID != company || companies[0].AssetCount != 2 {
		t.Fatalf("company material leaked/global count used: %+v err=%v", companies, err)
	}
	cov, err := as.TaskCoverageWithSources(task.ID)
	if err != nil || cov.Denominator != 3 || cov.HistoricalOnly != 1 {
		t.Fatalf("current denominator/history mixed: %+v err=%v", cov, err)
	}
	untested, total, err := as.ListUntestedAssetsWithSources(task.ID, "service", 100, 0)
	if err != nil || total != 3 || len(untested) != 3 {
		t.Fatalf("untested candidates=%+v total=%d err=%v", untested, total, err)
	}
	graph, err := as.BuildCoverageGraph(task.ID, task.ExplorationID)
	if err != nil {
		t.Fatal(err)
	}
	var graphIDs []int64
	for _, node := range graph.Nodes {
		if node.InScope && node.AssetID > 0 {
			graphIDs = append(graphIDs, node.AssetID)
		}
	}
	sort.Slice(graphIDs, func(i, j int) bool { return graphIDs[i] < graphIDs[j] })
	if !reflect.DeepEqual(graphIDs, want) {
		t.Fatalf("graph candidates=%v want=%v", graphIDs, want)
	}
	if node, err := store.GetNode(history); err != nil || node == nil || !strings.Contains(string(node.Payload), "历史") {
		t.Fatalf("history hidden: %+v err=%v", node, err)
	}
	// A manual association exposes in-scope material, but cannot authorize an
	// out-of-scope row. No source-of-source relation is followed implicitly.
	if _, err := as.AttachAssetsToTask(task.ID, []int64{peerAsset, outside}, "用户显式关联合成资料"); err != nil {
		t.Fatal(err)
	}
	items, err = as.GetByIDsInScope(task.ID, []int64{peerAsset, outside, grandAsset})
	if err != nil || len(items) != 1 || items[0].ID != peerAsset {
		t.Fatalf("manual association expanded scope: %+v err=%v", items, err)
	}
	before, err := as.ListTaskScope(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := as.AddAgentScope(task.ID, "subdomain", "machine.outside.invalid", "模型新增不构成授权", "agent"); err == nil {
		t.Fatal("agent extended narrow-mode authorization")
	}
	if _, err := as.AddAgentScope(task.ID, "root_domain", root, "幂等现有规则", "agent"); err != nil {
		t.Fatalf("idempotent existing scope rejected: %v", err)
	}
	if err := as.AddAutoScope(task.ID, "service", "machine.outside.invalid", "https://machine.outside.invalid", ""); err != nil {
		t.Fatal(err)
	}
	preload, err := as.GetWorkerAssets(task.ID, []int64{current, outside, grandAsset})
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range preload {
		if err := as.AddAutoScope(task.ID, asset.Type, asset.Domain, asset.URL, asset.IP); err != nil {
			t.Fatal(err)
		}
	}
	after, err := as.ListTaskScope(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range after {
		if strings.Contains(scope.Domain, "outside.invalid") {
			t.Fatalf("hidden preload bootstrapped scope: %+v", scope)
		}
	}
	if len(after) != len(before) {
		t.Fatal("machine discovery changed the declared scopes")
	}
	legacy := AssetContextDeclaredScope
	updated, err := d.UpdateTask(task.ID, TaskPatch{AssetContextMode: &legacy})
	if err != nil || updated.AssetContextMode != legacy {
		t.Fatalf("mode switch=%+v err=%v", updated, err)
	}
	if count, err := as.CountDSLInScope("", "service", task.ID); err != nil || count != 6 {
		t.Fatalf("compatibility declared scope changed: count=%d err=%v", count, err)
	}
	if peer.AssetContextMode != AssetContextDeclaredScope {
		t.Fatalf("new task default changed: %+v", peer)
	}
}
