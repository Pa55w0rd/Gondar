package db

import "testing"

func TestMarkTaskExecutingRespectsAdmissionAndTerminal(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, tc := range []struct {
		status                        string
		paused, queued, deleted, want bool
	}{
		{status: "created", want: true}, {status: "created", paused: true}, {status: "created", queued: true}, {status: "created", deleted: true},
		{status: "done"}, {status: "failed"}, {status: "timeout"}, {status: "running"},
	} {
		task, err := d.CreateTask("execution-state fixture", "synthetic", nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.DeleteTask(task.ID) })
		if _, err := d.Exec(`UPDATE tasks SET status=$1,paused=$2,queued=$3,deleted_at=CASE WHEN $4 THEN now() ELSE NULL END WHERE id=$5`, tc.status, tc.paused, tc.queued, tc.deleted, task.ID); err != nil {
			t.Fatal(err)
		}
		changed, err := d.MarkTaskExecuting(task.ID)
		if err != nil || changed != tc.want {
			t.Fatalf("case=%+v changed=%v err=%v", tc, changed, err)
		}
		var status string
		if err := d.QueryRow(`SELECT status FROM tasks WHERE id=$1`, task.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		want := tc.status
		if tc.want {
			want = "running"
		}
		if status != want {
			t.Fatalf("case=%+v status=%s want=%s", tc, status, want)
		}
	}
}
