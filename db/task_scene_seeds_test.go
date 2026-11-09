package db

import (
	"errors"
	"strings"
	"testing"
)

func TestBuiltinTaskSceneCatalog(t *testing.T) {
	if len(builtinTaskScenes) != 8 {
		t.Fatalf("scene count=%d", len(builtinTaskScenes))
	}
	keys := map[string]bool{}
	for _, scene := range builtinTaskScenes {
		if keys[scene.key] {
			t.Fatalf("duplicate key %s", scene.key)
		}
		keys[scene.key] = true
		if !strings.Contains(scene.description, "{{target}}") || !strings.Contains(scene.goal, "{{target}}") {
			t.Fatalf("scene %s has no target slot", scene.key)
		}
		if scene.preset.TimeoutSeconds <= 0 || scene.preset.MaxTargets < 1 {
			t.Fatalf("scene %s has invalid defaults", scene.key)
		}
		if _, _, err := normalizeTaskTemplateInput(TaskTemplateInput{Name: scene.name, Description: scene.description, Goal: scene.goal}); err != nil {
			t.Fatal(err)
		}
		if scene.preset.SeedFirstIntent != (scene.key == "ctf") {
			t.Fatalf("unexpected direct-start mode for %s", scene.key)
		}
	}
}

func TestBuiltinTaskSceneUpgradeIdempotentAndUserSafe(t *testing.T) {
	dsn := testDSN(t)
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	list := func(db *DB) map[string]*TaskTemplate {
		t.Helper()
		rows, err := db.ListTaskTemplates()
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*TaskTemplate{}
		for _, row := range rows {
			if row.BuiltinKey != "" {
				out[row.BuiltinKey] = row
			}
		}
		if len(out) != 8 {
			t.Fatalf("builtin count=%d", len(out))
		}
		return out
	}
	initial := list(d)
	if err := d.seedBuiltinTaskScenes(); err != nil {
		t.Fatal(err)
	}
	for key, row := range list(d) {
		if row.ID != initial[key].ID || !row.UpdatedAt.Equal(initial[key].UpdatedAt) {
			t.Fatalf("restart changed %s", key)
		}
		if row.Scene == nil || row.Scene.TimeoutSeconds <= 0 {
			t.Fatalf("scene defaults missing for %s", key)
		}
	}
	goal := "attempted overwrite"
	if _, err := d.PatchTaskTemplate(initial["single_web"].ID, TaskTemplatePatch{Goal: &goal}); !errors.Is(err, ErrTaskTemplateBuiltin) {
		t.Fatalf("builtin update error=%v", err)
	}
	if _, err := d.DeleteTaskTemplate(initial["single_web"].ID); !errors.Is(err, ErrTaskTemplateBuiltin) {
		t.Fatalf("builtin delete error=%v", err)
	}

	// Simulate an older/partial installation plus a legacy custom-name collision.
	if _, err := d.Exec(`DELETE FROM task_templates WHERE builtin_key='ctf'`); err != nil {
		t.Fatal(err)
	}
	custom, err := d.CreateTaskTemplate(TaskTemplateInput{Name: "builtin:ctf", Description: "user description", Goal: "user goal"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = d.DeleteTaskTemplate(custom.ID) }()
	if _, err := d.Exec(`UPDATE task_templates SET description='older builtin revision' WHERE builtin_key='source_audit'`); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	after := list(d2)
	if after["ctf"].ID == initial["ctf"].ID || !after["ctf"].Scene.SeedFirstIntent {
		t.Fatal("missing CTF scene was not reseeded")
	}
	if after["single_web"].ID != initial["single_web"].ID || after["source_audit"].Description == "older builtin revision" {
		t.Fatal("upgrade did not preserve identity/update managed prompt")
	}
	user, err := d2.GetTaskTemplate(custom.ID)
	if err != nil || user == nil || user.Description != "user description" || user.Goal != "user goal" || user.BuiltinKey != "" || user.Scene != nil {
		t.Fatalf("user template overwritten: %+v %v", user, err)
	}
}
