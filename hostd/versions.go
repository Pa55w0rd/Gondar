package hostd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"gondar/hosttools"
)

const versionCacheTTL = 5 * time.Minute
const installPlanTTL = 10 * time.Minute

// ToolVersion is an observation, not an instruction supplied by a browser.
// Only a hostd-created resolution ID can select a discovered archive for a job.
type ToolVersion struct {
	ToolID           string         `json:"tool_id"`
	Kind             hosttools.Kind `json:"kind"`
	Platform         string         `json:"platform"`
	Available        bool           `json:"available"`
	UpdateAvailable  bool           `json:"update_available"`
	State            string         `json:"state"`
	InstalledVersion string         `json:"installed_version,omitempty"`
	LatestVersion    string         `json:"latest_version,omitempty"`
	LockedVersion    string         `json:"-"`
	LockedAvailable  bool           `json:"-"`
	LockedReason     string         `json:"-"`
	Source           string         `json:"source,omitempty"`
	SHA256           string         `json:"archive_sha256,omitempty"`
	ResolutionID     string         `json:"resolution_id,omitempty"`
	Installable      bool           `json:"installable"`
	Reason           string         `json:"reason,omitempty"`
	CheckedAt        time.Time      `json:"checked_at"`
	ExpiresAt        time.Time      `json:"expires_at,omitempty"`
	Cached           bool           `json:"cached"`
}

type installPlan struct {
	Pin         hosttools.InstallPin
	Platform    string
	Source      string
	Expires     time.Time
	AptPackages []string
}

type versionCacheEntry struct {
	info  ToolVersion
	until time.Time
	proxy string
}
type versionState struct {
	mu    sync.Mutex
	cache map[string]versionCacheEntry
	plans map[string]installPlan
	sem   chan struct{}
	apt   aptRefreshState
}

func newVersionState() *versionState {
	return &versionState{cache: map[string]versionCacheEntry{}, plans: map[string]installPlan{}, sem: make(chan struct{}, 4)}
}

var exactVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+!-]{0,127}$`)
var digestPattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

// Versions only probes local versions and official metadata; it never installs
// software or creates jobs. APT metadata refresh is shared across tools.
func (s *Server) Versions(ctx context.Context, id, proxy string) (ToolVersion, error) {
	tool, ok := hosttools.Lookup(id)
	if !ok {
		return ToolVersion{}, fmt.Errorf("未知工具，不在受管白名单内")
	}
	cleanProxy, err := ValidateProxy(proxy)
	if err != nil {
		return ToolVersion{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	state := s.versions
	state.mu.Lock()
	entry, hit := state.cache[id]
	if hit && entry.proxy == cleanProxy && time.Now().Before(entry.until) {
		entry.info.Cached = true
		state.mu.Unlock()
		return entry.info, nil
	}
	state.mu.Unlock()
	select {
	case state.sem <- struct{}{}:
		defer func() { <-state.sem }()
	case <-ctx.Done():
		return ToolVersion{}, ctx.Err()
	}
	// Requests queued behind an identical check reuse its result.
	state.mu.Lock()
	entry, hit = state.cache[id]
	state.mu.Unlock()
	if hit && entry.proxy == cleanProxy && time.Now().Before(entry.until) {
		entry.info.Cached = true
		return entry.info, nil
	}
	info := ToolVersion{ToolID: id, Kind: tool.Kind, Platform: runtime.GOOS + "/" + runtime.GOARCH, CheckedAt: time.Now().UTC()}
	current := hosttools.ProbeInstalled(ctx, tool)
	info.Available, info.InstalledVersion = current.Available, current.Version
	client := s.metadataClient(cleanProxy)
	defer client.CloseIdleConnections()
	var pin *hosttools.InstallPin
	var aptPackages []string
	switch tool.Kind {
	case hosttools.KindGitHub:
		pin, err = resolveGitHub(ctx, client, tool, &info)
	case hosttools.KindPip:
		pin, err = resolvePyPI(ctx, client, tool, &info)
	case hosttools.KindData:
		pin, err = s.resolveData(ctx, tool, &info, cleanProxy)
	case hosttools.KindSource:
		pin, err = s.resolveSource(ctx, client, tool, &info, cleanProxy)
	case hosttools.KindGem:
		pin, err = resolveGem(ctx, client, tool, &info)
	case hosttools.KindApt:
		info.Source = "已配置系统软件源"
		if s.pkgMgr == PkgApt {
			var installed, latest string
			err = s.ensureAPTMetadata(ctx, cleanProxy)
			if err == nil {
				installed, latest, err = aptVersions(ctx, tool)
			}
			if current.Version == "" || baseVersion(current.Version) == baseVersion(aptDisplayVersion(installed)) {
				info.InstalledVersion = aptDisplayVersion(installed)
			}
			info.LatestVersion = aptDisplayVersion(latest)
			if err == nil {
				aptPackages, err = aptPlanPackages(tool, latest)
				if err == nil {
					pin = &hosttools.InstallPin{ToolID: tool.ID, Kind: tool.Kind, Package: tool.Apt, Version: info.LatestVersion}
				}
			}
		} else {
			info.Reason = "当前平台暂不支持系统包的自动版本查询"
		}
	case hosttools.KindManual:
		info.Reason = "此工具需按官方说明手动安装"
	}
	if err != nil {
		if ctx.Err() != nil {
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ToolVersion{}, ctx.Err()
			}
			// Slow remote metadata must not erase an already observed local
			// installation, or leave a partial latest version/action behind.
			info.LatestVersion, info.SHA256 = "", ""
			pin = nil
			info.Reason = "官方版本查询超时；已保留本地可用状态和当前版本，请稍后重新检测"
		} else {
			info.Reason = err.Error()
		}
	}
	finishVersionStatus(&info)
	state.mu.Lock()
	defer state.mu.Unlock()
	now := time.Now()
	for key, plan := range state.plans {
		if !now.Before(plan.Expires) {
			delete(state.plans, key)
		}
	}
	if pin != nil && err == nil && (!info.Available || info.UpdateAvailable) && runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") {
		var random [24]byte
		if _, err := rand.Read(random[:]); err != nil {
			return ToolVersion{}, fmt.Errorf("无法生成安装计划")
		}
		info.ResolutionID = hex.EncodeToString(random[:])
		info.ExpiresAt = now.Add(installPlanTTL).UTC()
		info.Installable = true
		state.plans[info.ResolutionID] = installPlan{Pin: *pin, Platform: info.Platform, Source: info.Source, Expires: info.ExpiresAt, AptPackages: aptPackages}
	}
	// The catalog bounds this map. Short-lived plans are also bounded when a
	// client repeatedly consumes/rechecks instead of waiting for cache expiry.
	if len(state.plans) > 256 {
		for key := range state.plans {
			if key != info.ResolutionID {
				delete(state.plans, key)
				break
			}
		}
	}
	cacheTTL := versionCacheTTL
	if err != nil {
		cacheTTL = 15 * time.Second
	}
	state.cache[id] = versionCacheEntry{info: info, until: now.Add(cacheTTL), proxy: cleanProxy}
	return info, nil
}

func (s *Server) metadataClient(proxy string) *http.Client {
	if s.versionHTTP != nil {
		return s.versionHTTP
	}
	client := httpClient(proxy)
	client.Timeout = 12 * time.Second
	client.CheckRedirect = officialRedirect
	return client
}

func (s *Server) archiveClient(proxy string) *http.Client {
	if s.versionHTTP != nil {
		return s.versionHTTP
	}
	client := httpClient(proxy)
	client.CheckRedirect = officialRedirect
	return client
}

func officialRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.User != nil || (req.URL.Port() != "" && req.URL.Port() != "443") {
		return fmt.Errorf("拒绝不安全的官方源重定向")
	}
	switch req.URL.Hostname() {
	case "github.com", "api.github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com", "pypi.org", "files.pythonhosted.org", "rubygems.org":
		return nil
	}
	return fmt.Errorf("重定向离开固定官方源")
}

func metadata(ctx context.Context, client *http.Client, address string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Gondar-managed-tools")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("官方元数据读取失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &metadataStatusError{Status: resp.StatusCode, RateLimited: resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.StatusCode == 429}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("官方元数据读取中断")
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("官方元数据超过大小限制")
	}
	return data, nil
}

func resolveGitHub(ctx context.Context, client *http.Client, tool hosttools.Tool, info *ToolVersion) (*hosttools.InstallPin, error) {
	info.Source = "https://github.com/" + tool.Repo + "/releases"
	raw, err := metadata(ctx, client, GitHubAPITagURL(tool), 2<<20)
	if err != nil {
		return githubReleaseFallback(ctx, client, tool, info, err)
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"assets"`
	}
	if json.Unmarshal(raw, &release) != nil || !exactVersion.MatchString(release.Tag) || strings.Contains(release.Tag, "..") || strings.EqualFold(release.Tag, "latest") || release.Draft || release.Prerelease {
		return nil, fmt.Errorf("官方响应不是有效的稳定Release")
	}
	info.LatestVersion = release.Tag
	asset := path.Base(GitHubAssetURL(tool, release.Tag))
	for _, a := range release.Assets {
		if a.Name != asset {
			continue
		}
		if a.Size <= 0 || a.Size > 128<<20 {
			return nil, fmt.Errorf("当前平台官方归档超过大小限制或为空")
		}
		var digest string
		if strings.HasPrefix(a.Digest, "sha256:") && digestPattern.MatchString(strings.TrimPrefix(a.Digest, "sha256:")) {
			digest = strings.TrimPrefix(a.Digest, "sha256:")
		}
		if digest == "" {
			// Only a checksum asset from the same fixed repository AND exact tag
			// may supply the digest. Never accept user URLs or a 'latest' download.
			for _, sums := range release.Assets {
				if !checksumAsset(sums.Name) || sums.Size <= 0 || sums.Size > 1<<20 {
					continue
				}
				address := "https://github.com/" + tool.Repo + "/releases/download/" + url.PathEscape(release.Tag) + "/" + url.PathEscape(sums.Name)
				data, fetchErr := metadata(ctx, client, address, 1<<20)
				if fetchErr != nil {
					continue
				}
				if found, ok := checksumFor(data, asset); ok {
					digest = found
					break
				}
			}
		}
		if !digestPattern.MatchString(digest) {
			return nil, fmt.Errorf("检测到稳定版本，但该平台归档没有可核验的官方SHA256，请稍后重试")
		}
		info.SHA256 = strings.ToLower(digest)
		return &hosttools.InstallPin{ToolID: tool.ID, Kind: tool.Kind, Repository: tool.Repo, Version: release.Tag, SHA256: info.SHA256}, nil
	}
	return nil, fmt.Errorf("最新稳定Release未提供当前平台的受管资产：%s", asset)
}

