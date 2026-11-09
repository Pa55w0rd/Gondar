package db

import (
	"context"
	"errors"
	"testing"
)

func TestRepeaterQuotaSerializesLastSlot(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("synthetic Repeater count quota", "never delete evidence", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	if _, err := d.Exec(`INSERT INTO http_repeater_runs(task_id,url,method,request,state) SELECT $1,'http://one.invalid/','GET','synthetic','completed' FROM generate_series(1,$2::int)`, task.ID, RepeaterRunLimit-1); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-ready
			results <- d.CreateRepeaterRun(context.Background(), &RepeaterRun{TaskID: task.ID, URL: "http://one.invalid/", Method: "GET", Request: "synthetic new"})
		}()
	}
	close(ready)
	accepted, rejected := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			accepted++
		} else if errors.Is(err, ErrRepeaterQuota) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	usage, err := d.RepeaterHistoryUsage(task.ID)
	if err != nil || accepted != 1 || rejected != 1 || usage.Count != RepeaterRunLimit {
		t.Fatalf("count quota race: accepted=%d rejected=%d usage=%+v err=%v", accepted, rejected, usage, err)
	}
}

func TestRepeaterQuotaReservesRunningResponseAndReleasesAtFinish(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("synthetic Repeater byte quota", "reserve before target send", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	// 126 saved 256KiB bodies occupy 31.5MiB logical message bytes. A pending
	// 320KiB response reservation fits; two reservations must be rejected.
	if _, err := d.Exec(`INSERT INTO http_repeater_runs(task_id,url,method,request,state,response_body) SELECT $1,'http://one.invalid/','GET','synthetic','completed',convert_to(repeat('x',$2::int),'UTF8') FROM generate_series(1,126)`, task.ID, RepeaterResponseLimit); err != nil {
		t.Fatal(err)
	}
	newRun := func() *RepeaterRun {
		return &RepeaterRun{TaskID: task.ID, URL: "http://one.invalid/", Method: "GET", Request: "synthetic new"}
	}
	first := newRun()
	if err := d.CreateRepeaterRun(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	before, err := d.RepeaterHistoryUsage(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CreateRepeaterRun(context.Background(), newRun()); !errors.Is(err, ErrRepeaterQuota) {
		t.Fatalf("unreserved future response: %v", err)
	}
	first.State = "completed"
	if err := d.FinishRepeaterRun(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	after, err := d.RepeaterHistoryUsage(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.ReservedBytes-after.ReservedBytes != RepeaterResponseLimit+RepeaterResponseHeadLimit {
		t.Fatalf("reservation not released: before=%+v after=%+v", before, after)
	}
	if err := d.CreateRepeaterRun(context.Background(), newRun()); err != nil {
		t.Fatal(err)
	}
	usage, err := d.RepeaterHistoryUsage(task.ID)
	if err != nil || usage.ReservedBytes > RepeaterStorageLimit || usage.Count != 128 {
		t.Fatalf("quota lost evidence or overshot: %+v %v", usage, err)
	}
}
