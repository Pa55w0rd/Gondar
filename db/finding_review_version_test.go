package db

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestRetestVersionGuardPreservesConcurrentChanges(t *testing.T) {
	for _, change := range []string{"triage", "traffic", "report"} {
		t.Run(change, func(t *testing.T) {
			d, fid := retestDB(t)
			r, c, _, err := d.CreateFindingRetest(t.Context(), fid, "检查原始版本")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.StartFindingRetest(t.Context(), r.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.RecordFindingRetestResult(t.Context(), c.ID, "fixed", "本次检查已完成", "本次实际输出"); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "triage":
				_, err = d.SetFindingStatus(fid, FindingFalsePositive)
			case "traffic":
				_, err = d.Exec(`UPDATE findings SET evidence_version=evidence_version+1 WHERE id=$1`, fid)
			case "report":
				_, err = d.Exec(`UPDATE findings SET report='人工补充的新报告' WHERE id=$1`, fid)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := d.FinishFindingRetest(r.ID, "completed", ""); err != nil {
				t.Fatal(err)
			}
			f, err := d.GetFinding(fid)
			if err != nil {
				t.Fatal(err)
			}
			if f.Status == FindingFixed {
				t.Fatal("late assessment marked changed finding fixed")
			}
			if change == "triage" && f.Status != FindingFalsePositive {
				t.Fatal("human triage overwritten")
			}
			full, err := d.FindingRetestForConversation(t.Context(), c.ID)
			if err != nil || full.Status != "stopped" || !strings.Contains(full.Error, "已变化") {
				t.Fatalf("missing stale version result: %+v %v", full, err)
			}
		})
	}
}

func TestRetestArtifactVersionIgnoresThumbnailsButRejectsChangedEvidence(t *testing.T) {
	d, fid := retestDB(t)
	if err := d.EnsureTaskArtifactsTable(); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask("attachment version fixture", "synthetic local evidence", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTaskCascade(task.ID, true, true) })
	if _, err := d.Exec(`UPDATE findings SET task_id=$2 WHERE id=$1`, fid, task.ID); err != nil {
		t.Fatal(err)
	}
	a := &TaskArtifact{TaskID: strconv.FormatInt(task.ID, 10), FindingID: fid, Name: "proof.txt", Kind: "log", Storage: "synthetic-proof.txt", SHA256: "original-sha", Note: "原始证据"}
	if err := d.InsertTaskArtifact(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM task_artifacts WHERE id=$1`, a.ID) })
	for _, change := range []bool{false, true} {
		if _, err := d.SetFindingStatus(fid, FindingPending); err != nil {
			t.Fatal(err)
		}
		r, c, _, err := d.CreateFindingRetest(t.Context(), fid, "附件版本检查")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.StartFindingRetest(t.Context(), r.ID); err != nil {
			t.Fatal(err)
		}
		if err := d.RecordFindingRetestResult(t.Context(), c.ID, "fixed", "受控复核", "受控实际检查"); err != nil {
			t.Fatal(err)
		}
		if change {
			_, err = d.Exec(`UPDATE task_artifacts SET note='原始证据已重新说明' WHERE id=$1`, a.ID)
		} else {
			err = d.SetTaskArtifactPreview(a.ID, "ready", 3, "")
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := d.FinishFindingRetest(r.ID, "completed", ""); err != nil {
			t.Fatal(err)
		}
		full, err := d.FindingRetestForConversation(t.Context(), c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if change && full.Status != "stopped" {
			t.Fatal("changed attachment did not invalidate result")
		}
		if !change && full.Status != "completed" {
			t.Fatal("thumbnail generation invalidated unchanged evidence")
		}
	}
}
