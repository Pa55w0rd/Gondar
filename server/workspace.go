package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Workspace file manager — browse / view / edit / download / upload / delete the
// shared work dir (s.m.dir), where all agents write their artifacts. Every path is
// confined to the work dir root (traversal via ".." is neutralised). All routes sit
// behind requireAuth (see Handler()).

const (
	maxWorkspaceRead   = 2 << 20   // 2 MiB: files bigger than this aren't inlined for view/edit (download instead)
	maxWorkspaceUpload = 512 << 20 // 512 MiB per upload request
)

// workspaceProtectedDirs 是工作区文件 API 不可修改的系统内部目录。
//
// 工作空间根目录就是 s.m.dir（data 目录），系统把运行时数据直接放在它下面。
// 文件管理器的路径约束只保证「不越出根目录」，但根目录里的这些子目录是
// 系统自己在用的：删掉之后要么不可再生，要么要花很久重建。
//
//	traffic          —— 流量采集库（实测 3.3G / 34 万条）。删了历史请求
//	                    响应全丢，漏洞报告里的 HTTP 证据链随之断掉。
//	evidence         —— 漏洞绑定的流量正文（按 sha256 存 blob）。删了
//	                    findings 里的绑定全部变成空指针。
//	nuclei-templates —— 漏洞模板库（实测 934M / 14077 个模板）。删了要
//	                    重新 git clone 一遍。
//
// 任务数据（tasks / sessions / transcripts）**不在**这份名单里：它们属于
// 某个任务，要清就通过 DELETE /api/tasks/{id} 走正规路径，而不是在工作
// 空间里顺手删一棵目录树。
//
// 文件 API 禁止写入和删除；系统内部采集与 agent 执行使用各自的文件接口。
var workspaceProtectedDirs = []string{"traffic", "evidence", "nuclei-templates"}

