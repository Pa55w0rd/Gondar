package hosttools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBundledToolManifestsAreExactForEachArchitecture(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		raw, err := os.ReadFile(filepath.Join("..", "deployment", "tool-lock", "linux-"+arch+".json"))
		if err != nil {
			t.Fatal(err)
		}
		lock, err := ParseInstallLock(raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"nuclei", "httpx", "subfinder"} {
			tool, ok := Lookup(id)
			if !ok {
				t.Fatalf("bundled tool %s missing from catalog", id)
			}
			if _, err := lock.Pin(tool, "linux", arch); err != nil {
				t.Fatalf("%s/%s: %v", id, arch, err)
			}
		}
		unreviewed, _ := Lookup("semgrep")
		if _, err := lock.Pin(unreviewed, "linux", arch); err == nil {
			t.Fatal("bundled manifest silently enabled an unreviewed package")
		}
	}
}
