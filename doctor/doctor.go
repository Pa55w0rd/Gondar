// Package doctor inspects deployment state without opening Gondar's stores,
// migrating schemas, starting workers or executing downloaded content.
package doctor

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gondar/hosttools"
	"gondar/selfupdate"
)

type Check struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type Tool struct {
	ID            string `json:"id"`
	Path          string `json:"path,omitempty"`
	Available     bool   `json:"available"`
	Version       string `json:"version,omitempty"`
	VersionSource string `json:"version_source"`
	BinarySHA256  string `json:"observed_binary_sha256,omitempty"`
	Note          string `json:"note,omitempty"`
}

type Dependency struct {
	Path               string `json:"path"`
	Version            string `json:"version"`
	ReplacementPath    string `json:"replacement_path,omitempty"`
	ReplacementVersion string `json:"replacement_version,omitempty"`
}

type Disk struct {
	Check
	Path      string `json:"path"`
	FreeBytes uint64 `json:"free_bytes,omitempty"`
}

type Report struct {
	Product      string                        `json:"product"`
	Version      string                        `json:"version"`
	GoVersion    string                        `json:"go_version"`
	OS           string                        `json:"os"`
	Arch         string                        `json:"arch"`
	ReadOnly     bool                          `json:"read_only"`
	Config       Check                         `json:"config"`
	Postgres     Check                         `json:"postgres"`
	Disk         Disk                          `json:"disk"`
	Tools        []Tool                        `json:"tools"`
	Dependencies []Dependency                  `json:"compiled_dependencies"`
	UpdatePolicy selfupdate.VerificationPolicy `json:"update_policy"`
	ReleaseCheck Check                         `json:"release_verification"`
	ToolLock     Check                         `json:"tool_install_lock"`
}

type Options struct {
	Version          string
	ConfigPath       string
	DataDir          string
	DSN              string
	HashTools        bool
	ProbeVersions    bool
	Tools            []hosttools.Tool
	ToolLockPath     string
	ReleaseManifest  string
	ReleaseSignature string
	ReleaseArchive   string
}

// Run never includes DSNs, environment values, configuration contents or raw
// database errors in its report. Database access is restricted to SELECT in a
// session explicitly configured read-only, without importing the db package.
func Run(ctx context.Context, opts Options) Report {
	r := Report{Product: "Gondar", Version: opts.Version, GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, ReadOnly: !opts.ProbeVersions, Tools: []Tool{}, Dependencies: []Dependency{}, UpdatePolicy: selfupdate.ReleaseVerificationPolicy()}
	r.Config = inspectConfig(opts.ConfigPath)
	r.Postgres = inspectPostgres(ctx, opts.DSN)
	r.Disk = inspectDisk(opts.DataDir)
	r.ReleaseCheck = verifyReleaseFiles(opts)
	r.ToolLock = inspectToolLock(opts.ToolLockPath)
	tools := opts.Tools
	if tools == nil {
		tools = hosttools.Catalog()
		for _, bin := range []string{"go", "node", "python3", "curl", "git", "rg", "psql", "docker"} {
			found := false
			for _, tool := range tools {
				found = found || tool.Bin == bin
			}
			if !found {
				tools = append(tools, hosttools.Tool{ID: bin, Name: bin, Bin: bin})
			}
		}
	}
	for _, tool := range tools {
		if ctx.Err() != nil {
			break
		}
		r.Tools = append(r.Tools, inspectTool(ctx, tool, opts.HashTools, opts.ProbeVersions))
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, module := range info.Deps {
			dependency := Dependency{Path: module.Path, Version: module.Version}
			if module.Replace != nil {
				dependency.ReplacementPath = module.Replace.Path
				dependency.ReplacementVersion = module.Replace.Version
			}
			r.Dependencies = append(r.Dependencies, dependency)
		}
		sort.Slice(r.Dependencies, func(i, j int) bool { return r.Dependencies[i].Path < r.Dependencies[j].Path })
	}
	return r
}

