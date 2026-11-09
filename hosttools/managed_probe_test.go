package hosttools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestManualToolIDsAndOfficialProbeAliases(t *testing.T) {
	for _, tc := range []struct {
		id      string
		aliases []string
	}{
		{"impacket", []string{"impacket-smbexec", "smbexec.py"}},
		{"netexec", []string{"nxc", "netexec", "NetExec"}},
	} {
		tool, ok := Lookup(tc.id)
		if !ok || tool.ID != tc.id || tool.Kind != KindManual || tool.Installable {
			t.Fatalf("manual tool identity drift: %+v", tool)
		}
		for _, alias := range tc.aliases {
			t.Run(tc.id+"/"+alias, func(t *testing.T) {
				dir := t.TempDir()
				name := alias
				if runtime.GOOS == "windows" {
					name += ".exe"
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte("synthetic; never executed"), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", dir)
				if !tool.AvailableInPath() {
					t.Fatal("supported command was not detected")
				}
				found := false
				for _, observed := range Inventory() {
					if observed.ID == tc.id {
						found = true
						matches := observed.Bin == alias || (runtime.GOOS == "windows" && strings.EqualFold(observed.Bin, alias))
						if !observed.Available || !matches {
							t.Fatalf("inventory did not expose actual command: %+v", observed)
						}
					}
				}
				if !found {
					t.Fatal("stable tool ID disappeared")
				}
			})
		}
	}
}

func TestLibraryOnlyImpacketDoesNotClaimCLICapability(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	tool, _ := Lookup("impacket")
	if tool.AvailableInPath() {
		t.Fatal("a missing example command was declared available")
	}
}

func TestRetiredNetExecPinDoesNotDisableOtherApprovedTools(t *testing.T) {
	lock := InstallLock{Format: 1, OS: "linux", Arch: "amd64", Pins: []InstallPin{
		{ToolID: "netexec", Kind: KindPip, Package: "netexec", Version: "1.0.0"},
		{ToolID: "nuclei", Kind: KindGitHub, Repository: "projectdiscovery/nuclei", Version: "v1.2.3", SHA256: strings.Repeat("a", 64)},
	}}
	raw, _ := json.Marshal(lock)
	parsed, err := ParseInstallLock(raw)
	if err != nil {
		t.Fatal(err)
	}
	nuclei, _ := Lookup("nuclei")
	if _, err := parsed.Pin(nuclei, "linux", "amd64"); err != nil {
		t.Fatal(err)
	}
	netexec, _ := Lookup("netexec")
	if _, err := parsed.Pin(netexec, "linux", "amd64"); err == nil {
		t.Fatal("retired recipe was enabled")
	}
	lock.Pins[0].Repository = "attacker/evil"
	raw, _ = json.Marshal(lock)
	if _, err := ParseInstallLock(raw); err == nil {
		t.Fatal("legacy compatibility accepted a changed source")
	}
}
