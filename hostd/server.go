package hostd

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gondar/hosttools"
	"gondar/proxyprobe"
)

// State 任务状态。
type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateDone      State = "done"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

const (
	maxLogBytes = 256 << 10 // 单个任务保留的最大日志(超出截尾)
	binDir      = "/usr/local/bin"
)

// Job 一次安装任务。
type Job struct {
	ID          string    `json:"id"`
	ToolID      string    `json:"tool_id"`
	ToolName    string    `json:"tool_name"`
	State       State     `json:"state"`
	ExitCode    int       `json:"exit_code"`
	Err         string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at,omitempty"`
	Log         string    `json:"log"`
	Version     string    `json:"version,omitempty"`
	Source      string    `json:"source,omitempty"`
	installPin  *hosttools.InstallPin
	binDir      string
	redactions  []string
	aptPackages []string

	mu     sync.Mutex
	buf    bytes.Buffer
	cancel context.CancelFunc
}

func (j *Job) append(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i := 0; i+1 < len(j.redactions); i += 2 {
		line = strings.ReplaceAll(line, j.redactions[i], j.redactions[i+1])
	}
	j.buf.WriteString(line)
	if j.buf.Len() > maxLogBytes {
		// 掐掉前半段,保留尾部(安装失败原因通常在最后)
		b := j.buf.Bytes()
		keep := b[len(b)-maxLogBytes:]
		j.buf.Reset()
		j.buf.WriteString("…(前段日志已截断)…\n")
		j.buf.Write(keep)
	}
	j.Log = j.buf.String()
}

func (j *Job) setState(s State) {
	j.mu.Lock()
	j.State = s
	j.mu.Unlock()
}

func (j *Job) setErr(err error) {
	if err == nil {
		return
	}
	j.mu.Lock()
	j.Err = err.Error()
	j.mu.Unlock()
}

// Polling can race with an installer appending output. Serialize a locked
// snapshot instead of letting encoding/json read the live exported fields.
func (j *Job) MarshalJSON() ([]byte, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return json.Marshal(struct {
		ID        string    `json:"id"`
		ToolID    string    `json:"tool_id"`
		ToolName  string    `json:"tool_name"`
		State     State     `json:"state"`
		ExitCode  int       `json:"exit_code"`
		Err       string    `json:"error,omitempty"`
		StartedAt time.Time `json:"started_at"`
		EndedAt   time.Time `json:"ended_at,omitempty"`
		Log       string    `json:"log"`
		Version   string    `json:"version,omitempty"`
		Source    string    `json:"source,omitempty"`
	}{j.ID, j.ToolID, j.ToolName, j.State, j.ExitCode, j.Err, j.StartedAt, j.EndedAt, j.Log, j.Version, j.Source})
}

// Server 任务管理器 + HTTP 接口。
type Server struct {
	mu     sync.Mutex
	jobs   map[string]*Job
	sem    chan struct{} // 串行执行:apt/dpkg 不允许并发
	pkgMgr PkgMgr
	logger *log.Logger
	// base 是任务的父 ctx。**绝不能**用 HTTP 请求的 ctx:handler 返回后它立刻被取消,
	// 排队中的任务会当场变成 cancelled(实测踩过)。
	base context.Context

	// Version 是助手的构建版本,SelfPath 是它自己的可执行文件(由 main 注入)。
	// 助手装在 root 拥有的 /usr/local/bin,主服务被 ProtectSystem=strict 锁死写不进去,
	// 所以「助手自身的升级」只能由助手自己做。
	Version      string
	SelfPath     string
	ToolLockPath string
	InstallDir   string // default /usr/local/bin; container uses a separate tools volume
	StateDir     string // trusted receipts, outside task workspaces
	PythonEnv    string // optional managed venv on the same separate tools volume
	versions     *versionState
	versionHTTP  *http.Client // local fixture injection; never configured by HTTP input

	upgrading atomic.Bool
}

