package db

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func reportRecoveryFixture(t *testing.T) (*DB, *Task, int64, time.Time) {
	t.Helper()
	dsn := os.Getenv("GONDAR_PG_DSN")
	if dsn == "" {
		t.Skip("报告恢复集成回归需要显式专用 GONDAR_PG_DSN")
	}
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	task, err := d.CreateTask("report delivery fixture", "仅合成证据，不访问目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID)
		d.DeleteTaskCascade(task.ID, true, true)
	})
	fid, err := d.AddFinding(task.ID, 0, "fixture", "合成发现", "low", "合成摘要", "已保存的合成命令输出", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := d.Exec(`UPDATE findings SET created_at=$2 WHERE id=$1`, fid, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	return d, task, fid, now
}

func mustReportClaim(t *testing.T, d *DB, now time.Time) *FindingReportRecovery {
	t.Helper()
	job, err := d.ClaimFindingReportRecovery(t.Context(), now, 30*time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim=%+v error=%v", job, err)
	}
	return job
}

func TestReportRecoveryClaimFiltersRealEmptyBodiesAndTaskLifecycle(t *testing.T) {
	for _, scenario := range []string{"empty", "whitespace", "one_character", "young", "paused", "queued", "deleted", "archiving", "false_positive"} {
		t.Run(scenario, func(t *testing.T) {
			d, task, fid, now := reportRecoveryFixture(t)
			var err error
			switch scenario {
			case "whitespace":
				_, err = d.Exec(`UPDATE findings SET report=E' \t\n\r' WHERE id=$1`, fid)
			case "one_character":
				_, err = d.Exec(`UPDATE findings SET report='短' WHERE id=$1`, fid)
			case "young":
				_, err = d.Exec(`UPDATE findings SET created_at=$2 WHERE id=$1`, fid, now)
			case "paused":
				err = d.SetPaused(task.ID, true)
			case "queued":
				_, err = d.Exec(`UPDATE tasks SET queued=true WHERE id=$1`, task.ID)
			case "deleted":
				_, err = d.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, task.ID)
			case "archiving":
				if err = d.SetPaused(task.ID, true); err == nil {
					_, err = d.QueueTaskArchive(task.ID)
				}
				if err == nil {
					_, err = d.Exec(`UPDATE tasks SET paused=false WHERE id=$1`, task.ID)
				}
			case "false_positive":
				_, err = d.SetFindingStatus(fid, FindingFalsePositive)
			}
			if err != nil {
				t.Fatal(err)
			}
			job, err := d.ClaimFindingReportRecovery(t.Context(), now, 30*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			want := scenario == "empty" || scenario == "whitespace"
			if (job != nil) != want {
				t.Fatalf("claim=%+v, want eligible=%t", job, want)
			}
			if job != nil && (job.FindingID != fid || job.TaskID != task.ID || job.NodeID != 0) {
				t.Fatalf("mixed IDs: %+v", job)
			}
		})
	}
}

func TestReportRecoveryConcurrentClaimAndSingleCommit(t *testing.T) {
	d, _, fid, now := reportRecoveryFixture(t)
	start := make(chan struct{})
	jobs := make(chan *FindingReportRecovery, 12)
	errs := make(chan error, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			job, err := d.ClaimFindingReportRecovery(t.Context(), now, 30*time.Minute)
			if err != nil {
				errs <- err
			} else if job != nil {
				jobs <- job
			}
		}()
	}
	close(start)
	wg.Wait()
	close(jobs)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("concurrent claims=%d", len(jobs))
	}
	job := <-jobs
	if job.FindingID != fid || job.Attempt != 1 || job.Token == "" {
		t.Fatal(job)
	}
	errs = make(chan error, 2)
	for _, report := range []string{"第一份合成报告", "第二份合成报告"} {
		wg.Add(1)
		go func(report string) {
			defer wg.Done()
			errs <- d.CompleteFindingReportRecovery(t.Context(), job, report, now.Add(time.Minute))
		}(report)
	}
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrFindingReportRecoveryStale) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("successful commits=%d", success)
	}
	if job, err := d.ClaimFindingReportRecovery(t.Context(), now.Add(24*time.Hour), time.Hour); err != nil || job != nil {
		t.Fatalf("saved report reclaimed: %+v %v", job, err)
	}
}

