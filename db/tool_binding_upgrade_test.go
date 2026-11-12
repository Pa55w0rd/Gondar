package db

import (
	"database/sql"
	"encoding/json"
	"testing"
)

func TestWorkerFactReaderUpgradePreservesUserChoices(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var original []byte
	if err := d.QueryRow(`SELECT to_jsonb(t) FROM tools t WHERE key='list_facts'`).Scan(&original); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	oldFlag, hadFlag, err := d.GetSetting("worker_fact_reader_v030")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		d.Exec(`DELETE FROM tools WHERE key='list_facts'`)
		if len(original) > 0 {
			d.Exec(`INSERT INTO tools SELECT (jsonb_populate_record(NULL::tools,$1)).*`, string(original))
		}
		d.Exec(`DELETE FROM settings WHERE key='worker_fact_reader_v030'`)
		if hadFlag {
			d.SetSetting("worker_fact_reader_v030", oldFlag)
		}
	}()
	for _, tc := range []struct {
		name, agents  string
		enabled, want bool
	}{
		{"previous default", `["mainagent","planner"]`, true, true},
		{"disabled", `["mainagent","planner"]`, false, false},
		{"custom binding", `["planner"]`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d.Exec(`DELETE FROM settings WHERE key='worker_fact_reader_v030'`)
			d.Exec(`DELETE FROM tools WHERE key='list_facts'`)
			if _, err := d.Exec(`INSERT INTO tools(key,system,description,schema,agents,enabled) VALUES('list_facts',true,'fixture','{}',$1,$2)`, tc.agents, tc.enabled); err != nil {
				t.Fatal(err)
			}
			if err := d.UpgradeWorkerFactReader(); err != nil {
				t.Fatal(err)
			}
			var bound bool
			if err := d.QueryRow(`SELECT agents ? 'worker' FROM tools WHERE key='list_facts'`).Scan(&bound); err != nil || bound != tc.want {
				t.Fatalf("binding changed incorrectly: %v %v", bound, err)
			}
			if tc.want {
				if _, err := d.Exec(`UPDATE tools SET agents=agents-'worker' WHERE key='list_facts'`); err != nil {
					t.Fatal(err)
				}
				if err := d.UpgradeWorkerFactReader(); err != nil {
					t.Fatal(err)
				}
				var agents string
				if err := d.QueryRow(`SELECT agents FROM tools WHERE key='list_facts'`).Scan(&agents); err != nil {
					t.Fatal(err)
				}
				var entries []string
				if err := json.Unmarshal([]byte(agents), &entries); err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if entry == "worker" {
						t.Fatal("restart overrode explicit removal")
					}
				}
			}
		})
	}
}
