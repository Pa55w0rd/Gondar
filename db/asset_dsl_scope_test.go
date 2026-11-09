package db

import (
	"fmt"
	"testing"
)

func TestDSLOrCannotBypassTaskTypeOrDeclaredScope(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	one, err := d.CreateTask("DSL scope fixture", "local inert fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	two, err := d.CreateTask("DSL foreign fixture", "local inert fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTaskCascade(two.ID, true, true); d.DeleteTaskCascade(one.ID, true, true) })
	owned := fmt.Sprintf("owned-dsl-%d.invalid", one.ID)
	foreign := fmt.Sprintf("foreign-dsl-%d.invalid", two.ID)
	av := d.Assets()
	want := seedTreeAsset(t, d, "subdomain", map[string]any{"domain": owned, "task_ids": []int64{one.ID}})
	other := seedTreeAsset(t, d, "subdomain", map[string]any{"domain": foreign, "task_ids": []int64{two.ID}})
	wrongType := seedTreeAsset(t, d, "root_domain", map[string]any{"domain": foreign, "task_ids": []int64{one.ID}})
	t.Cleanup(func() {
		for _, id := range []int64{want, other, wrongType} {
			deleteAsset(d, id)
		}
	})
	if _, err := av.AddAgentScope(one.ID, "subdomain", owned, "fixture", "manual"); err != nil {
		t.Fatal(err)
	}
	dsl := fmt.Sprintf(`domain=="%s" OR domain=="%s"`, foreign, owned)
	for _, tc := range []struct {
		name, typ string
		task      int64
		count     int
	}{
		{"unscoped_or_keeps_both_branches", "", 0, 3},
		{"type_applies_to_whole_or", "subdomain", 0, 2},
		{"task_applies_to_whole_or", "", one.ID, 2},
		{"type_and_task_apply_to_whole_or", "subdomain", one.ID, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := av.CountDSL(dsl, tc.typ, tc.task)
			if err != nil || n != tc.count {
				t.Fatalf("count=%d want=%d err=%v", n, tc.count, err)
			}
			assets, err := av.QueryDSL(dsl, tc.typ, tc.task, 50, 0)
			if err != nil || len(assets) != tc.count {
				t.Fatalf("query count=%d want=%d err=%v", len(assets), tc.count, err)
			}
		})
	}
	t.Run("declared_scope_applies_to_whole_or", func(t *testing.T) {
		for _, typ := range []string{"", "subdomain"} {
			assets, err := av.QueryDSLInScope(dsl, typ, one.ID, 50, 0)
			if err != nil || len(assets) != 1 || assets[0].ID != want {
				t.Fatalf("scope query type=%q assets=%+v err=%v", typ, assets, err)
			}
		}
	})
}