// NewServer 构造。base 为进程级生命周期 ctx(随守护进程退出而取消);传 nil 用 Background。
func NewServer(base context.Context, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	if base == nil {
		base = context.Background()
	}
	return &Server{
		jobs:     map[string]*Job{},
		sem:      make(chan struct{}, 1),
		pkgMgr:   DetectPkgMgr(),
		logger:   logger,
		base:     base,
		versions: newVersionState(),
	}
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Start 发起一次安装。toolID 必须命中 hosttools 白名单,否则拒绝。
// parent 必须是进程级 ctx,不能传 HTTP 请求的 ctx(见 Server.base 注释)。
func (s *Server) Start(parent context.Context, toolID, proxy string) (*Job, error) {
	return s.start(parent, toolID, proxy, "")
}

func (s *Server) start(parent context.Context, toolID, proxy, resolutionID string) (*Job, error) {
	if parent == nil {
		parent = s.base
	}
	tool, ok := hosttools.Lookup(toolID)
	if !ok {
		return nil, fmt.Errorf("未知工具 id: %q(不在白名单内)", toolID)
	}
	if tool.Kind == hosttools.KindManual {
		return nil, fmt.Errorf("%s 尚无自动安装配方，请按官方说明安装", tool.Name)
	}
	cleanProxy, err := ValidateProxy(proxy)
	if err != nil {
		return nil, err
	}
	if resolutionID == "" && tool.AvailableInPath() {
		return nil, fmt.Errorf("%s 已安装", tool.Name)
	}
	var installPin *hosttools.InstallPin
	var source string
	var aptPackages []string
	if resolutionID != "" {
		plan, err := s.takeResolution(resolutionID, toolID)
		if err != nil {
			return nil, err
		}
		installPin, source = &plan.Pin, plan.Source
		aptPackages = plan.AptPackages
	} else if tool.Kind == hosttools.KindGitHub || tool.Kind == hosttools.KindPip || tool.Kind == hosttools.KindData {
		return nil, fmt.Errorf("请先检测官方版本，检测完成后即可安装")
	}
	if installPin != nil {
		current := hosttools.ProbeInstalled(parent, tool)
		canUpdate := newerVersion(current.Version, installPin.Version)
		if tool.Kind == hosttools.KindApt && current.Available {
			installed, _, _ := aptVersions(parent, tool)
			if installed == strings.Join(aptPackages, ", ") {
				return nil, fmt.Errorf("工具已可用，没有新版本，无需重新安装")
			}
			if baseVersion(current.Version) == baseVersion(aptDisplayVersion(installed)) {
				current.Version = aptDisplayVersion(installed)
			}
			canUpdate = aptVersionNewer(current.Version, installPin.Version)
		}
		if tool.Kind == hosttools.KindData {
			canUpdate = current.Version != installPin.Version
		}
		if current.Available && (current.Version == "" || !canUpdate) {
			return nil, fmt.Errorf("工具已可用，没有可确认的新版本，无需重新安装")
		}
	}

	ctx, cancel := context.WithCancel(parent)
	j := &Job{
		ID: newID(), ToolID: tool.ID, ToolName: tool.Name,
		State: StateQueued, StartedAt: time.Now(), cancel: cancel,
		installPin: installPin,
		Source:     source, binDir: s.InstallDir,
		aptPackages: aptPackages,
	}
	if installPin != nil {
		j.Version = installPin.Version
	}
	if u, err := url.Parse(cleanProxy); err == nil && u.User != nil {
		j.redactions = append(j.redactions, cleanProxy, u.Redacted())
		if password, ok := u.User.Password(); ok && password != "" {
			j.redactions = append(j.redactions, password, "***")
		}
	}
	s.mu.Lock()
	for _, previous := range s.jobs {
		previous.mu.Lock()
		active := previous.State == StateQueued || previous.State == StateRunning
		previous.mu.Unlock()
		if active && previous.ToolID == toolID {
			s.mu.Unlock()
			cancel()
			return nil, fmt.Errorf("该工具已有安装任务，请等待或取消")
		}
	}
	if len(s.jobs) >= 200 {
		for id, previous := range s.jobs {
			previous.mu.Lock()
			terminal := previous.State != StateQueued && previous.State != StateRunning
			previous.mu.Unlock()
			if terminal {
				delete(s.jobs, id)
			}
		}
		if len(s.jobs) >= 100 {
			s.mu.Unlock()
			cancel()
			return nil, fmt.Errorf("安装队列已满")
		}
	}
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.logger.Printf("[audit] install start job=%s tool=%s proxy_configured=%t", j.ID, tool.ID, cleanProxy != "")
	go func() {
		defer cancel()
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			j.mu.Lock()
			j.EndedAt = time.Now()
			j.mu.Unlock()
			j.append("[cancel] 任务在排队时被取消\n")
			j.setState(StateCancelled)
			return
		}
		defer func() { <-s.sem }()
		if ctx.Err() != nil {
			j.append("[cancel] 任务在排队时被取消\n")
			j.setState(StateCancelled)
			return
		}
		j.setState(StateRunning)
		code, err := s.run(ctx, j, tool, cleanProxy)
		j.mu.Lock()
		j.ExitCode = code
		j.EndedAt = time.Now()
		j.mu.Unlock()
		switch {
		case ctx.Err() != nil:
			j.append("[cancel] 已取消\n")
			j.setState(StateCancelled)
		case err != nil:
			j.setErr(err)
			j.append(fmt.Sprintf("[fail] %v\n", err))
			j.setState(StateFailed)
		default:
			if err := s.saveReceipt(j); err != nil {
				j.append("[warn] 工具已安装，但版本记录保存失败\n")
			}
			j.append("[ok] 安装完成\n")
			hosttools.InvalidatePackageCache()
			s.versions.mu.Lock()
			delete(s.versions.cache, tool.ID)
			s.versions.mu.Unlock()
			j.setState(StateDone)
		}
		j.mu.Lock()
		state := j.State
		j.mu.Unlock()
		s.logger.Printf("[audit] install end job=%s tool=%s state=%s exit=%d err=%v",
			j.ID, tool.ID, state, code, err)
	}()
	return j, nil
}

