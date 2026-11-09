package server

import (
	"net/http"
	"strconv"
)

// Suggestions are an explicit review aid based on a complete source URL or
// host-recorded intent lineage. This never assigns or replaces a finding.
func (s *Server) artifactFindingSuggestions(w http.ResponseWriter, r *http.Request) {
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
	taskID, err := strconv.ParseInt(a.TaskID, 10, 64)
	if err != nil {
		writeErr(w, 400, "任务编号无效")
		return
	}
	items, truncated, err := pg.ArtifactFindingSuggestions(taskID, id)
	if err != nil {
		writeErr(w, 500, "读取关联建议失败")
		return
	}
	writeJSON(w, 200, map[string]any{"suggestions": items, "truncated": truncated})
}