func checksumAsset(name string) bool {
	name = strings.ToLower(name)
	return name == "sha256sums" || name == "sha256sums.txt" || name == "checksums.txt" || strings.HasSuffix(name, "_checksums.txt") || strings.HasSuffix(name, "-checksums.txt")
}

func checksumFor(data []byte, asset string) (string, bool) {
	var result string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !digestPattern.MatchString(fields[0]) || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		if result != "" && !strings.EqualFold(result, fields[0]) {
			return "", false
		}
		result = strings.ToLower(fields[0])
	}
	return result, result != ""
}

func resolvePyPI(ctx context.Context, client *http.Client, tool hosttools.Tool, info *ToolVersion) (*hosttools.InstallPin, error) {
	if len(strings.Fields(tool.Pip)) != 1 {
		info.Reason = "多包配方暂不自动选择统一版本"
		return nil, nil
	}
	info.Source = "https://pypi.org/project/" + tool.Pip + "/"
	version, err := pypiLatest(ctx, client, tool.Pip)
	if err != nil {
		return nil, err
	}
	info.LatestVersion = version
	return &hosttools.InstallPin{ToolID: tool.ID, Kind: tool.Kind, Package: tool.Pip, Version: version}, nil
}

// Popular PyPI projects have many megabytes of historical release files. Read
// only the bounded info object instead of downloading their entire history.
func pypiLatest(ctx context.Context, client *http.Client, name string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://pypi.org/pypi/"+url.PathEscape(name)+"/json", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Gondar-managed-tools")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("PyPI版本查询失败，请检查安装代理后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("PyPI版本查询返回HTTP %d，请稍后重试或检查安装代理", resp.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 2<<20))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return "", fmt.Errorf("PyPI未返回有效版本信息")
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			break
		}
		if key == "info" {
			var info struct {
				Version string `json:"version"`
			}
			if decoder.Decode(&info) != nil || !exactVersion.MatchString(info.Version) {
				break
			}
			return info.Version, nil
		}
		var unused json.RawMessage
		if decoder.Decode(&unused) != nil {
			break
		}
	}
	return "", fmt.Errorf("PyPI未返回有效版本，或版本信息超过读取限制")
}