func (r Report) ExitCode() int {
	for _, check := range []Check{r.Config, r.Postgres, r.Disk.Check, r.ReleaseCheck, r.ToolLock} {
		if check.Status == "error" {
			return 1
		}
	}
	return 0
}

func inspectConfig(filename string) Check {
	raw, err := readBoundedFile(filename, 1<<20)
	if os.IsNotExist(err) {
		return Check{"warning", "未找到配置文件；可使用 GONDAR_PG_DSN"}
	}
	if err != nil {
		return Check{"error", "配置文件不可读、不是普通文件或超过 1 MiB"}
	}
	if !json.Valid(raw) || len(strings.TrimSpace(string(raw))) == 0 || strings.TrimSpace(string(raw))[0] != '{' {
		return Check{"error", "配置文件不是有效的 JSON 对象或超过大小限制"}
	}
	return Check{"ok", "配置文件可读且 JSON 格式有效；未输出配置值"}
}

func inspectPostgres(ctx context.Context, dsn string) Check {
	if strings.TrimSpace(dsn) == "" {
		return Check{"error", "未配置 PostgreSQL 连接"}
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return Check{"error", "PostgreSQL 连接格式无效；已隐藏连接内容"}
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = make(map[string]string)
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["application_name"] = "gondar-doctor"
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return Check{"error", "PostgreSQL 连接失败；请检查地址、认证和网络，未输出原始连接错误"}
	}
	defer conn.Close(context.Background())
	var version, readOnly string
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num'), current_setting('default_transaction_read_only')").Scan(&version, &readOnly); err != nil || readOnly != "on" {
		return Check{"error", "PostgreSQL 只读查询失败"}
	}
	for _, ch := range version {
		if ch < '0' || ch > '9' {
			return Check{"error", "PostgreSQL 返回异常版本信息"}
		}
	}
	return Check{"ok", "PostgreSQL 可达，服务版本号 " + version + "；仅执行只读 SELECT，未修改业务 schema"}
}

func inspectDisk(filename string) Disk {
	if filename == "" {
		filename = "."
	}
	abs, err := filepath.Abs(filename)
	if err != nil {
		return Disk{Check: Check{"error", "数据路径无法解析"}}
	}
	r := Disk{Path: abs}
	probe := abs
	for {
		st, err := os.Stat(probe)
		if err == nil {
			if !st.IsDir() {
				r.Check = Check{"error", "数据路径不是目录"}
				return r
			}
			break
		}
		if !os.IsNotExist(err) {
			r.Check = Check{"error", "数据路径不可读取；未执行写入测试"}
			return r
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			r.Check = Check{"error", "数据路径及父目录不可读"}
			return r
		}
		probe = parent
	}
	free, err := freeDisk(probe)
	if err != nil {
		r.Check = Check{"warning", "无法读取空闲磁盘容量；未执行写入测试"}
		return r
	}
	r.FreeBytes = free
	switch {
	case free < 512<<20:
		r.Check = Check{"warning", "空闲空间少于 512 MiB；未执行写入测试"}
	case probe != abs:
		r.Check = Check{"warning", "数据目录尚不存在；容量取自现有父目录，未创建目录"}
	default:
		r.Check = Check{"ok", "已读取磁盘容量；未创建目录或执行写入测试"}
	}
	return r
}