// wsProtected reports whether an absolute path is one of the protected system
// directories (the directory itself, or anything under it).
func (s *Server) wsProtected(abs string) bool {
	root := filepath.Clean(s.m.dir)
	target := filepath.Clean(abs)
	for _, name := range workspaceProtectedDirs {
		p := filepath.Join(root, name)
		if target == p || strings.HasPrefix(target, p+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// wsResolve only normalizes the displayed path lexically. It is not an I/O
// boundary: handlers must also use workspaceRoot to resolve and pin aliases.
func (s *Server) wsResolve(rel string) (string, bool) {
	base := filepath.Clean(s.m.dir)
	rel = strings.TrimPrefix(strings.TrimSpace(rel), "/")
	clean := filepath.Clean("/" + rel) // e.g. "/a/../../etc" → "/etc" (still rooted at "/")
	abs := filepath.Clean(filepath.Join(base, clean))
	if abs != base && !strings.HasPrefix(abs, base+string(os.PathSeparator)) {
		return "", false
	}
	return abs, true
}

// wsRel renders an absolute path back as a workspace-relative path (forward slashes).
func (s *Server) wsRel(abs string) string {
	base := filepath.Clean(s.m.dir)
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

type wsEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix millis
}

// GET /api/workspace/list?path=<rel>
func (s *Server) wsList(w http.ResponseWriter, r *http.Request) {
	rel, err := s.workspaceRelative(r.URL.Query().Get("path"))
	if err != nil {
		workspaceError(w, err)
		return
	}
	ws, err := s.openWorkspace()
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer ws.root.Close()
	target, err := ws.resolve(rel, false, true, false)
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer target.close()
	dirRoot, err := target.openDirectory()
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer dirRoot.Close()
	f, err := dirRoot.Open(".")
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer f.Close()
	ents, err := f.ReadDir(-1)
	if err != nil {
		workspaceError(w, err)
		return
	}
	out := make([]wsEntry, 0, len(ents))
	for _, e := range ents {
		// DirEntry.Info may reopen File.Name on Unix. Use the pinned directory
		// handle for metadata as well, even if its original alias was replaced.
		info, err := dirRoot.Lstat(e.Name())
		if err != nil {
			continue
		}
		out = append(out, wsEntry{
			Name:  e.Name(),
			Path:  workspaceDisplayPath(filepath.Join(rel, e.Name())),
			Dir:   info.IsDir(),
			Size:  info.Size(),
			MTime: info.ModTime().UnixMilli(),
		})
	}
	// 目录在前，各自按名称排序。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, 200, map[string]any{"path": workspaceDisplayPath(rel), "entries": out})
}

// GET /api/workspace/read?path=<rel> — inline text for view/edit. Binary or oversize
// files return {binary:true}/{too_large:true} with no content (use download instead).
func (s *Server) wsRead(w http.ResponseWriter, r *http.Request) {
	rel, err := s.workspaceRelative(r.URL.Query().Get("path"))
	if err != nil {
		workspaceError(w, err)
		return
	}
	ws, err := s.openWorkspace()
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer ws.root.Close()
	target, err := ws.resolve(rel, false, true, false)
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer target.close()
	f, err := target.open(false)
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		workspaceError(w, err)
		return
	}
	path := workspaceDisplayPath(rel)
	if fi.Size() > maxWorkspaceRead {
		writeJSON(w, 200, map[string]any{"path": path, "size": fi.Size(), "too_large": true, "binary": true})
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, maxWorkspaceRead+1))
	if err != nil {
		workspaceError(w, err)
		return
	}
	if len(data) > maxWorkspaceRead {
		writeJSON(w, 200, map[string]any{"path": path, "size": len(data), "too_large": true, "binary": true})
		return
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		writeJSON(w, 200, map[string]any{"path": path, "size": len(data), "binary": true})
		return
	}
	writeJSON(w, 200, map[string]any{"path": path, "size": len(data), "binary": false, "content": string(data)})
}

// POST /api/workspace/write  {path, content} — create/overwrite a text file.
func (s *Server) wsWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rel, err := s.workspaceRelative(req.Path)
	if err != nil || rel == "." {
		writeErr(w, 400, "非法路径")
		return
	}
	ws, err := s.openWorkspace()
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer ws.root.Close()
	target, err := ws.resolve(rel, true, true, true)
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer target.close()
	if err := target.write(strings.NewReader(req.Content)); err != nil {
		workspaceError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": workspaceDisplayPath(rel)})
}

// POST /api/workspace/mkdir  {path}
func (s *Server) wsMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rel, err := s.workspaceRelative(req.Path)
	if err != nil || rel == "." {
		writeErr(w, 400, "非法路径")
		return
	}
	ws, err := s.openWorkspace()
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer ws.root.Close()
	target, err := ws.resolve(rel, true, true, true)
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer target.close()
	if target.info == nil {
		if err := target.parent.Mkdir(target.name, 0o755); err != nil {
			workspaceError(w, err)
			return
		}
	} else if !target.info.IsDir() {
		workspaceError(w, errWorkspaceType)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": workspaceDisplayPath(rel)})
}

// DELETE /api/workspace/delete?path=<rel> — removes a file or a directory tree
// (confined to the work dir; the root itself can't be deleted).
func (s *Server) wsDelete(w http.ResponseWriter, r *http.Request) {
	rel, err := s.workspaceRelative(r.URL.Query().Get("path"))
	if err != nil {
		workspaceError(w, err)
		return
	}
	if rel == "." {
		writeErr(w, 400, "不能删除工作区根目录")
		return
	}
	ws, err := s.openWorkspace()
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer ws.root.Close()
	// Validate a final alias's destination, but remove the alias itself, never its
	// resolved directory. RemoveAll does not recurse through symbolic links.
	target, err := ws.resolve(rel, true, false, false)
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer target.close()
	if target.info == nil {
		workspaceError(w, os.ErrNotExist)
		return
	}
	if workspaceIsLink(target.info) {
		checked, err := ws.resolve(rel, true, true, false)
		if err != nil {
			workspaceError(w, err)
			return
		}
		checked.close()
	}
	if err := target.parent.RemoveAll(target.name); err != nil {
		workspaceError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// GET /api/workspace/download?path=<rel> — stream a file as an attachment.
func (s *Server) wsDownload(w http.ResponseWriter, r *http.Request) {
	rel, err := s.workspaceRelative(r.URL.Query().Get("path"))
	if err != nil {
		workspaceError(w, err)
		return
	}
	ws, err := s.openWorkspace()
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer ws.root.Close()
	target, err := ws.resolve(rel, false, true, false)
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer target.close()
	f, err := target.open(false)
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		workspaceError(w, err)
		return
	}
	name := filepath.Base(rel)
	w.Header().Set("Content-Disposition", attachmentDisposition(name))
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// POST /api/workspace/upload?path=<dir> — multipart form field "file" (one or more).
func (s *Server) wsUpload(w http.ResponseWriter, r *http.Request) {
	rel, err := s.workspaceRelative(r.URL.Query().Get("path"))
	if err != nil {
		workspaceError(w, err)
		return
	}
	ws, err := s.openWorkspace()
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer ws.root.Close()
	dir, err := ws.resolve(rel, true, true, false)
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer dir.close()
	if dir.info == nil || !dir.info.IsDir() {
		workspaceError(w, errWorkspaceType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkspaceUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "解析上传失败或超出大小限制："+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeErr(w, 400, "缺少上传文件(表单字段 file)")
		return
	}
	saved := 0
	for _, hdr := range files {
		name := filepath.Base(hdr.Filename) // strip any path component
		if name == "" || name == "." || name == ".." {
			continue
		}
		// Resolve each destination too: an existing file alias can point into a
		// protected directory even when the selected upload directory is ordinary.
		target, err := ws.resolve(filepath.Join(rel, name), true, true, false)
		if err != nil {
			workspaceError(w, err)
			return
		}
		err = saveWorkspaceUpload(hdr, target)
		target.close()
		if err != nil {
			workspaceError(w, err)
			return
		}
		saved++
	}
	writeJSON(w, 200, map[string]any{"uploaded": saved})
}

func workspaceDisplayPath(rel string) string {
	if rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

func saveWorkspaceUpload(hdr *multipart.FileHeader, target *workspaceTarget) error {
	src, err := hdr.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	return target.write(src)
}