// Get 取任务快照。
func (s *Server) Get(id string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	return j, ok
}

// List 全部任务(新→旧)。
func (s *Server) List() []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j)
	}
	return out
}

// Cancel 取消任务。
func (s *Server) Cancel(id string) bool {
	s.mu.Lock()
	j, ok := s.jobs[id]
	s.mu.Unlock()
	if !ok {
		return false
	}
	if j.cancel != nil {
		j.cancel()
		return true
	}
	return false
}

// ── 执行 ─────────────────────────────────────────

// run 按配方执行,返回退出码与错误。
func (s *Server) run(ctx context.Context, j *Job, tool hosttools.Tool, proxy string) (int, error) {
	switch tool.Kind {
	case hosttools.KindApt:
		return s.runApt(ctx, j, tool, proxy)
	case hosttools.KindGitHub:
		return s.runGitHub(ctx, j, tool, proxy)
	case hosttools.KindPip:
		return s.runPip(ctx, j, tool, proxy)
	case hosttools.KindData:
		return s.runData(ctx, j, tool, proxy)
	}
	return 1, fmt.Errorf("不支持的安装方式: %s", tool.Kind)
}

// execLogged 执行一条命令,输出实时追加到任务日志。
func (s *Server) execLogged(ctx context.Context, j *Job, name string, argv []string, env []string) (int, error) {
	j.append(fmt.Sprintf("$ %s %s\n", name, strings.Join(argv, " ")))
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Env = append(os.Environ(), env...)
	w := &logWriter{j: j}
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), fmt.Errorf("%s 退出码 %d", name, ee.ExitCode())
	}
	return 1, err
}

type logWriter struct{ j *Job }

func (w *logWriter) Write(p []byte) (int, error) {
	w.j.append(string(p))
	return len(p), nil
}

func (s *Server) runApt(ctx context.Context, j *Job, tool hosttools.Tool, proxy string) (int, error) {
	if s.pkgMgr == PkgNone {
		return 1, fmt.Errorf("本机未检测到 apt/dnf")
	}
	// 先刷新索引;失败不致命(可能只是某个源不可达),继续尝试安装
	if code, err := s.execLogged(ctx, j, firstWord(aptUpdateArgv(s.pkgMgr, proxy)),
		aptUpdateArgv(s.pkgMgr, proxy)[1:], []string{"DEBIAN_FRONTEND=noninteractive"}); err != nil {
		j.append(fmt.Sprintf("[warn] 索引刷新失败(退出码 %d),继续尝试安装\n", code))
	}
	argv, err := aptArgv(tool, s.pkgMgr, proxy)
	if err != nil {
		return 1, err
	}
	if len(j.aptPackages) > 0 {
		packages, err := aptPlanPackages(tool, strings.Join(j.aptPackages, ", "))
		if err != nil {
			return 1, err
		}
		// Only a hostd-created candidate set supplies exact versions.
		argv = append(argv[:len(argv)-len(strings.Fields(tool.Apt))], packages...)
	}
	return s.execLogged(ctx, j, argv[0], argv[1:], []string{"DEBIAN_FRONTEND=noninteractive"})
}

