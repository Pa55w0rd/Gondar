package hosttools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecListsVersionBelongsToDictionary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	parent := t.TempDir()
	for _, args := range [][]string{{"init", "-q", parent}, {"-C", parent, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "parent"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture failed: %v %s", err, out)
		}
	}
	dir := filepath.Join(parent, "dictionary")
	if err := os.MkdirAll(filepath.Join(dir, "Discovery"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GONDAR_SECLISTS_DIR", dir)
	tool, _ := Lookup("seclists")
	current := ProbeInstalled(t.Context(), tool)
	if !current.Available || current.Version != "" {
		t.Fatalf("parent repository must not be dictionary version: %+v", current)
	}
	for _, marker := range []string{"invalid", strings.Repeat("b", 40) + "\n"} {
		if err := os.WriteFile(filepath.Join(dir, ".gondar-version"), []byte(marker), 0o644); err != nil {
			t.Fatal(err)
		}
		want := ""
		if strings.HasPrefix(marker, "b") {
			want = strings.TrimSpace(marker)
		}
		if current := ProbeInstalled(t.Context(), tool); !current.Available || current.Version != want {
			t.Fatalf("incorrect marker detection: %+v", current)
		}
	}
}
