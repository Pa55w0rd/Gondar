package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gondar/hosttools"
)

func TestDoctorTemplateDoesNotBootstrapDeployment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GONDAR_CONFIG", filepath.Join(dir, "missing-config.json"))
	t.Setenv("GONDAR_PG_DSN", "invalid connection must never be used by the template")
	var out, diagnostics bytes.Buffer
	if code := runDoctor([]string{"--tool-lock-template", "--target", "linux/arm64", "--data", filepath.Join(dir, "data")}, &out, &diagnostics); code != 0 {
		t.Fatalf("template exit=%d diagnostics=%s", code, diagnostics.String())
	}
	var lock hosttools.InstallLock
	if err := json.Unmarshal(out.Bytes(), &lock); err != nil || lock.OS != "linux" || lock.Arch != "arm64" || len(lock.Pins) == 0 {
		t.Fatalf("invalid template: %#v, %v", lock, err)
	}
	for _, pin := range lock.Pins {
		if pin.Version != "" || pin.SHA256 != "" {
			t.Fatal("template invented reviewed versions or archive digests")
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("template bootstrapped deployment files: %#v, %v", entries, err)
	}
}

func TestDoctorRejectsUnexpectedCommandWithoutRunningIt(t *testing.T) {
	var out, diagnostics bytes.Buffer
	if code := runDoctor([]string{"install"}, &out, &diagnostics); code != 2 || out.Len() != 0 {
		t.Fatalf("unexpected command exit=%d output=%s", code, out.String())
	}
	if code := runDoctor([]string{"--tool-lock-template", "--target", "windows/amd64"}, &out, &diagnostics); code != 2 {
		t.Fatalf("unsupported template platform exit=%d", code)
	}
}
