package db

import (
	"fmt"
	"testing"
	"time"
)

func TestFindingReportSourceAndBodyRollbackTogetherOnLateWriteFailure(t *testing.T) {
	d, task, fid, node, conv := reportSourceFixture(t)
	if _, err := d.SetFindingReportWithSourceByNodeID(t.Context(), node, "original report", nil, reportConversationSource(conv.ID)); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("report_source_fixture_%d", time.Now().UnixNano())
	_, err := d.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN
 IF NEW.id=%d AND NEW.report='reject late write fixture' THEN RAISE EXCEPTION 'synthetic report write failure'; END IF;
 RETURN NEW; END$$`, name, fid))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DROP FUNCTION ` + name + `() CASCADE`)
	if _, err := d.Exec(`CREATE TRIGGER ` + name + ` BEFORE UPDATE ON findings FOR EACH ROW EXECUTE FUNCTION ` + name + `()`); err != nil {
		t.Fatal(err)
	}
	replacement := reportConversationSource(conv.ID)
	replacement.InvocationID = "new-host-invocation"
	if _, err := d.SetFindingReportWithSourceByNodeID(t.Context(), node, "reject late write fixture", nil, replacement); err == nil {
		t.Fatal("late report error did not abort source transaction")
	}
	f, err := d.GetFinding(fid)
	if err != nil || f.Report != "original report" || f.ReportSource == nil || f.ReportSource.InvocationID != "host-invocation" {
		t.Fatal("late body failure committed a replacement source")
	}
	var events int
	if err := d.QueryRow(`SELECT COUNT(*) FROM activity WHERE exploration_id=$1 AND kind='report_source'`, task.ExplorationID).Scan(&events); err != nil || events != 0 {
		t.Fatal("rolled back source update left a false historical receipt")
	}
}