func aptVersions(ctx context.Context, tool hosttools.Tool) (string, string, error) {
	packages := strings.Fields(tool.Apt)
	if len(packages) == 0 {
		return "", "", fmt.Errorf("没有适用的APT包名")
	}
	var installed, candidates []string
	for _, pkg := range packages {
		cmd := exec.CommandContext(ctx, "apt-cache", "policy", pkg)
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		var output limitedBuffer
		cmd.Stdout, cmd.Stderr = &output, io.Discard
		if err := cmd.Run(); err != nil {
			return "", "", fmt.Errorf("软件源版本读取失败")
		}
		var current, candidate string
		for _, line := range strings.Split(output.String(), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Installed:") {
				current = strings.TrimSpace(strings.TrimPrefix(line, "Installed:"))
			}
			if strings.HasPrefix(line, "Candidate:") {
				candidate = strings.TrimSpace(strings.TrimPrefix(line, "Candidate:"))
			}
		}
		if current != "" && current != "(none)" {
			installed = append(installed, pkg+"="+current)
		}
		if candidate != "" && candidate != "(none)" {
			candidates = append(candidates, pkg+"="+candidate)
		}
	}
	if len(candidates) != len(packages) {
		return strings.Join(installed, ", "), "", fmt.Errorf("暂时无法获取软件源最新版本，请重新检测")
	}
	return strings.Join(installed, ", "), strings.Join(candidates, ", "), nil
}

func aptPlanPackages(tool hosttools.Tool, candidates string) ([]string, error) {
	packages := strings.Fields(tool.Apt)
	parts := strings.Split(candidates, ", ")
	if len(parts) != len(packages) {
		return nil, fmt.Errorf("软件源版本未覆盖全部配方包")
	}
	version := regexp.MustCompile(`^[0-9][A-Za-z0-9.+:~_-]{0,200}$`)
	for i, pkg := range packages {
		name, value, ok := strings.Cut(parts[i], "=")
		if !ok || name != pkg || !version.MatchString(value) {
			return nil, fmt.Errorf("软件源版本与固定包配方不一致")
		}
	}
	return parts, nil
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 64<<10 {
		remain := (64 << 10) - b.Len()
		if len(p) > remain {
			p = p[:remain]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

func (s *Server) takeResolution(id, toolID string) (installPlan, error) {
	s.versions.mu.Lock()
	defer s.versions.mu.Unlock()
	plan, ok := s.versions.plans[id]
	if !ok || plan.Pin.ToolID != toolID || plan.Platform != runtime.GOOS+"/"+runtime.GOARCH || !time.Now().Before(plan.Expires) {
		return installPlan{}, fmt.Errorf("安装计划不存在、已过期或不适用于当前工具/平台；请重新检测")
	}
	delete(s.versions.plans, id)
	delete(s.versions.cache, toolID)
	return plan, nil
}

func (s *Server) receiptDir() string {
	if s.StateDir != "" {
		return s.StateDir
	}
	return "/var/lib/gondar-hostd"
}
func (s *Server) readReceipt(id string) (hosttools.InstallPin, error) {
	var pin hosttools.InstallPin
	raw, err := os.ReadFile(filepath.Join(s.receiptDir(), id+".json"))
	if err != nil {
		return pin, err
	}
	err = json.Unmarshal(raw, &pin)
	if err == nil && pin.ToolID != id {
		err = fmt.Errorf("安装回执不匹配")
	}
	return pin, err
}
func (s *Server) saveReceipt(j *Job) error {
	if j.installPin == nil {
		return nil
	}
	if err := os.MkdirAll(s.receiptDir(), 0o700); err != nil {
		return err
	}
	raw, _ := json.Marshal(j.installPin)
	filename := filepath.Join(s.receiptDir(), j.ToolID+".json")
	file, err := os.CreateTemp(s.receiptDir(), ".receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filename)
}
