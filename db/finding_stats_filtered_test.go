package db

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

func TestFilteredFindingCountsMatchListAndAssetScope(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	marker := fmt.Sprintf("__filtered_counts_%d__", time.Now().UnixNano())
	task, err := d.CreateTask(marker, "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	defer d.Exec(`DELETE FROM findings WHERE vulnclass=$1`, marker)
	asset := seedTreeAsset(t, d, "subdomain", map[string]any{"domain": marker + ".invalid", "root_domain": marker + ".invalid"})
	defer d.Exec(`DELETE FROM assets WHERE id=$1`, asset)
	for _, item := range []struct {
		task                   int64
		severity, status, name string
		assets                 []int64
	}{
		{task.ID, "critical", "pending", "literal_%", []int64{asset}},
		{task.ID, "high", "confirmed", "other", []int64{asset}},
		{0, "low", "pending", "unassigned", nil},
	} {
		id, addErr := d.AddFinding(item.task, 0, marker, item.name, item.severity, "", "", "fixture", item.assets)
		if addErr != nil {
			t.Fatal(addErr)
		}
		if _, err := d.SetFindingStatus(id, item.status); err != nil {
			t.Fatal(err)
		}
	}
	filters := []FindingFilter{
		{VulnClass: marker},
		{VulnClass: marker, TaskID: strconv.FormatInt(task.ID, 10)},
		{VulnClass: marker, Status: "pending"},
		{VulnClass: marker, Severity: "high"},
		{VulnClass: marker, Query: "_%"},
		{VulnClass: marker, TaskID: FindingUnassignedTask},
		{VulnClass: marker, AssetScope: fmt.Sprintf("a:%d", asset)},
		{VulnClass: marker, AssetScope: "__none__"},
		{VulnClass: marker, AssetScope: "a:9223372036854775806"},
		{VulnClass: marker, ExcludeStatuses: []string{"confirmed"}},
	}
	for _, filter := range filters {
		counts, err := d.FilteredFindingCounts(filter)
		if err != nil {
			t.Fatal(err)
		}
		rows, total, err := d.ListFindingsPage(filter, 1, 100)
		if err != nil {
			t.Fatal(err)
		}
		want := FindingCounts{Total: total}
		for _, row := range rows {
			if row.Status == "pending" {
				want.Pending++
			}
			switch row.Severity {
			case "critical":
				want.Critical++
			case "high":
				want.High++
			case "medium":
				want.Medium++
			case "low":
				want.Low++
			}
		}
		if *counts != want {
			t.Fatalf("filter %+v: got %+v want %+v", filter, *counts, want)
		}
	}
	counts, err := d.FilteredFindingCounts(FindingFilter{VulnClass: marker, Query: "_%"})
	if err != nil || counts.Total != 1 {
		t.Fatalf("literal wildcard filter: %+v %v", counts, err)
	}
	global, err := d.FindingStats()
	if err != nil || global.Total < 3 || global.Filtered != nil {
		t.Fatalf("legacy global aggregate changed: %+v %v", global, err)
	}
}
