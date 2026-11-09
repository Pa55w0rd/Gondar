package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRepeaterArchiveBinaryHistoryRoundTrip(t *testing.T) {
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
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy-without-table=%t", legacy), func(t *testing.T) {
			task, err := d.CreateTask("synthetic repeater archive", "byte-exact restore", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer d.DeleteTask(task.ID)
			if _, err := d.Exec(`UPDATE tasks SET paused=true WHERE id=$1`, task.ID); err != nil {
				t.Fatal(err)
			}
			original := &RepeaterRun{TaskID: task.ID, URL: "http://one.invalid/", Method: "POST", Request: "POST / HTTP/1.1\r\nHost: one.invalid\r\nCookie: synthetic=retained\r\n\r\nbody", State: "running", SourceTrafficID: "source-flow"}
			if !legacy {
				if err := d.CreateRepeaterRun(context.Background(), original); err != nil {
					t.Fatal(err)
				}
				original.State = "completed"
				original.Status = 200
				original.TrafficID = "recorded-flow"
				original.ResponseBody = []byte{0, 255, 13, 10, 128}
				original.ResponseHead = "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\n"
				original.ResponseSHA256 = "synthetic-digest"
				original.Truncated = true
				if err := d.FinishRepeaterRun(context.Background(), original); err != nil {
					t.Fatal(err)
				}
			}
			archive, err := d.QueueTaskArchive(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.CreateRepeaterRun(context.Background(), &RepeaterRun{TaskID: task.ID, Method: "GET", URL: "http://one.invalid/", Request: "GET / HTTP/1.1\r\nHost: one.invalid\r\n\r\n"}); !errors.Is(err, ErrTaskArchiveState) {
				t.Fatalf("late manual request accepted: %v", err)
			}
			claimed, err := d.ClaimTaskArchiveJob(t.Context())
			if err != nil || claimed == nil || claimed.ID != archive.ID {
				t.Fatalf("claim: %+v %v", claimed, err)
			}
			snapshot, err := d.SnapshotTaskArchive(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if legacy {
				delete(snapshot.Tables, "http_repeater_runs")
				delete(snapshot.DataCounts, "http_repeater_runs")
			} else if snapshot.DataCounts["http_repeater_runs"] != 1 {
				t.Fatal("history missing in manifest")
			}
			// The real package serializes the snapshot. In particular BYTEA must survive
			// PostgreSQL's hex JSON form rather than Go's unrelated base64 JSON form.
			encoded, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			var decoded TaskArchiveSnapshot
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if err := d.CompleteTaskArchive(archive.ID, &decoded, "synthetic.tar.zst", "digest", 100, 50); err != nil {
				t.Fatal(err)
			}
			var hot int
			if err := d.QueryRow(`SELECT count(*) FROM http_repeater_runs WHERE task_id=$1`, task.ID).Scan(&hot); err != nil || hot != 0 {
				t.Fatalf("hot history not compacted: %d %v", hot, err)
			}
			if _, err := d.QueueTaskArchiveRestore(archive.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := d.RestoreTaskArchive(archive.ID, &decoded, 0); err != nil {
				t.Fatal(err)
			}
			if err := d.CompleteTaskArchiveRestore(archive.ID); err != nil {
				t.Fatal(err)
			}
			list, err := d.ListRepeaterRuns(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if legacy {
				if len(list) != 0 {
					t.Fatal("legacy package invented history")
				}
				return
			}
			if len(list) != 1 {
				t.Fatalf("restored history count %d", len(list))
			}
			got, err := d.GetRepeaterRun(task.ID, original.ID)
			if err != nil || got == nil || got.Request != original.Request || !bytes.Equal(got.ResponseBody, original.ResponseBody) || got.TrafficID != original.TrafficID || got.SourceTrafficID != original.SourceTrafficID || got.ResponseHead != original.ResponseHead || got.CompletedAt == nil || !got.Truncated {
				t.Fatalf("roundtrip changed evidence: %+v %v", got, err)
			}
			_, _ = d.Exec(`DELETE FROM task_archives WHERE id=$1`, archive.ID)
		})
	}
}

func TestRepeaterRecoveryUnblocksArchiveWithoutResending(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("synthetic interrupted Repeater", "no retry", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	if _, err := d.Exec(`UPDATE tasks SET paused=true WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	run := &RepeaterRun{TaskID: task.ID, Method: "GET", URL: "http://one.invalid/", Request: "GET / HTTP/1.1\r\nHost: one.invalid\r\n\r\n"}
	if err := d.CreateRepeaterRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := d.QueueTaskArchive(task.ID); !errors.Is(err, ErrTaskArchiveIneligible) {
		t.Fatalf("running manual operation not busy: %v", err)
	}
	if err := d.RecoverInterruptedRepeaterRuns(); err != nil {
		t.Fatal(err)
	}
	saved, err := d.GetRepeaterRun(task.ID, run.ID)
	if err != nil || saved.State != "error" || !strings.Contains(saved.Error, "不会自动重发") || saved.CompletedAt == nil {
		t.Fatalf("recovery incomplete: %+v %v", saved, err)
	}
	archive, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM task_archives WHERE id=$1`, archive.ID)
}
