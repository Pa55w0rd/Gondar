package hosttools

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const InstallLockFormat = 1

type InstallPin struct {
	ToolID     string `json:"tool_id"`
	Kind       Kind   `json:"kind"`
	Repository string `json:"repository,omitempty"`
	Package    string `json:"package,omitempty"`
	Version    string `json:"version"`
	SHA256     string `json:"archive_sha256,omitempty"`
	Commit     string `json:"source_commit,omitempty"`
}

type InstallLock struct {
	Format int          `json:"format"`
	OS     string       `json:"os"`
	Arch   string       `json:"arch"`
	Pins   []InstallPin `json:"pins"`
}

// InstallLockTemplate is deliberately incomplete. It never invents versions or
// archive digests from an installed binary's unrelated SHA256 observation.
func InstallLockTemplate(goos, arch string) InstallLock {
	lock := InstallLock{Format: InstallLockFormat, OS: goos, Arch: arch, Pins: []InstallPin{}}
	for _, tool := range Catalog() {
		if tool.Kind == KindGitHub || tool.Kind == KindPip {
			lock.Pins = append(lock.Pins, InstallPin{ToolID: tool.ID, Kind: tool.Kind, Repository: tool.Repo, Package: tool.Pip})
		}
	}
	return lock
}

func ParseInstallLock(raw []byte) (*InstallLock, error) {
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("工具安装清单超过大小限制")
	}
	var lock InstallLock
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		return nil, fmt.Errorf("工具安装清单格式错误")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("工具安装清单含尾随数据")
	}
	if lock.Format != InstallLockFormat || lock.OS != "linux" || (lock.Arch != "amd64" && lock.Arch != "arm64") {
		return nil, fmt.Errorf("工具安装清单需要 format=1 和 linux/amd64 或 linux/arm64")
	}
	seen := make(map[string]bool)
	for _, pin := range lock.Pins {
		if seen[pin.ToolID] {
			return nil, fmt.Errorf("工具安装清单含重复工具")
		}
		seen[pin.ToolID] = true
		tool, ok := Lookup(pin.ToolID)
		// Keep other reviewed pins usable when an older administrator manifest
		// contains the retired NetExec PyPI recipe. Pin rejects this inert entry
		// against the current manual catalog; changed sources remain invalid.
		if ok && tool.ID == "netexec" && tool.Kind == KindManual && pin.Kind == KindPip && pin.Package == "netexec" && pin.Repository == "" {
			continue
		}
		if !ok || pin.Kind != tool.Kind || (pin.Kind != KindGitHub && pin.Kind != KindPip) || pin.Repository != tool.Repo || pin.Package != tool.Pip {
			return nil, fmt.Errorf("工具安装清单来源与内置白名单不一致")
		}
	}
	return &lock, nil
}

var pinnedVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+!-]*$`)

func (lock *InstallLock) Pin(tool Tool, goos, arch string) (InstallPin, error) {
	if lock == nil || lock.OS != goos || lock.Arch != arch {
		return InstallPin{}, fmt.Errorf("工具安装清单不适用于当前平台")
	}
	for _, pin := range lock.Pins {
		if pin.ToolID != tool.ID {
			continue
		}
		if pin.Kind != tool.Kind || pin.Repository != tool.Repo || pin.Package != tool.Pip || !pinnedVersionPattern.MatchString(pin.Version) || strings.Contains(pin.Version, "..") || strings.EqualFold(pin.Version, "latest") {
			return InstallPin{}, fmt.Errorf("工具尚未锁定可信的确切版本")
		}
		if pin.Kind == KindGitHub {
			digest, err := hex.DecodeString(pin.SHA256)
			if err != nil || len(digest) != 32 {
				return InstallPin{}, fmt.Errorf("GitHub 工具需要已审核的归档 SHA256，不能使用已安装文件的摘要替代")
			}
		}
		if pin.Kind == KindPip && len(strings.Fields(tool.Pip)) != 1 {
			return InstallPin{}, fmt.Errorf("多包配方需要逐包审核，当前版本锁不接受共享版本")
		}
		return pin, nil
	}
	return InstallPin{}, fmt.Errorf("工具没有锁定安装条目")
}