func inspectTool(ctx context.Context, tool hosttools.Tool, hash, probe bool) Tool {
	r := Tool{ID: tool.ID, VersionSource: "未执行本地工具"}
	filename := ""
	for _, bin := range append([]string{tool.Bin}, tool.Aliases...) {
		if found, err := exec.LookPath(bin); err == nil {
			filename = found
			break
		}
	}
	if filename == "" {
		return r
	}
	r.Available, r.Path = true, filename
	if info, err := buildinfo.ReadFile(filename); err == nil {
		r.Version = info.Main.Version
		if r.Version == "" || r.Version == "(devel)" {
			r.Version = info.GoVersion
		}
		r.VersionSource = "静态 Go 构建信息；未执行工具"
	}
	if probe {
		if output, ok := probeVersion(ctx, tool.Bin, filename); ok {
			r.Version, r.VersionSource = output, "固定本地版本参数；没有下载或安装"
		}
	}
	if hash {
		f, err := os.Open(filename)
		if err != nil {
			r.Note = "无法读取工具文件摘要"
			return r
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() || st.Size() > 256<<20 {
			r.Note = "工具不是普通文件或超过 256 MiB，未读取摘要"
			return r
		}
		h := sha256.New()
		if _, err := io.Copy(h, io.LimitReader(f, (256<<20)+1)); err == nil {
			r.BinarySHA256 = hex.EncodeToString(h.Sum(nil))
		}
	}
	return r
}

func inspectToolLock(filename string) Check {
	if filename == "" {
		return Check{"warning", "未指定工具安装清单；GitHub/PyPI 自动安装需要管理员审核的版本锁"}
	}
	raw, err := readBoundedFile(filename, 1<<20)
	if err != nil {
		return Check{"warning", "工具安装清单不可读；不输出文件内容"}
	}
	lock, err := hosttools.ParseInstallLock(raw)
	if err != nil {
		return Check{"error", "工具安装清单格式或来源不符合白名单"}
	}
	ready := 0
	for _, tool := range hosttools.Catalog() {
		if _, err := lock.Pin(tool, lock.OS, lock.Arch); err == nil {
			ready++
		}
	}
	status := "ok"
	if ready == 0 {
		status = "warning"
	}
	return Check{status, fmt.Sprintf("清单有 %d 个完整版本条目；助手仍会核对文件所有权与平台。PyPI 仅锁定顶层版本，传递依赖未因此完成哈希锁定", ready)}
}

func readBoundedFile(filename string, limit int64) ([]byte, error) {
	st, err := os.Stat(filename)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, fmt.Errorf("需要大小受限的普通文件")
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, fmt.Errorf("文件不可读或超过限制")
	}
	return raw, nil
}

func verifyReleaseFiles(opts Options) Check {
	if opts.ReleaseManifest == "" && opts.ReleaseSignature == "" && opts.ReleaseArchive == "" {
		return Check{"unknown", "未提供离线发行包；尚未执行签名或归档摘要验证"}
	}
	if opts.ReleaseManifest == "" || opts.ReleaseSignature == "" || opts.ReleaseArchive == "" {
		return Check{"error", "离线验证需要同时提供清单、签名与归档路径"}
	}
	manifest, err := readBoundedFile(opts.ReleaseManifest, 1<<20)
	if err != nil {
		return Check{"error", "发行清单读取失败"}
	}
	signature, err := readBoundedFile(opts.ReleaseSignature, 512)
	if err != nil {
		return Check{"error", "发行签名读取失败"}
	}
	if err := selfupdate.VerifyReleaseManifest(manifest, signature); err != nil {
		return Check{"error", "发行签名未通过可信公钥验证"}
	}
	f, err := os.Open(opts.ReleaseArchive)
	if err != nil {
		return Check{"error", "发行归档读取失败"}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 512<<20 {
		return Check{"error", "发行归档不是普通文件或超过 512 MiB"}
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, (512<<20)+1)); err != nil {
		return Check{"error", "发行归档摘要读取失败"}
	}
	want := hex.EncodeToString(h.Sum(nil))
	name := filepath.Base(opts.ReleaseArchive)
	for _, line := range strings.Split(string(manifest), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name && strings.EqualFold(fields[0], want) {
			return Check{"ok", "可信签名及归档 SHA256 均通过；未解压或执行归档"}
		}
	}
	return Check{"error", "归档摘要或名称与已签名清单不一致"}
}
