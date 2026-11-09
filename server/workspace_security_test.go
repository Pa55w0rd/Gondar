package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Windows 未授予创建符号链接权限时，目录用真实 junction 验证；文件链接则准确跳过。
func workspaceTestLink(t *testing.T, target, link string, directory bool) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	} else if runtime.GOOS != "windows" || !directory {
		t.Skipf("当前环境无法创建测试符号链接：%v", err)
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "$ErrorActionPreference='Stop'; New-Item -ItemType Junction -Path $env:GONDAR_WS_TEST_LINK -Target $env:GONDAR_WS_TEST_TARGET | Out-Null")
	cmd.Env = append(os.Environ(), "GONDAR_WS_TEST_LINK="+link, "GONDAR_WS_TEST_TARGET="+target)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("当前环境无法创建测试目录链接：%v，%s", err, out)
	}
	t.Log("使用 Windows junction 验证目录别名")
}

func workspaceTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	return &Server{m: &Manager{dir: dir}}, dir
}

func workspaceTestRequest(t *testing.T, s *Server, op, path string) *httptest.ResponseRecorder {
	t.Helper()
	method := http.MethodGet
	var body io.Reader
	contentType := ""
	switch op {
	case "write", "mkdir":
		method = http.MethodPost
		b, err := json.Marshal(map[string]string{"path": path, "content": "替换内容"})
		if err != nil {
			t.Fatal(err)
		}
		body, contentType = bytes.NewReader(b), "application/json"
	case "delete":
		method = http.MethodDelete
	case "upload":
		method = http.MethodPost
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, err := mw.CreateFormFile("file", "secret.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, "替换内容"); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		body, contentType = &buf, mw.FormDataContentType()
	}
	r := httptest.NewRequest(method, "/api/workspace/"+op+"?path="+url.QueryEscape(path), body)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	switch op {
	case "list":
		s.wsList(w, r)
	case "read":
		s.wsRead(w, r)
	case "write":
		s.wsWrite(w, r)
	case "mkdir":
		s.wsMkdir(w, r)
	case "delete":
		s.wsDelete(w, r)
	case "download":
		s.wsDownload(w, r)
	case "upload":
		s.wsUpload(w, r)
	default:
		t.Fatalf("未知操作：%s", op)
	}
	return w
}

func TestWorkspaceRejectsOutsideAlias(t *testing.T) {
	for _, op := range []string{"list", "read", "download", "write", "mkdir", "delete", "upload"} {
		t.Run(op, func(t *testing.T) {
			s, dir := workspaceTestServer(t)
			outside := t.TempDir()
			secret := filepath.Join(outside, "secret.txt")
			if err := os.WriteFile(secret, []byte("外部原始内容"), 0o600); err != nil {
				t.Fatal(err)
			}
			workspaceTestLink(t, outside, filepath.Join(dir, "escape"), true)
			path := "escape/secret.txt"
			if op == "list" || op == "upload" {
				path = "escape"
			} else if op == "mkdir" {
				path = "escape/new"
			}
			w := workspaceTestRequest(t, s, op, path)
			if w.Code != http.StatusForbidden {
				t.Errorf("越界操作应被拒绝，状态=%d，响应=%s", w.Code, w.Body.String())
			}
			data, err := os.ReadFile(secret)
			if err != nil || string(data) != "外部原始内容" {
				t.Errorf("外部文件被修改或删除：%q，%v", data, err)
			}
			if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
				t.Errorf("越界创建了目录：%v", err)
			}
		})
	}
}

func TestWorkspaceProtectedAliases(t *testing.T) {
	for _, protected := range workspaceProtectedDirs {
		for _, op := range []string{"write", "mkdir", "delete", "upload"} {
			t.Run(protected+"/"+op, func(t *testing.T) {
				s, dir := workspaceTestServer(t)
				target := filepath.Join(dir, protected)
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
				secret := filepath.Join(target, "secret.txt")
				if err := os.WriteFile(secret, []byte("系统原始内容"), 0o600); err != nil {
					t.Fatal(err)
				}
				workspaceTestLink(t, target, filepath.Join(dir, "alias"), true)
				workspaceTestLink(t, filepath.Join(dir, "alias"), filepath.Join(dir, "nested"), true)
				path := "nested/secret.txt"
				if op == "upload" || op == "delete" {
					path = "nested"
				} else if op == "mkdir" {
					path = "nested/new"
				}
				w := workspaceTestRequest(t, s, op, path)
				if w.Code != http.StatusForbidden {
					t.Errorf("受保护目录别名应拒绝修改，状态=%d，响应=%s", w.Code, w.Body.String())
				}
				data, err := os.ReadFile(secret)
				if err != nil || string(data) != "系统原始内容" {
					t.Errorf("系统文件被修改：%q，%v", data, err)
				}
			})
		}
	}
}

