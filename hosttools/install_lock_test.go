package hosttools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInstallLockRequiresReviewedVersionAndArchiveDigest(t *testing.T) {
	lock := InstallLockTemplate("linux", "amd64")
	tool, _ := Lookup("nuclei")
	if _, err := lock.Pin(tool, "linux", "amd64"); err == nil {
		t.Fatal("template must not claim to be a complete trusted lock")
	}
	for i := range lock.Pins {
		if lock.Pins[i].ToolID == tool.ID {
			lock.Pins[i].Version = "v3.4.10"
			lock.Pins[i].SHA256 = strings.Repeat("a", 64)
		}
	}
	raw, _ := json.Marshal(lock)
	parsed, err := ParseInstallLock(raw)
	if err != nil {
		t.Fatal(err)
	}
	if pin, err := parsed.Pin(tool, "linux", "amd64"); err != nil || pin.Version != "v3.4.10" {
		t.Fatalf("pin=%#v, error=%v", pin, err)
	}
	if _, err := parsed.Pin(tool, "linux", "arm64"); err == nil {
		t.Fatal("accepted a lock for another architecture")
	}
	for _, version := range []string{"latest", "../escape", "v1;id", "v1\nother", "--upgrade"} {
		for i := range parsed.Pins {
			if parsed.Pins[i].ToolID == tool.ID {
				parsed.Pins[i].Version = version
			}
		}
		if _, err := parsed.Pin(tool, "linux", "amd64"); err == nil {
			t.Fatalf("accepted version %q", version)
		}
	}
}

func TestInstallLockRejectsUnknownSourceAndDuplicates(t *testing.T) {
	tool, _ := Lookup("nuclei")
	for _, pins := range [][]InstallPin{
		{{ToolID: tool.ID, Kind: tool.Kind, Repository: "untrusted/tool"}},
		{{ToolID: tool.ID, Kind: tool.Kind, Repository: tool.Repo}, {ToolID: tool.ID, Kind: tool.Kind, Repository: tool.Repo}},
		{{ToolID: "unlisted-command", Kind: KindGitHub}},
	} {
		raw, _ := json.Marshal(InstallLock{Format: 1, OS: "linux", Arch: "amd64", Pins: pins})
		if _, err := ParseInstallLock(raw); err == nil {
			t.Fatal("accepted an untrusted source or duplicate entry")
		}
	}
}
