package server

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSeedFirstIntentWritesOnlySingleDirection(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	s := &Server{m: m}
	for _, complex := range []bool{false, true} {
		description := "仅在已登记域名核对一个接口"
		if complex {
			description = strings.Repeat("保留完整范围与多阶段检查说明", 30)
		}
		task, err := m.CreateTask(description, "验证合成证据", nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := strconv.ParseInt(task.ID, 10, 64)
		t.Cleanup(func() { m.pg.DeleteTask(id) })
		s.seedFirstIntent(task)
		var count int
		if err := m.pg.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent'`, task.Store.ID()).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 1
		if complex {
			want = 0
		}
		if count != want {
			t.Fatalf("complex=%v intents=%d want=%d", complex, count, want)
		}
		if !complex {
			var edges int
			if err := m.pg.QueryRow(`SELECT count(*) FROM exploration_edges WHERE exploration_id=$1 AND rel='derived_from'`, task.Store.ID()).Scan(&edges); err != nil {
				t.Fatal(err)
			}
			if edges != 1 {
				t.Fatal("direct seed lost origin lineage")
			}
		}
	}
}

func TestDirectSeedRoutesFullScenariosToPlanner(t *testing.T) {
	for _, tc := range []struct {
		name, goal, description string
		direct                  bool
	}{
		{"single", "核对指定接口的鉴权响应", "仅限已登记的单一测试域名", true},
		{"long", "核对证据", strings.Repeat("长场景说明", 100), false},
		{"list", "1. 梳理入口\n2. 逐个复核\n3. 汇总证据", "保留全部范围与限制", false},
		{"bullets", "核对证据", "- 仅允许已登记域名\n- 禁止外部跳转", false},
		{"empty", "", "描述", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := &Task{Goal: tc.goal, Description: tc.description}
			summary, direct := directSeedSummary(task)
			if direct != tc.direct {
				t.Fatalf("direct=%v summary=%q", direct, summary)
			}
			if direct && (!strings.Contains(summary, tc.goal) || !strings.Contains(summary, tc.description) || !utf8.ValidString(summary)) {
				t.Fatal("direct seed lost scope or split UTF-8")
			}
			if task.Goal != tc.goal || task.Description != tc.description {
				t.Fatal("routing changed original task scope")
			}
		})
	}
}
