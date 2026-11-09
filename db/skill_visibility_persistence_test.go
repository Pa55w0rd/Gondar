package db

import (
	"slices"
	"testing"
)

// A real startup reseed must not resurrect a binding disabled through either
// public write API. This previously failed because both APIs deleted the row.
func TestSkillVisibilityOptOutSurvivesStartupSeed(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ag, err := d.GetAgentByKey("worker")
	if err != nil || ag == nil {
		t.Fatalf("worker: %v", err)
	}
	type binding struct {
		name    string
		enabled bool
	}
	rows, err := d.Query(`SELECT skill_name,enabled FROM agent_skill_visibility WHERE agent_id=$1`, ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	var saved []binding
	for rows.Next() {
		var row binding
		if err := rows.Scan(&row.name, &row.enabled); err != nil {
			t.Fatal(err)
		}
		saved = append(saved, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	defer func() {
		if _, err := d.Exec(`DELETE FROM agent_skill_visibility WHERE agent_id=$1`, ag.ID); err != nil {
			t.Error(err)
		}
		for _, row := range saved {
			if _, err := d.Exec(`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) VALUES($1,$2,$3)`, ag.ID, row.name, row.enabled); err != nil {
				t.Error(err)
			}
		}
	}()

	assertDisabled := func(t *testing.T, name string) {
		t.Helper()
		var enabled bool
		if err := d.QueryRow(`SELECT enabled FROM agent_skill_visibility WHERE agent_id=$1 AND skill_name=$2`, ag.ID, name).Scan(&enabled); err != nil {
			t.Fatalf("missing opt-out marker for %s: %v", name, err)
		}
		if enabled {
			t.Fatalf("startup re-enabled disabled skill %s", name)
		}
		visible, err := d.AgentSkillNames(ag.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range visible {
			if item == name {
				t.Fatalf("disabled skill %s still visible", name)
			}
		}
	}

	t.Run("toggle", func(t *testing.T) {
		if err := d.ToggleSkillVisibility(ag.ID, "vuln-hunting", false); err != nil {
			t.Fatal(err)
		}
		if err := d.seedBuiltinSkillVisibility(); err != nil {
			t.Fatal(err)
		}
		assertDisabled(t, "vuln-hunting")
		if err := d.ToggleSkillVisibility(ag.ID, "vuln-hunting", true); err != nil {
			t.Fatal(err)
		}
		if names, err := d.AgentSkillNames(ag.ID); err != nil || !slices.Contains(names, "vuln-hunting") {
			t.Fatalf("re-enable failed: %v, %v", names, err)
		}
	})
	t.Run("replace and clear", func(t *testing.T) {
		if err := d.SetAgentSkillVisibility(ag.ID, []string{"redteam-ops"}); err != nil {
			t.Fatal(err)
		}
		if err := d.seedBuiltinSkillVisibility(); err != nil {
			t.Fatal(err)
		}
		assertDisabled(t, "vuln-hunting")
		if err := d.SetAgentSkillVisibility(ag.ID, nil); err != nil {
			t.Fatal(err)
		}
		if err := d.seedBuiltinSkillVisibility(); err != nil {
			t.Fatal(err)
		}
		if names, err := d.AgentSkillNames(ag.ID); err != nil || len(names) != 0 {
			t.Fatalf("cleared skill grants resurrected: %v, %v", names, err)
		}
	})
}
