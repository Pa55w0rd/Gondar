package db

import "testing"

func TestPojunReportGapsUseRealBodyAndUnboundedCount(t *testing.T) {
	d, task, other, _, _ := provenanceFixture(t)
	// Put the only gaps after more than 500 real report bodies; list DTOs omit
	// report and would misclassify all of these as empty.
	if _, err := d.Exec(`INSERT INTO findings(task_id,vulnclass,severity,summary,evidence,worker,asset_ids,report)
SELECT $1,'fixture','low','fixture','synthetic','fixture','[]','complete report' FROM generate_series(1,510)`, task.ID); err != nil {
		t.Fatal(err)
	}
	gap, err := d.FindingReportGaps(task.ID)
	if err != nil || gap.Total != 0 || len(gap.SampleIDs) != 0 {
		t.Fatalf("full reports misclassified: %+v %v", gap, err)
	}
	if _, err := d.Exec(`INSERT INTO findings(task_id,vulnclass,severity,summary,evidence,worker,asset_ids,report)
SELECT $1,'fixture','low','fixture','synthetic','fixture','[]',E' \t\n\r' FROM generate_series(1,8)`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddFinding(other.ID, 0, "fixture", "other task gap", "low", "synthetic", "synthetic", "fixture", nil); err != nil {
		t.Fatal(err)
	}
	gap, err = d.FindingReportGaps(task.ID)
	if err != nil || gap.Total != 8 || len(gap.SampleIDs) != 5 {
		t.Fatalf("count/sample lost or crossed task: %+v %v", gap, err)
	}
}
