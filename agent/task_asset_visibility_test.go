package agent

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gondar/db"
)

func TestTaskSourcesToolsKeepVisibilityAndDoNotAuthorizeDiscoveredAssets(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	root := fmt.Sprintf("tool-visibility-%d.invalid", time.Now().UnixNano())
	task, err := d.CreateTaskWithOptions(root, "合成资料测试", db.TaskCreateOptions{AssetContextMode: db.AssetContextTaskSources})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTaskCascade(task.ID, true, true)
	peer, err := d.CreateTask("peer", "合成其它资料", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTaskCascade(peer.ID, true, true)
	as := d.Assets()
	if _, err := as.AddAgentScope(task.ID, "root_domain", root, "用户指定合成范围", "manual"); err != nil {
		t.Fatal(err)
	}
	visibleCompany, _, err := d.Companies().UpsertCompany(root+" visible", "")
	if err != nil {
		t.Fatal(err)
	}
	hiddenCompany, _, err := d.Companies().UpsertCompany(root+" hidden", "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Companies().DeleteCompany(visibleCompany)
	defer d.Companies().DeleteCompany(hiddenCompany)
	var visible, hidden int64
	for _, entry := range []struct {
		label          string
		owner, company int64
		dest           *int64
	}{{"current", task.ID, visibleCompany, &visible}, {"other", peer.ID, hiddenCompany, &hidden}} {
		host := entry.label + "." + root
		if err := d.QueryRow(`INSERT INTO assets(type,url,domain,root_domain,task_ids,company_id) VALUES('service',$1,$2,$3,$4,$5) RETURNING id`, "https://"+host, host, root, []int64{entry.owner}, entry.company).Scan(entry.dest); err != nil {
			t.Fatal(err)
		}
	}
	defer as.DeleteByIDs([]int64{visible, hidden})
	tools := NewToolSet(d.Exploration(task.ExplorationID), "worker")
	tools.SetTaskID(task.ID)
	tools.SetAssetStore(as, d.Companies())
	lookup := func(raw string) map[string]any {
		t.Helper()
		result, err := tools.listAssets().Call(t.Context(), json.RawMessage(raw), nil)
		if err != nil || result.IsError {
			t.Fatalf("lookup failed: %s err=%v", result.Flatten(), err)
		}
		return toolResultJSON(t, result)
	}
	for _, raw := range []string{fmt.Sprintf(`{"id":%d}`, visible), fmt.Sprintf(`{"ids":%d}`, visible), fmt.Sprintf(`{"ids":[%d,%d]}`, visible, hidden)} {
		value := lookup(raw)
		items := value["assets"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["id"] != float64(visible) {
			t.Fatalf("ID contract leaked hidden row: %s => %v", raw, value)
		}
	}
	value := lookup(fmt.Sprintf(`{"ids":%d}`, hidden))
	if len(value["assets"].([]any)) != 0 {
		t.Fatalf("hidden scalar ID leaked: %v", value)
	}
	companies, err := tools.listCompanies().Call(t.Context(), json.RawMessage(`{}`), nil)
	if err != nil || companies.IsError {
		t.Fatalf("companies failed: %s err=%v", companies.Flatten(), err)
	}
	companyResult := toolResultJSON(t, companies)
	items := companyResult["companies"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != float64(visibleCompany) || items[0].(map[string]any)["asset_count"] != float64(1) {
		t.Fatalf("company tool leaked global material/count: %v", companyResult)
	}
	for _, entry := range []struct {
		kind, value string
		wantErrors  bool
	}{{"subdomain", "discovered.outside.invalid", true}, {"root_domain", root, false}} {
		input, _ := json.Marshal(map[string]any{"kind": entry.kind, "value": entry.value, "reason": "合成工具回归"})
		result, err := tools.addTaskScope().Call(t.Context(), input, nil)
		if err != nil || result.IsError {
			t.Fatalf("scope outcome became an execution failure: %s err=%v", result.Flatten(), err)
		}
		decoded := toolResultJSON(t, result)
		_, hasErrors := decoded["errors"]
		if hasErrors != entry.wantErrors {
			t.Fatalf("scope result=%v want errors=%t", decoded, entry.wantErrors)
		}
	}
	inserted, err := tools.insertAssets().Call(t.Context(), json.RawMessage(`{"assets":[{"type":"service","url":"https://machine-discovered.outside.invalid"}]}`), nil)
	if err != nil || inserted.IsError {
		t.Fatalf("material registration failed: %s err=%v", inserted.Flatten(), err)
	}
	insertResult := toolResultJSON(t, inserted)
	resultItems := insertResult["results"].([]any)
	if len(resultItems) != 1 {
		t.Fatalf("new material not retained: %v", insertResult)
	}
	discovered := int64(resultItems[0].(map[string]any)["id"].(float64))
	defer as.DeleteByIDs([]int64{discovered})
	if rows, err := as.GetByIDs([]int64{discovered}); err != nil || len(rows) != 1 {
		t.Fatalf("registered material missing: %+v err=%v", rows, err)
	}
	if rows, err := as.GetWorkerAssets(task.ID, []int64{discovered}); err != nil || len(rows) != 0 {
		t.Fatalf("new material authorized itself: %+v err=%v", rows, err)
	}
	scopes, err := as.ListTaskScope(task.ID)
	if err != nil || len(scopes) != 1 || scopes[0].Domain != root {
		t.Fatalf("machine discovery expanded scope: %+v err=%v", scopes, err)
	}
}
