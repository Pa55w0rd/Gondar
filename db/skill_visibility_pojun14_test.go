package db

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/Autumn-27/norma/skill"
)

var pojun14SkillNames = []string{
	"pentest-verification", "source-code-hunting", "api-security-testing", "ai-llm-app-attack",
	"component-vuln-intel", "pentest-output-standards", "redteam-opsec", "web-attack-methods",
}

func TestPojun14SkillsHaveWorkingRoleDefaults(t *testing.T) {
	reg, err := skill.LoadDir(shippedSkillDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range pojun14SkillNames {
		t.Run(name, func(t *testing.T) {
			loaded, ok := reg.Get(name)
			if !ok || loaded.Description == "" || filepath.Base(loaded.Dir) != name {
				t.Fatalf("技能不能由实际loader发现：%s", name)
			}
			roles := builtinSkillVisibility[name]
			for _, role := range []string{"planner", "worker", "pentest"} {
				if !slices.Contains(roles, role) {
					t.Errorf("新增技能未给实际工作角色%s绑定", role)
				}
			}
			for _, role := range []string{"goals", "auto"} {
				if slices.Contains(roles, role) {
					t.Errorf("不应将测试专项默认给平台/拆目标角色%s", role)
				}
			}
			if got := slices.Contains(roles, "mainagent"); got != (name == "pentest-output-standards") {
				t.Errorf("主控应只默认获取交付整理卡：%s %v", name, roles)
			}
		})
	}
}

func TestPojun14SkillUpgradePreservesOptOut(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	type binding struct {
		agentID int64
		name    string
		enabled bool
	}
	var saved []binding
	for _, name := range pojun14SkillNames {
		rows, err := d.Query(`SELECT agent_id,enabled FROM agent_skill_visibility WHERE skill_name=$1`, name)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			row := binding{name: name}
			if err := rows.Scan(&row.agentID, &row.enabled); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			saved = append(saved, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	}
	defer func() {
		for _, name := range pojun14SkillNames {
			if _, err := d.Exec(`DELETE FROM agent_skill_visibility WHERE skill_name=$1`, name); err != nil {
				t.Error(err)
			}
		}
		for _, row := range saved {
			if _, err := d.Exec(`INSERT INTO agent_skill_visibility(agent_id,skill_name,enabled) VALUES($1,$2,$3)`, row.agentID, row.name, row.enabled); err != nil {
				t.Error(err)
			}
		}
	}()
	// An upgrade from 1.3 has no new rows, except any manually pre-created
	// explicit opt-out. Repeated startup seeds must add missing defaults only.
	for _, name := range pojun14SkillNames {
		if _, err := d.Exec(`DELETE FROM agent_skill_visibility WHERE skill_name=$1`, name); err != nil {
			t.Fatal(err)
		}
	}
	worker, err := d.GetAgentByKey("worker")
	if err != nil || worker == nil {
		t.Fatalf("worker不存在：%v", err)
	}
	if err := d.ToggleSkillVisibility(worker.ID, "pentest-verification", false); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := d.seedBuiltinSkillVisibility(); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range pojun14SkillNames {
		for _, role := range builtinSkillVisibility[name] {
			a, err := d.GetAgentByKey(role)
			if err != nil || a == nil {
				t.Fatalf("默认角色不存在：%s，%v", role, err)
			}
			var enabled bool
			if err := d.QueryRow(`SELECT enabled FROM agent_skill_visibility WHERE agent_id=$1 AND skill_name=$2`, a.ID, name).Scan(&enabled); err != nil {
				t.Fatalf("升级未实际绑定%s→%s：%v", name, role, err)
			}
			want := role != "worker" || name != "pentest-verification"
			if enabled != want {
				t.Errorf("升级覆盖显式关闭：%s→%s=%v", name, role, enabled)
			}
		}
		if err := d.ToggleSkillVisibility(worker.ID, name, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.seedBuiltinSkillVisibility(); err != nil {
		t.Fatal(err)
	}
	visible, err := d.AgentSkillNames(worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range pojun14SkillNames {
		if slices.Contains(visible, name) {
			t.Errorf("关闭技能在再次启动后复活：%s", name)
		}
	}
}