func TestReportRecoverySnapshotCASPreservesConcurrentChanges(t *testing.T) {
	for _, change := range []string{"report", "evidence_text", "traffic", "artifact", "triage", "paused", "deleted"} {
		t.Run(change, func(t *testing.T) {
			d, task, fid, now := reportRecoveryFixture(t)
			job := mustReportClaim(t, d, now)
			var err error
			switch change {
			case "report":
				_, err = d.Exec(`UPDATE findings SET report='人工短报告' WHERE id=$1`, fid)
			case "evidence_text":
				_, err = d.Exec(`UPDATE findings SET evidence='新证据' WHERE id=$1`, fid)
			case "traffic":
				_, err = d.Exec(`UPDATE findings SET evidence_version=evidence_version+1 WHERE id=$1`, fid)
			case "artifact":
				_, err = d.Exec(`UPDATE findings SET artifact_evidence_version=artifact_evidence_version+1 WHERE id=$1`, fid)
			case "triage":
				_, err = d.SetFindingStatus(fid, FindingFalsePositive)
			case "paused":
				err = d.SetPaused(task.ID, true)
			case "deleted":
				_, err = d.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, task.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			valid, renewErr := d.RenewFindingReportRecovery(t.Context(), job, now.Add(time.Minute), time.Hour)
			if valid || (renewErr != nil && !errors.Is(renewErr, ErrTaskArchiveState)) {
				t.Fatalf("changed claim renewed: %t %v", valid, renewErr)
			}
			err = d.CompleteFindingReportRecovery(t.Context(), job, "过期补写不得覆盖", now.Add(time.Minute))
			if !errors.Is(err, ErrFindingReportRecoveryStale) && !errors.Is(err, ErrTaskArchiveState) {
				t.Fatalf("stale commit=%v", err)
			}
			f, err := d.GetFinding(fid)
			if err != nil || f.Report == "过期补写不得覆盖" {
				t.Fatalf("changed result overwritten: %+v %v", f, err)
			}
			if change == "report" && f.Report != "人工短报告" {
				t.Fatal("human report lost")
			}
		})
	}
}

func TestReportRecoveryPersistentBudgetAndLeaseFence(t *testing.T) {
	d, _, _, now := reportRecoveryFixture(t)
	old := mustReportClaim(t, d, now)
	// A second process sees the persisted claim/next_at; no restart-local reset.
	second, err := Open(os.Getenv("GONDAR_PG_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if job, err := second.ClaimFindingReportRecovery(t.Context(), now.Add(31*time.Minute), time.Hour); err != nil || job != nil {
		t.Fatalf("crash cooldown bypassed: %+v %v", job, err)
	}
	now = now.Add(91 * time.Minute)
	job := mustReportClaim(t, second, now)
	if job.Attempt != 2 || job.Token == old.Token {
		t.Fatal(job)
	}
	if err := d.CompleteFindingReportRecovery(t.Context(), old, "old process", now); !errors.Is(err, ErrFindingReportRecoveryStale) {
		t.Fatalf("old lease committed: %v", err)
	}
	for {
		if err := second.FailFindingReportRecovery(t.Context(), job, "report_model_error", now); err != nil {
			t.Fatal(err)
		}
		next := now.Add(FindingReportRecoveryBackoff(job.Attempt))
		if got, err := second.ClaimFindingReportRecovery(t.Context(), next.Add(-time.Second), time.Hour); err != nil || got != nil {
			t.Fatalf("backoff bypassed: %+v %v", got, err)
		}
		if job.Attempt == FindingReportRecoveryMaxAttempts {
			if got, err := second.ClaimFindingReportRecovery(t.Context(), next.Add(24*time.Hour), time.Hour); err != nil || got != nil {
				t.Fatalf("retry limit reset: %+v %v", got, err)
			}
			break
		}
		now = next
		job = mustReportClaim(t, second, now)
	}
}

func TestReportRecoveryArchiveDefaultsAndLeaseReset(t *testing.T) {
	d, task, fid, now := reportRecoveryFixture(t)
	for _, ensure := range []func() error{d.EnsureLLMRecordsTable, d.EnsureLLMUsageTable} {
		if err := ensure(); err != nil {
			t.Fatal(err)
		}
	}
	job := mustReportClaim(t, d, now)
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	archive, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := decodeArchiveRows(snapshot.Tables["findings"])
	if err != nil || len(rows) != 1 || rows[0]["report_recovery_token"] != job.Token {
		t.Fatalf("claim missing from archive: %+v %v", rows, err)
	}
	if err := d.CompleteTaskArchive(archive.ID, snapshot, "synthetic-report-archive.tar.zst", "fixture", 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.QueueTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RestoreTaskArchive(archive.ID, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	var attempt int
	var token string
	var lease *time.Time
	if err := d.QueryRow(`SELECT report_recovery_attempts,report_recovery_token,report_recovery_lease_until FROM findings WHERE id=$1`, fid).Scan(&attempt, &token, &lease); err != nil {
		t.Fatal(err)
	}
	if attempt != 1 || token != "" || lease != nil {
		t.Fatalf("archive restored a live claim or reset budget: %d %q %v", attempt, token, lease)
	}
	if err := d.CompleteTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	if next, err := d.ClaimFindingReportRecovery(t.Context(), now.Add(48*time.Hour), time.Hour); err != nil || next != nil {
		t.Fatalf("paused restored task auto-delivered: %+v %v", next, err)
	}
	if err := d.CompleteFindingReportRecovery(t.Context(), job, "old process", now.Add(time.Minute)); err == nil {
		t.Fatal("pre-archive worker committed after restore")
	}
	legacy, err := normalizeArchivedFindingVersions(json.RawMessage(`[{"id":1,"report":"旧报告"}]`))
	if err != nil {
		t.Fatal(err)
	}
	var defaults []map[string]any
	if err := json.Unmarshal(legacy, &defaults); err != nil {
		t.Fatal(err)
	}
	if defaults[0]["report_recovery_attempts"] != float64(0) || defaults[0]["report_recovery_token"] != "" || defaults[0]["report_recovery_error"] != "" {
		t.Fatal(string(legacy))
	}
}
