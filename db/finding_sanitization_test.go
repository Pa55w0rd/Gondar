package db

import (
	"testing"
)

func TestRecordFindingSanitizesUntrustedText(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTask("finding text encoding fixture", "local fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	recorded, err := d.Exploration(task.ExplorationID).RecordFinding(t.Context(), RecordFindingInput{
		TaskID: task.ID, ExplorationID: task.ExplorationID,
		Name: "na\x00me\xff", Severity: "lo\x00w", VulnClass: "test",
		Summary: "su\x00mmary\xff", Evidence: "ev\x00idence\xff", Worker: "wo\x00rker\xff",
	})
	if err != nil {
		t.Fatalf("untrusted text discarded the entire finding: %v", err)
	}
	var name, summary, evidence, worker string
	if err := d.QueryRow(`SELECT name,summary,evidence,worker FROM findings WHERE id=$1`, recorded.FindingID).Scan(&name, &summary, &evidence, &worker); err != nil {
		t.Fatal(err)
	}
	if name != "name�" || summary != "summary�" || evidence != "evidence�" || worker != "worker�" {
		t.Fatalf("unexpected stored text: %q / %q / %q / %q", name, summary, evidence, worker)
	}
	var body string
	if err := d.QueryRow(`SELECT payload->'evidence'->>'poc' FROM exploration_nodes WHERE id=$1`, recorded.NodeID).Scan(&body); err != nil || body != evidence {
		t.Fatalf("finding and exploration evidence differ: %q, err=%v", body, err)
	}
}