func (s *Server) runPip(ctx context.Context, j *Job, tool hosttools.Tool, proxy string) (int, error) {
	if j.installPin == nil {
		return 1, fmt.Errorf("缺少有效版本计划，请重新检测")
	}
	tool.Pip += "==" + j.installPin.Version
	if s.PythonEnv != "" {
		python := filepath.Join(s.PythonEnv, "bin", "python3")
		if _, err := os.Stat(python); err != nil {
			if code, err := s.execLogged(ctx, j, "python3", []string{"-m", "venv", s.PythonEnv}, nil); err != nil {
				return code, err
			}
		}
		argv := []string{"-m", "pip", "install", "--disable-pip-version-check", "--index-url", "https://pypi.org/simple", tool.Pip}
		if proxy != "" {
			argv = append(argv, "--proxy", proxy)
		}
		return s.execLogged(ctx, j, python, argv, []string{"PIP_CONFIG_FILE=/dev/null", "PIP_INDEX_URL=https://pypi.org/simple", "PIP_EXTRA_INDEX_URL="})
	}
	argv := pipArgv(tool, proxy)
	return s.execLogged(ctx, j, argv[0], argv[1:], []string{"PIP_DISABLE_PIP_VERSION_CHECK=1"})
}

// runGitHub 从 GitHub Release 取单文件二进制并装到 /usr/local/bin。
func (s *Server) runGitHub(ctx context.Context, j *Job, tool hosttools.Tool, proxy string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	client := s.archiveClient(proxy)
	defer client.CloseIdleConnections()
	if j.installPin == nil {
		return 1, fmt.Errorf("缺少有效工具版本计划或安装包校验信息，请重新检测")
	}
	u := GitHubAssetURL(tool, j.installPin.Version)
	j.append(fmt.Sprintf("[*] 下载 %s\n", u))

	blob, err := fetchToolArchive(ctx, client, u)
	if err != nil {
		return 1, err
	}
	if err := verifyToolArchive(blob, *j.installPin); err != nil {
		return 1, err
	}
	j.append(fmt.Sprintf("[*] 已下载 %.1f MB,解压…\n", float64(len(blob))/(1<<20)))

	// 归档格式各家不一：ProjectDiscovery 系是 .zip，gitleaks/chisel/amass 是
	// .tar.gz 或 .gz，feroxbuster 是 .tar.gz。按内容嗅探决定走哪条解压路径，
	// 而不是靠资产名的后缀猜（后缀拼错就直接装不上）。
	return installFromArchive(j, blob, tool)
}

