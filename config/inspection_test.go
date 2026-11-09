package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectionUsesExplicitBoundedFileWithoutChangingEnvironment(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "inspected.json")
	t.Setenv("GONDAR_CONFIG", "unchanged-config-path")
	t.Setenv("GONDAR_PG_DSN", "")
	if err := os.WriteFile(filename, []byte(`{"database":{"host":"localhost","user":"fixture","dbname":"fixture"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := InspectionPostgresDSN(filename); got != "postgres://fixture@localhost:5432/fixture?sslmode=disable" {
		t.Fatal("inspection did not follow the existing field configuration semantics")
	}
	if os.Getenv("GONDAR_CONFIG") != "unchanged-config-path" {
		t.Fatal("inspection changed the service configuration path")
	}
	_ = os.WriteFile(filename, []byte(strings.Repeat(" ", (1<<20)+1)), 0o600)
	if InspectionPostgresDSN(filename) != "" || InspectionPostgresDSN(dir) != "" {
		t.Fatal("inspection accepted an oversized or non-regular configuration")
	}
	t.Setenv("GONDAR_PG_DSN", "postgres://environment/fixture")
	if InspectionPostgresDSN(filename) != "postgres://environment/fixture" {
		t.Fatal("inspection did not honor environment DSN precedence")
	}
}
