package hosttools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"regexp"
)

// InstalledRecord binds a version observation to the actual installed file.
// This is separate from the publisher's download digest and never replaces it.
type InstalledRecord struct {
	ToolID  string `json:"tool_id"`
	Version string `json:"version"`
	SHA256  string `json:"installed_sha256"`
}

var installedVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+!-]{0,127}$`)

func installedRecordVersion(tool Tool, bin string) (string, bool) {
	name, err := exec.LookPath(bin)
	if err != nil {
		return "", false
	}
	f, err := os.Open(name + ".gondar-version.json")
	if err != nil {
		return "", false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	var record InstalledRecord
	if err != nil || len(raw) > 4096 || json.Unmarshal(raw, &record) != nil || record.ToolID != tool.ID || !installedVersion.MatchString(record.Version) {
		return "", false
	}
	file, err := os.Open(name)
	if err != nil {
		return "", false
	}
	defer file.Close()
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(file, (256<<20)+1)); err != nil {
		return "", false
	}
	return record.Version, hex.EncodeToString(h.Sum(nil)) == record.SHA256
}