func TestWorkspaceInternalAliasesAndRootAlias(t *testing.T) {
	s, dir := workspaceTestServer(t)
	physical := t.TempDir()
	workspaceTestLink(t, physical, filepath.Join(dir, "configured-root"), true)
	s.m.dir = filepath.Join(dir, "configured-root")
	if err := os.Mkdir(filepath.Join(physical, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspaceTestLink(t, filepath.Join(physical, "files"), filepath.Join(physical, "inside"), true)
	if ws, err := s.openWorkspace(); err == nil {
		link, _ := ws.root.Readlink("inside")
		t.Logf("配置根别名=%q，内部别名=%q", ws.aliases, link)
		ws.root.Close()
	}
	for _, tc := range []struct{ op, path string }{
		{"write", "inside/new/secret.txt"}, {"read", "inside/new/secret.txt"},
		{"download", "inside/new/secret.txt"}, {"list", "inside/new"},
		{"upload", "inside/new"}, {"mkdir", "inside/made"},
	} {
		w := workspaceTestRequest(t, s, tc.op, tc.path)
		if w.Code != http.StatusOK {
			t.Fatalf("合法内部别名操作%s失败：%d %s", tc.op, w.Code, w.Body.String())
		}
		if (tc.op == "read" || tc.op == "download") && !strings.Contains(w.Body.String(), "替换内容") {
			t.Errorf("合法别名未读到真实文件：%s", w.Body.String())
		}
	}
	w := workspaceTestRequest(t, s, "delete", "inside")
	if w.Code != http.StatusOK {
		t.Fatalf("删除合法别名失败：%d %s", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(physical, "inside")); !os.IsNotExist(err) {
		t.Errorf("别名未删除：%v", err)
	}
	if _, err := os.Stat(filepath.Join(physical, "files", "new", "secret.txt")); err != nil {
		t.Errorf("删除别名不应递归删除真实目标：%v", err)
	}
}

func TestWorkspaceProtectedFileAlias(t *testing.T) {
	s, dir := workspaceTestServer(t)
	if err := os.Mkdir(filepath.Join(dir, "evidence"), 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "evidence", "secret.txt")
	if err := os.WriteFile(secret, []byte("系统原始内容"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceTestLink(t, secret, filepath.Join(dir, "secret.txt"), false)
	for _, op := range []string{"write", "upload", "delete"} {
		path := "secret.txt"
		if op == "upload" {
			path = ""
		}
		if w := workspaceTestRequest(t, s, op, path); w.Code != http.StatusForbidden {
			t.Errorf("文件别名绕过保护：%s=%d %s", op, w.Code, w.Body.String())
		}
	}
	if data, err := os.ReadFile(secret); err != nil || string(data) != "系统原始内容" {
		t.Errorf("系统文件已改变：%q，%v", data, err)
	}
}

func TestWorkspacePinnedParentSurvivesAliasSwap(t *testing.T) {
	s, dir := workspaceTestServer(t)
	parent := filepath.Join(dir, "ordinary")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "existing.txt"), []byte("锚定目录元数据"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := s.openWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer ws.root.Close()
	target, err := ws.resolve("ordinary/secret.txt", true, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer target.close()
	dirTarget, err := ws.resolve("ordinary", false, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dirTarget.close()
	dirRoot, err := dirTarget.openDirectory()
	if err != nil {
		t.Fatal(err)
	}
	defer dirRoot.Close()
	// Deterministically replace the checked parent before the actual write. The
	// open handle must stay with the original directory, not follow its new alias.
	moved := filepath.Join(dir, "pinned-original")
	if err := os.Rename(parent, moved); err != nil {
		t.Skipf("当前系统不允许重命名已打开目录：%v", err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("外部原始内容"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "existing.txt"), []byte("外"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceTestLink(t, outside, parent, true)
	if info, err := dirRoot.Lstat("existing.txt"); err != nil || info.Size() != int64(len("锚定目录元数据")) {
		t.Fatalf("列表元数据经替换后的父路径越界：%v，%v", info, err)
	}
	if err := target.write(strings.NewReader("锚定目录内容")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(outside, "secret.txt")); err != nil || string(data) != "外部原始内容" {
		t.Fatalf("父目录替换导致越界写入：%q，%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(moved, "secret.txt")); err != nil || string(data) != "锚定目录内容" {
		t.Fatalf("未写入已锚定目录：%q，%v", data, err)
	}
}

func TestWorkspaceLeafSwapDoesNotTruncate(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "new"}[missing], func(t *testing.T) {
			s, dir := workspaceTestServer(t)
			if err := os.Mkdir(filepath.Join(dir, "evidence"), 0o755); err != nil {
				t.Fatal(err)
			}
			protected := filepath.Join(dir, "evidence", "secret.txt")
			if err := os.WriteFile(protected, []byte("受保护内容"), 0o600); err != nil {
				t.Fatal(err)
			}
			leaf := filepath.Join(dir, "ordinary.txt")
			if !missing {
				if err := os.WriteFile(leaf, []byte("原始内容"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ws, err := s.openWorkspace()
			if err != nil {
				t.Fatal(err)
			}
			defer ws.root.Close()
			target, err := ws.resolve("ordinary.txt", true, true, false)
			if err != nil {
				t.Fatal(err)
			}
			defer target.close()
			if !missing {
				if err := os.Remove(leaf); err != nil {
					t.Fatal(err)
				}
			}
			workspaceTestLink(t, protected, leaf, false)
			if f, err := target.open(false); err == nil {
				f.Close()
				t.Fatal("最终文件被替换后仍允许读取不同 inode")
			}
			if err := target.write(strings.NewReader("不得写入")); err == nil {
				t.Fatal("最终文件被替换成链接后仍允许写入")
			}
			if data, err := os.ReadFile(protected); err != nil || string(data) != "受保护内容" {
				t.Fatalf("身份核验前已截断保护文件：%q，%v", data, err)
			}
		})
	}
}

func TestWorkspaceReadBoundsAndDownloadRange(t *testing.T) {
	s, dir := workspaceTestServer(t)
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), bytes.Repeat([]byte("a"), maxWorkspaceRead+1), 0o600); err != nil {
		t.Fatal(err)
	}
	w := workspaceTestRequest(t, s, "read", "large.txt")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"too_large":true`) || strings.Contains(w.Body.String(), `"content"`) {
		t.Fatalf("大文件内联限制失效：%d %s", w.Code, w.Body.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "range.txt"), []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/workspace/download?path=range.txt", nil)
	r.Header.Set("Range", "bytes=1-3")
	w = httptest.NewRecorder()
	s.wsDownload(w, r)
	if w.Code != http.StatusPartialContent || w.Body.String() != "bcd" {
		t.Fatalf("句柄下载未保留Range语义：%d %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceProtectedReadsRemainAvailable(t *testing.T) {
	for _, protected := range workspaceProtectedDirs {
		t.Run(protected, func(t *testing.T) {
			s, dir := workspaceTestServer(t)
			if err := os.Mkdir(filepath.Join(dir, protected), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, protected, "secret.txt"), []byte("可读系统内容"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, op := range []string{"read", "download", "list"} {
				path := protected + "/secret.txt"
				if op == "list" {
					path = protected
				}
				w := workspaceTestRequest(t, s, op, path)
				if w.Code != http.StatusOK {
					t.Errorf("保护目录读取回归：%s %d %s", op, w.Code, w.Body.String())
				}
			}
			for _, op := range []string{"write", "mkdir", "delete", "upload"} {
				path := protected + "/secret.txt"
				if op == "upload" {
					path = protected
				}
				w := workspaceTestRequest(t, s, op, path)
				if w.Code != http.StatusForbidden {
					t.Errorf("直接路径未保护：%s %d %s", op, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestWorkspaceRelativeAliasAndDotDot(t *testing.T) {
	s, dir := workspaceTestServer(t)
	for _, path := range []string{"alpha", "beta/sub"} {
		if err := os.MkdirAll(filepath.Join(dir, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "beta", "result.txt"), []byte("正确物理目标"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alpha", "result.txt"), []byte("错误词法目标"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Use actual relative symlinks here: a Windows junction is always absolute
	// and would not exercise the intended component-walking semantics.
	if err := os.Symlink(filepath.FromSlash("../beta/sub"), filepath.Join(dir, "alpha", "jump")); err != nil {
		t.Skipf("当前环境不支持相对符号链接：%v", err)
	}
	if err := os.Symlink(filepath.FromSlash("alpha/jump/../result.txt"), filepath.Join(dir, "alias")); err != nil {
		t.Skipf("当前环境不支持相对符号链接：%v", err)
	}
	w := workspaceTestRequest(t, s, "read", "alias")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "正确物理目标") {
		t.Fatalf("相对别名的..被提前词法折叠：%d %s", w.Code, w.Body.String())
	}
	if err := os.Symlink("loop", filepath.Join(dir, "loop")); err != nil {
		t.Fatal(err)
	}
	ws, err := s.openWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer ws.root.Close()
	if _, err := ws.resolve("loop", false, true, false); !errors.Is(err, errWorkspaceBoundary) {
		t.Fatalf("链接循环没有有界拒绝：%v", err)
	}
}