// installFromArchive 从归档里取出目标二进制装到 /usr/local/bin。
// 支持 zip / tar(.gz) / 单文件 gz 三种形态。
func installFromArchive(j *Job, blob []byte, tool hosttools.Tool) (int, error) {
	want := tool.BinInZip
	if want == "" {
		want = tool.Bin
	}
	// zip: PK\x03\x04
	if len(blob) > 4 && blob[0] == 'P' && blob[1] == 'K' {
		zr, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
		if err != nil {
			return 1, fmt.Errorf("压缩包无法解析: %w", err)
		}
		return installFromZip(j, zr, tool)
	}
	// gzip: \x1f\x8b
	if len(blob) > 2 && blob[0] == 0x1f && blob[1] == 0x8b {
		gr, err := gzip.NewReader(bytes.NewReader(blob))
		if err != nil {
			return 1, fmt.Errorf("gzip 无法解析: %w", err)
		}
		defer gr.Close()
		// 单文件 gz（内部就是那个二进制）或 tar.gz
		raw, err := io.ReadAll(io.LimitReader(gr, (256<<20)+1))
		if err != nil {
			return 1, err
		}
		if len(raw) > 256<<20 {
			return 1, fmt.Errorf("解压内容超过大小限制")
		}
		// 可能是 tar.gz（多个条目），也可能是单文件 gz（内部就是那个二进制）。
		// 用「能否在 tar 流里找到目标文件」区分：找到就装 tar 里的那个，
		// 找不到就把解压结果整体当成那个二进制。
		if data, ok := findInTar(raw, tool); ok {
			return installBinary(j, tool, data)
		}
		if strings.HasSuffix(tool.AssetTmpl, ".gz") && !strings.HasSuffix(tool.AssetTmpl, ".tar.gz") {
			return installBinary(j, tool, raw)
		}
		return 1, fmt.Errorf("压缩包里找不到二进制 %q", want)
	}
	// 纯 tar
	if data, ok := findInTar(blob, tool); ok {
		return installBinary(j, tool, data)
	}
	return 1, fmt.Errorf("无法识别的归档格式")
}

// findInTar 在 tar（tar.gz 解压后）里找目标二进制的内容。
// 找不到返回 ok=false——调用方据此回退到「整个文件就是那个二进制」。
func findInTar(blob []byte, tool hosttools.Tool) ([]byte, bool) {
	want := tool.BinInZip
	if want == "" {
		want = tool.Bin
	}
	tr := tar.NewReader(bytes.NewReader(blob))
	for {
		hdr, err := tr.Next()
		if err != nil {
			return nil, false
		}
		if hdr.Typeflag != tar.TypeReg || path.Base(hdr.Name) != want {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, (256<<20)+1))
		if err != nil || len(data) > 256<<20 {
			return nil, false
		}
		return data, true
	}
}

// installBinary 把二进制内容原子地装到 /usr/local/bin。
func installBinary(j *Job, tool hosttools.Tool, data []byte) (int, error) {
	want := tool.BinInZip
	if want == "" {
		want = tool.Bin
	}
	dst := filepath.Join(j.installBinDir(), tool.Bin)
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return 1, fmt.Errorf("写入 %s 失败: %w", tmp, err)
	}
	if _, err := out.Write(data); err != nil {
		out.Close()
		os.Remove(tmp)
		return 1, err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return 1, err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return 1, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return 1, err
	}
	j.append(fmt.Sprintf("[ok] %s → %s\n", want, dst))
	return 0, nil
}

func installFromZip(j *Job, zr *zip.Reader, tool hosttools.Tool) (int, error) {
	want := tool.BinInZip
	if want == "" {
		want = tool.Bin
	}
	for _, f := range zr.File {
		if path.Base(f.Name) != want || f.FileInfo().IsDir() {
			continue
		}
		if f.UncompressedSize64 > 256<<20 {
			return 1, fmt.Errorf("解压成员超过大小限制")
		}
		rc, err := f.Open()
		if err != nil {
			return 1, err
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, (256<<20)+1))
		if err != nil {
			return 1, err
		}
		if len(data) > 256<<20 {
			return 1, fmt.Errorf("解压成员超过大小限制")
		}
		return installBinary(j, tool, data)
	}
	return 1, fmt.Errorf("压缩包里找不到二进制 %q", want)
}

// ── 辅助 ─────────────────────────────────────────

func firstWord(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return argv[0]
}

// httpClient 构造带代理的 HTTP 客户端。
//
// socks5 必须走 golang.org/x/net/proxy 的 SOCKS5 dialer：
// http.ProxyURL 只懂 HTTP CONNECT 代理，喂它 socks5:// 会得到
// "unsupported protocol scheme" —— 表现为「填了 socks 代理但下载照样失败」，
// 而用户完全看不出是协议不匹配。
//
// 客户端构造统一走 proxyprobe 包：运行期的自测 API 用的是同一套逻辑，
// 避免出现「装机说代理通了、运行期却连不上」的口径割裂。
func httpClient(proxy string) *http.Client {
	c, err := proxyprobe.ClientFor(proxy, 10*time.Minute)
	if err != nil {
		// 代理串不合法时退回直连：让安装继续，错误由调用方在日志里报出来，
		// 而不是整个客户端建不起来。
		return &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	}
	return c
}

