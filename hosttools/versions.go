package hosttools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Installed struct {
	Available bool
	Version   string
}

// SecLists is data, not an executable. Keep legacy installations discoverable.
func SecListsPath() string {
	paths := []string{os.Getenv("GONDAR_SECLISTS_DIR"), "/opt/gondar-tools/share/seclists", "/opt/SecLists", "/usr/local/share/seclists", "/usr/share/seclists", "/usr/share/wordlists/seclists"}
	for _, dir := range paths {
		if dir == "" {
			continue
		}
		if a, err := os.Stat(filepath.Join(dir, "README.md")); err == nil && a.Mode().IsRegular() {
			if b, err := os.Stat(filepath.Join(dir, "Discovery")); err == nil && b.IsDir() {
				return dir
			}
		}
	}
	return ""
}

var packagesCache struct {
	sync.Mutex
	path   string
	until  time.Time
	values map[string]string
}

func InvalidatePackageCache() {
	packagesCache.Lock()
	packagesCache.until = time.Time{}
	packagesCache.Unlock()
}

func packageVersions() map[string]string {
	packagesCache.Lock()
	defer packagesCache.Unlock()
	if packagesCache.path == os.Getenv("PATH") && time.Now().Before(packagesCache.until) {
		return packagesCache.values
	}
	names := []string{"impacket", "netexec"}
	for _, t := range catalog {
		if t.Pip != "" && len(strings.Fields(t.Pip)) == 1 {
			names = append(names, t.Pip)
		}
	}
	raw, _ := json.Marshal(names)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-I", "-c", "import importlib.metadata as m,json,sys\nout={}\nfor n in json.loads(sys.argv[1]):\n try: out[n]=m.version(n)\n except m.PackageNotFoundError: pass\nprint(json.dumps(out))", string(raw))
	var output probeOutput
	cmd.Stdout = &output
	err := cmd.Run()
	data := output.Bytes()
	values := map[string]string{}
	if err == nil && len(data) <= 64<<10 {
		_ = json.Unmarshal(data, &values)
	}
	packagesCache.values, packagesCache.until = values, time.Now().Add(30*time.Second)
	packagesCache.path = os.Getenv("PATH")
	return values
}

var versionNumber = regexp.MustCompile(`(?i)\bv?(\d+\.\d+(?:\.\d+)?(?:[a-z0-9.+~_-]*))`)
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
var dataCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)

// Only fixed, non-network version arguments are used; never execute user text.
func ProbeInstalled(ctx context.Context, tool Tool) Installed {
	if tool.ID == "seclists" {
		dir := SecListsPath()
		if dir == "" {
			return Installed{}
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			probe, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			cmd := exec.CommandContext(probe, "git", "-C", dir, "rev-parse", "HEAD")
			var output probeOutput
			cmd.Stdout = &output
			if err := cmd.Run(); err == nil {
				if v := strings.TrimSpace(output.String()); dataCommit.MatchString(v) {
					return Installed{true, v}
				}
			}
		}
		if f, err := os.Open(filepath.Join(dir, ".gondar-version")); err == nil {
			defer f.Close()
			data, _ := io.ReadAll(io.LimitReader(f, 128))
			if v := strings.TrimSpace(string(data)); dataCommit.MatchString(v) {
				return Installed{true, v}
			}
		}
		return Installed{Available: true}
	}
	if tool.Kind == KindPip || tool.ID == "impacket" || tool.ID == "netexec" {
		name := tool.Pip
		if name == "" {
			name = tool.ID
		}
		if v := packageVersions()[name]; v != "" {
			if _, cli := tool.availableBin(); tool.PipLibrary || cli {
				return Installed{true, v}
			}
		}
		if tool.PipLibrary {
			return Installed{}
		}
	}
	bin, ok := tool.availableBin()
	if !ok {
		return Installed{}
	}
	current := Installed{Available: true}
	if version, ok := installedRecordVersion(tool, bin); ok {
		current.Version = version
		return current
	}
	// Enumerators must never be run merely to detect a version. An unknown
	// version is preferable to accidentally starting enumeration or a listener.
	if tool.ID == "linpeas" || tool.ID == "pspy64" || tool.ID == "responder" {
		return current
	}
	flags := map[string][]string{
		"nc": {"-h"}, "ssh": {"-V"}, "openssl": {"version"}, "dig": {"-v"}, "imagemagick": {"-version"}, "poppler": {"-v"},
		"nuclei": {"-version"}, "httpx": {"-version"}, "subfinder": {"-version"}, "naabu": {"-version"}, "dnsx": {"-version"}, "katana": {"-version"}, "tlsx": {"-version"},
		"ffuf": {"-V"}, "gobuster": {"version"}, "feroxbuster": {"--version"}, "chisel": {"--version"}, "dalfox": {"--version"}, "masscan": {"--version"}, "evil-winrm": {"-V"},
	}
	args := flags[tool.ID]
	if len(args) == 0 {
		args = []string{"--version"}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var output probeOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	_ = cmd.Run()
	data := output.Bytes()
	if m := versionNumber.FindStringSubmatch(ansiEscape.ReplaceAllString(string(data), "")); len(m) > 1 {
		current.Version = m[1]
	}
	return current
}

type probeOutput struct{ bytes.Buffer }

func (b *probeOutput) Write(data []byte) (int, error) {
	n := len(data)
	if remaining := (64 << 10) - b.Len(); remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = b.Buffer.Write(data)
	}
	return n, nil
}

func toolInvocation(t Tool) string {
	if t.PipLibrary {
		return "Python库 " + t.Pip
	}
	if t.ID == "seclists" {
		return "字典目录 " + SecListsPath()
	}
	return t.Bin
}
