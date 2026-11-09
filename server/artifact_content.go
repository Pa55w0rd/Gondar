package server

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"gondar/filepreview"
)

func (s *Server) previewArtifactContent(taskID string, artifactID int64, entry string) (*filepreview.Preview, error) {
	pg := s.m.PG()
	if pg == nil {
		return nil, fmt.Errorf("PG 不可用")
	}
	a, err := pg.GetTaskArtifact(artifactID)
	if err != nil || a == nil || a.TaskID != taskID {
		return nil, fmt.Errorf("附件不存在或不属于此任务")
	}
	root, err := filepath.EvalSymlinks(s.taskArtifactsDir(taskID))
	if err != nil {
		return nil, err
	}
	filename, err := filepath.EvalSymlinks(filepath.Join(s.m.dir, a.Storage))
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, filename)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("附件超出任务目录")
	}
	return filepreview.Read(filename, a.Name, entry)
}

func (s *Server) PreviewArtifactContent(taskID, artifactID int64, entry string) (*filepreview.Preview, error) {
	return s.previewArtifactContent(strconv.FormatInt(taskID, 10), artifactID, entry)
}

func (s *Server) serveArtifactContentPreview(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("aid"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "附件编号无效")
		return
	}
	pg := s.m.PG()
	if pg == nil {
		writeErr(w, 500, "PG 不可用")
		return
	}
	a, err := pg.GetTaskArtifact(id)
	if err != nil || a == nil || a.TaskID != r.PathValue("id") {
		writeErr(w, 404, "附件不存在")
		return
	}
	p, err := s.previewArtifactContent(r.PathValue("id"), id, r.URL.Query().Get("entry"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, p)
}