func latestTag(ctx context.Context, client *http.Client, apiURL string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API HTTP %d", resp.StatusCode)
	}
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", err
	}
	if payload.TagName == "" {
		return "", fmt.Errorf("响应里没有 tag_name")
	}
	return payload.TagName, nil
}

// ── HTTP ─────────────────────────────────────────

// Handler 返回挂在 Unix socket 上的 mux。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"ok": true, "service": "gondar-hostd",
			"version": s.Version,
			"pkg_mgr": string(s.pkgMgr),
			"tools":   len(hosttools.Inventory()),
		})
	})

	// /version 供主服务做版本对齐：主程序自我升级后会拿自己的版本号来敲门，
	// 助手发现自己落后就自我换装（见 UpgradeSelf 的注释：只能由它自己来做）。
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"service": "gondar-hostd", "version": s.Version})
	})

	mux.HandleFunc("POST /self-upgrade", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tag   string `json:"tag"`
			Proxy string `json:"proxy"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]any{"error": "请求体不是合法 JSON"})
			return
		}
		if !ValidVersionTag(req.Tag) {
			writeJSON(w, 400, map[string]any{"error": "版本号格式非法: " + req.Tag})
			return
		}
		cleanProxy, err := ValidateProxy(req.Proxy)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		if !s.upgrading.CompareAndSwap(false, true) {
			writeJSON(w, 409, map[string]any{"error": "已有一个自我换装在进行中"})
			return
		}
		writeJSON(w, 202, map[string]any{"ok": true, "target": req.Tag})
		// 用进程级 ctx：请求一返回它就被取消，下载会当场中断（同一课踩过一次）。
		go func() {
			if err := s.UpgradeSelf(s.base, req.Tag, cleanProxy); err != nil {
				log.Printf("[hostd] 自我换装失败：%v", err)
				s.upgrading.Store(false)
				return
			}
			log.Printf("[hostd] 已换装到 %s，退出由 systemd 重启", req.Tag)
			// 留一点时间把日志刷出去再退；Restart=always 会把新版本拉起来。
			time.Sleep(300 * time.Millisecond)
			os.Exit(0)
		}()
	})

	mux.HandleFunc("GET /tools", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"tools": hosttools.Inventory()})
	})
	mux.HandleFunc("GET /versions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("refresh") == "1" {
			s.versions.mu.Lock()
			delete(s.versions.cache, r.PathValue("id"))
			s.versions.mu.Unlock()
			hosttools.InvalidatePackageCache()
		}
		info, err := s.Versions(r.Context(), r.PathValue("id"), r.Header.Get("X-Gondar-Install-Proxy"))
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, info)
	})

	mux.HandleFunc("POST /jobs", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ToolID       string `json:"tool_id"`
			Proxy        string `json:"proxy"`
			ResolutionID string `json:"resolution_id"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]any{"error": "请求体不是合法 JSON"})
			return
		}
		job, err := s.start(s.base, req.ToolID, req.Proxy, req.ResolutionID)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 201, job)
	})

	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		job, ok := s.Get(r.PathValue("id"))
		if !ok {
			writeJSON(w, 404, map[string]any{"error": "任务不存在"})
			return
		}
		writeJSON(w, 200, job)
	})

	mux.HandleFunc("POST /jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if !s.Cancel(r.PathValue("id")) {
			writeJSON(w, 404, map[string]any{"error": "任务不存在"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	return mux
}

// Listen 建立 Unix socket 并设置权限(仅属主/属组可读写,杜绝本机其它用户触发安装)。
func Listen(socketPath string, mode os.FileMode, group string) (net.Listener, error) {
	_ = os.Remove(socketPath)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, mode); err != nil {
		ln.Close()
		return nil, err
	}
	if group != "" {
		if g, err := lookupGID(group); err == nil {
			if err := os.Chown(socketPath, 0, g); err != nil {
				ln.Close()
				return nil, err
			}
		} else {
			ln.Close()
			return nil, fmt.Errorf("找不到属组 %q: %w", group, err)
		}
	}
	return ln, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// lookupGID 把属组名解析成 gid。
func lookupGID(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}
