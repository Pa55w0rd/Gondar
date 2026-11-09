package server

import (
	"strconv"
	"testing"
)

func TestPojunSettleReportsOnlyActualMissingBodies(t *testing.T) {
	for _, report := range []string{"", "complete synthetic report"} {
		t.Run("report="+report, func(t *testing.T) {
			m, err := NewManager(t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			task, err := m.CreateTask("delivery gap fixture", "synthetic", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) }()
			tid, _ := strconv.ParseInt(task.ID, 10, 64)
			fid, err := m.pg.AddFinding(tid, 0, "fixture", "synthetic finding", "low", "summary", "synthetic", "fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.pg.Exec(`UPDATE findings SET report=$2 WHERE id=$1`, fid, report); err != nil {
				t.Fatal(err)
			}
			e := NewEngine(m)
			e.settleTaskWithReason(t.Context(), task, "goal_met", "synthetic completion")
			var count int
			if err := m.pg.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1 AND worker='system' AND kind='text' AND summary LIKE '收尾交付提示%'`, task.Store.ID()).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if report == "" {
				want = 1
			}
			if count != want || m.TaskStatus(task.ID) != "done" {
				t.Fatalf("gap hint changed settlement/misreported body: count=%d status=%s", count, m.TaskStatus(task.ID))
			}
		})
	}
}
