package agent

import (
	"encoding/json"
	"fmt"
	"testing"

	"gondar/db"
)

func TestAssetLookupHasBoundedScopedPagination(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask(t.Name(), "synthetic", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	as := d.Assets()
	var ids []int64
	defer func() {
		for _, id := range ids {
			_, _ = d.Exec(`DELETE FROM assets WHERE id=$1`, id)
		}
	}()
	for i := 1; i <= 55; i++ {
		id, err := as.UpsertIP(db.UpsertIPReq{IP: fmt.Sprintf("192.0.2.%d", i), TaskID: task.ID})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if err := as.AddAutoScope(task.ID, "ip", "", "", fmt.Sprintf("192.0.2.%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	tools := NewToolSet(d.Exploration(task.ExplorationID), "planner")
	tools.SetTaskID(task.ID)
	tools.SetAssetStore(as, as.Companies())
	page := func(raw string) map[string]any {
		t.Helper()
		result, err := tools.listAssets().Call(t.Context(), json.RawMessage(raw), nil)
		if err != nil || result.IsError {
			t.Fatalf("lookup failed %v %s", err, result.Flatten())
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(result.Flatten()), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := page(`{"dsl":"cidr=192.0.2.0/24","limit":999}`)
	if first["total"] != float64(55) || len(first["assets"].([]any)) != 50 || first["has_more"] != true || first["next_offset"] != float64(50) {
		t.Fatalf("page boundary missing: %+v", first)
	}
	second := page(`{"dsl":"cidr=192.0.2.0/24","offset":50}`)
	if len(second["assets"].([]any)) != 5 || second["has_more"] != false || second["total"] != float64(55) {
		t.Fatalf("page lost remainder %+v", second)
	}
	if outside := page(`{"dsl":"cidr=198.51.100.0/24"}`); outside["total"] != float64(0) {
		t.Fatal("CIDR lookup escaped authorized scope")
	}
	if excluded := page(`{"dsl":"cidr!=192.0.2.0/24"}`); excluded["total"] != float64(0) {
		t.Fatal("CIDR exclusion used reversed containment")
	}
	invalid, err := tools.listAssets().Call(t.Context(), json.RawMessage(`{"dsl":"cidr=192.0.2.0/24","detail":true}`), nil)
	if err != nil || !invalid.IsError {
		t.Fatal("unbounded full details allowed")
	}
}

func TestCompactAssetsAvoidsCredentialAndRawMetadataDump(t *testing.T) {
	assets := compactAssets([]*db.Asset{{ID: 1, Auth: []map[string]any{{"token": "fixture-only"}}, Extra: map[string]any{"raw_response": "large fixture"}}})
	if len(assets) != 1 || assets[0]["auth_available"] != true || assets[0]["auth"] != nil || assets[0]["extra"] != nil {
		t.Fatalf("compact list exposed detail fields: %+v", assets)
	}
}
