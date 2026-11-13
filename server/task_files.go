package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxTaskFileList = 2000

var taskFileSlots = make(chan struct{}, 8)

func (s *Server) registerTaskFileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tasks/{id}/files/list", s.taskFilesList)
	mux.HandleFunc("GET /api/tasks/{id}/files/read", s.taskFilesRead)
	mux.HandleFunc("GET /api/tasks/{id}/files/download", s.taskFilesDownload)
	mux.HandleFunc("GET /api/tasks/{id}/files/recent", s.taskFilesRecent)
	mux.HandleFunc("GET /api/tasks/{id}/files/zip", s.taskFilesZIP)
}

type taskFileEntry struct {
	wsEntry
	Link bool `json:"link,omitempty"`
	info os.FileInfo
}

func taskFilePath(raw string) (string, error) {
	if len(raw) > 4096 {
		return "", errWorkspaceBoundary
	}
	raw = strings.ReplaceAll(raw, `\`, "/")
	if strings.HasPrefix(raw, "/") || filepath.VolumeName(raw) != "" {
		return "", errWorkspaceBoundary
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." || strings.ContainsAny(part, ":\x00") {
			return "", errWorkspaceBoundary
		}
	}
	clean := path.Clean(raw)
	if clean != "." && !filepath.IsLocal(filepath.FromSlash(clean)) {
		return "", errWorkspaceBoundary
	}
	return filepath.FromSlash(clean), nil
}

func taskFileFromInfo(rel string, info os.FileInfo) taskFileEntry {
	return taskFileEntry{wsEntry: wsEntry{Name: info.Name(), Path: workspaceDisplayPath(rel), Dir: info.IsDir(), Size: info.Size(), MTime: info.ModTime().UnixMilli()}, Link: workspaceIsLink(info), info: info}
}

func taskFileOpenDir(parent *os.Root, name string, info os.FileInfo) (*os.Root, error) {
	if info == nil {
		return nil, os.ErrNotExist
	}
	if workspaceIsLink(info) || !info.IsDir() {
		return nil, errWorkspaceBoundary
	}
	dir, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	actual, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		dir.Close()
		return nil, errWorkspaceChanged
	}
	return dir, nil
}

// openTaskFiles pins only this live task's physical directory. Task relation
// inheritance never expands file access to a source task. The deletion barrier
// owns the lifetime of each read without holding a global mutex during I/O.
func (s *Server) openTaskFiles(w http.ResponseWriter, r *http.Request) (*workspaceRoot, func(), bool) {
	id := r.PathValue("id")
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != id {
		writeErr(w, 400, "任务 ID 无效")
		return nil, nil, false
	}
	if _, ok := s.m.Task(id); !ok {
		writeErr(w, 404, "任务不存在或已归档")
		return nil, nil, false
	}
	select {
	case taskFileSlots <- struct{}{}:
	default:
		writeErr(w, 429, "文件请求较多，请稍后重试")
		return nil, nil, false
	}
	var releaseReader func()
	release := func() {
		if releaseReader != nil {
			releaseReader()
		}
		<-taskFileSlots
	}
	if s.engine != nil {
		var admitted bool
		releaseReader, admitted = s.engine.beginTaskFileRead(id)
		if !admitted {
			release()
			writeErr(w, 409, "任务正在删除")
			return nil, nil, false
		}
	}
	if _, ok := s.m.Task(id); !ok {
		release()
		writeErr(w, 404, "任务不存在或已归档")
		return nil, nil, false
	}
	base, err := s.openWorkspace()
	if err != nil {
		release()
		workspaceError(w, err)
		return nil, nil, false
	}
	defer base.root.Close()
	info, err := base.root.Lstat("tasks")
	if err != nil {
		release()
		workspaceError(w, err)
		return nil, nil, false
	}
	tasks, err := taskFileOpenDir(base.root, "tasks", info)
	if err != nil {
		release()
		workspaceError(w, err)
		return nil, nil, false
	}
	defer tasks.Close()
	info, err = tasks.Lstat(id)
	if err != nil {
		release()
		workspaceError(w, err)
		return nil, nil, false
	}
	root, err := taskFileOpenDir(tasks, id, info)
	if err != nil {
		release()
		workspaceError(w, err)
		return nil, nil, false
	}
	abs, err := filepath.Abs(filepath.Join(s.m.dir, "tasks", id))
	if err != nil {
		root.Close()
		release()
		workspaceError(w, err)
		return nil, nil, false
	}
	physical, err := workspacePhysicalRoot(root, abs)
	if err != nil {
		root.Close()
		release()
		workspaceError(w, err)
		return nil, nil, false
	}
	ws := &workspaceRoot{root: root, aliases: []string{filepath.Clean(abs), filepath.Clean(physical)}}
	return ws, func() { root.Close(); release() }, true
}

func taskFileTarget(ws *workspaceRoot, raw string, followLeaf bool) (*workspaceTarget, string, error) {
	rel, err := taskFilePath(raw)
	if err != nil {
		return nil, "", err
	}
	target, err := ws.resolve(rel, false, followLeaf, false)
	return target, rel, err
}

func (s *Server) taskFilesList(w http.ResponseWriter, r *http.Request) {
	ws, done, ok := s.openTaskFiles(w, r)
	if !ok {
		return
	}
	defer done()
	target, rel, err := taskFileTarget(ws, r.URL.Query().Get("path"), true)
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer target.close()
	dir, err := target.openDirectory()
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer dir.Close()
	f, err := dir.Open(".")
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer f.Close()
	entries, err := f.ReadDir(maxTaskFileList + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		workspaceError(w, err)
		return
	}
	truncated := len(entries) > maxTaskFileList
	if truncated {
		entries = entries[:maxTaskFileList]
	}
	out := make([]taskFileEntry, 0, len(entries))
	for _, entry := range entries {
		if err := r.Context().Err(); err != nil {
			return
		}
		info, err := dir.Lstat(entry.Name())
		if err != nil {
			workspaceError(w, err)
			return
		}
		out = append(out, taskFileFromInfo(filepath.Join(rel, entry.Name()), info))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, 200, map[string]any{"task_id": r.PathValue("id"), "path": workspaceDisplayPath(rel), "entries": out, "read_only": true, "truncated": truncated, "limit": maxTaskFileList})
}

func (s *Server) taskFilesRead(w http.ResponseWriter, r *http.Request) {
	ws, done, ok := s.openTaskFiles(w, r)
	if !ok {
		return
	}
	defer done()
	target, rel, err := taskFileTarget(ws, r.URL.Query().Get("path"), true)
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
	info, err := f.Stat()
	if err != nil {
		workspaceError(w, err)
		return
	}
	result := map[string]any{"path": workspaceDisplayPath(rel), "size": info.Size(), "binary": true}
	if info.Size() > maxWorkspaceRead {
		result["too_large"] = true
		writeJSON(w, 200, result)
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, maxWorkspaceRead+1))
	if err != nil {
		workspaceError(w, err)
		return
	}
	if len(data) > maxWorkspaceRead {
		result["too_large"] = true
		writeJSON(w, 200, result)
		return
	}
	if bytes.IndexByte(data, 0) < 0 && utf8.Valid(data) {
		result["binary"], result["content"] = false, string(data)
	}
	writeJSON(w, 200, result)
}

func (s *Server) taskFilesDownload(w http.ResponseWriter, r *http.Request) {
	ws, done, ok := s.openTaskFiles(w, r)
	if !ok {
		return
	}
	defer done()
	target, rel, err := taskFileTarget(ws, r.URL.Query().Get("path"), true)
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
	info, err := f.Stat()
	if err != nil {
		workspaceError(w, err)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Minute))
	w.Header().Set("Content-Disposition", attachmentDisposition(filepath.Base(rel)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(rel), info.ModTime(), f)
}
