package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNextDaily(t *testing.T) {
	parse := func(v string) time.Time {
		r, e := time.Parse(time.RFC3339, v)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	cases := []struct{ after, clock, zone, want string }{
		{"2026-11-04T00:00:00Z", "09:00", "Asia/Shanghai", "2026-11-04T01:00:00Z"},
		{"2026-11-04T01:00:00Z", "09:00", "Asia/Shanghai", "2026-11-05T01:00:00Z"},
		{"2026-03-08T05:00:00Z", "02:30", "America/New_York", "2026-03-09T06:30:00Z"},
		{"2026-11-01T04:00:00Z", "01:30", "America/New_York", "2026-11-01T05:30:00Z"},
		{"2026-11-01T05:31:00Z", "01:30", "America/New_York", "2026-11-02T06:30:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.after+tc.zone, func(t *testing.T) {
			got, err := NextDaily(parse(tc.after), tc.clock, tc.zone)
			if err != nil || !got.Equal(parse(tc.want)) {
				t.Fatalf("got %v %v want %s", got, err, tc.want)
			}
		})
	}
	for _, v := range []struct{ clock, zone string }{{"25:00", "UTC"}, {"9:00", "UTC"}, {"09:00", "Local"}, {"09:00", "invalid/zone"}} {
		if _, err := NextDaily(time.Now(), v.clock, v.zone); err == nil {
			t.Fatalf("invalid plan accepted: %#v", v)
		}
	}
}

func TestDailyScheduleAtomicQueueSnapshotAndCancel(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	source, err := d.CreateTaskWithOptions("https://one.example.invalid", "检查指定目标", TaskCreateOptions{
		TimeoutSeconds: 300, AuthConfig: &TaskAuthConfig{Enabled: true, Username: "test", Password: "synthetic-schedule-secret", LoginURL: "https://one.example.invalid/login"},
		InterceptRules: []TaskInterceptRuleInput{{Enabled: true, Action: "block", Kind: "exact_domain", Pattern: "other.example.invalid", Note: "明确排除"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM tasks WHERE id=$1`, source.ID)
	_, err = d.Exec(`INSERT INTO task_scope(task_id,kind,domain,source) VALUES($1,'subdomain','one.example.invalid','manual')`, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	plan, err := d.CreateDailySchedule(source.ID, "09:00", "Asia/Shanghai", 3, 600, now)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "synthetic-schedule-secret") || strings.Contains(string(wire), "snapshot") {
		t.Fatal("private snapshot escaped")
	}
	// The original can change after creation without silently changing the plan.
	if _, err := d.Exec(`UPDATE tasks SET description='different target' WHERE id=$1`, source.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan *Task, 6)
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := d.MaterializeDailySchedule(plan.NextRunAt.Add(time.Second))
			results <- v
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var created *Task
	count := 0
	for v := range results {
		if v != nil {
			count++
			created = v
		}
	}
	if count != 1 {
		t.Fatalf("concurrent due occurrence created %d tasks", count)
	}
	defer d.Exec(`DELETE FROM tasks WHERE id=$1`, created.ID)
	persisted, err := d.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Description != source.Description || !persisted.Queued || persisted.Paused || persisted.QueueMode != "bootstrap" || persisted.TimeoutSeconds != 600 || persisted.AuthConfig.Password != "synthetic-schedule-secret" {
		t.Fatalf("snapshot/queue lost: %+v", persisted)
	}
	var rules, scopes, occurrences int
	if err := d.QueryRow(`SELECT count(*) FROM task_intercept_rules WHERE task_id=$1`, created.ID).Scan(&rules); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM task_scope WHERE task_id=$1 AND domain='one.example.invalid'`, created.ID).Scan(&scopes); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM daily_task_occurrences WHERE schedule_id=$1`, plan.ID).Scan(&occurrences); err != nil {
		t.Fatal(err)
	}
	if rules != 1 || scopes != 1 || occurrences != 1 {
		t.Fatalf("lost configuration/occurrence: %d %d %d", rules, scopes, occurrences)
	}
	// A still-running prior task skips the next day instead of overlapping.
	list, err := d.ListDailySchedules(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := d.MaterializeDailySchedule(list[0].NextRunAt.Add(time.Second)); err != nil || v != nil {
		t.Fatalf("overlap: %v %v", v, err)
	}
	list, err = d.ListDailySchedules(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Skipped != 1 || list[0].RunsCreated != 1 {
		t.Fatalf("skip state: %+v", list[0])
	}
	if err := d.CancelDailySchedule(source.ID, plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.SetStatus(created.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if v, err := d.MaterializeDailySchedule(list[0].NextRunAt.Add(48 * time.Hour)); err != nil || v != nil {
		t.Fatalf("canceled plan resurrected: %v %v", v, err)
	}
}

func TestDailyScheduleFiniteBudgetAndDeletedSource(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	for _, runs := range []int{0, 367} {
		if _, err := d.CreateDailySchedule(1, "08:00", "UTC", runs, 600, now); err == nil {
			t.Fatal("unbounded run count accepted")
		}
	}
	source, err := d.CreateTask("daily-budget", "one target", nil, 300, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM tasks WHERE id=$1`, source.ID)
	p, err := d.CreateDailySchedule(source.ID, "08:00", "UTC", 1, 120, now)
	if err != nil {
		t.Fatal(err)
	}
	task, err := d.MaterializeDailySchedule(p.NextRunAt)
	if err != nil || task == nil {
		t.Fatalf("first occurrence: %v %v", task, err)
	}
	defer d.Exec(`DELETE FROM tasks WHERE id=$1`, task.ID)
	list, err := d.ListDailySchedules(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Enabled || list[0].RunsCreated != 1 {
		t.Fatal("finite plan remains enabled")
	}
	if _, err := d.CreateDailySchedule(source.ID, "08:00", "UTC", 2, 120, now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, source.ID); err != nil {
		t.Fatal(err)
	}
	if next, err := d.MaterializeDailySchedule(now.Add(72 * time.Hour)); err != nil || next != nil {
		t.Fatal(fmt.Sprintf("deleted source generated a task: %v %v", next, err))
	}
}
