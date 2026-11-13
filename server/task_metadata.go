package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"gondar/db"
)

const (
	maxTaskNameRunes           = 200
	maxTaskMetadataRequestSize = 16 << 10
)

// updateTaskMetadata changes list-only task metadata. It intentionally does not
// touch lifecycle state, scheduling, or the task's immutable description/goal.
func (s *Server) updateTaskMetadata(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if _, ok := s.m.Task(taskID); !ok {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskMetadataRequestSize)
	var request struct {
		Name             *string `json:"name"`
		Pinned           *bool   `json:"pinned"`
		AssetContextMode *string `json:"asset_context_mode"`
	}
	if err := decode(r, &request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "请求正文过大")
		} else {
			writeErr(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	if request.Name == nil && request.Pinned == nil && request.AssetContextMode == nil {
		writeErr(w, http.StatusBadRequest, "至少需要提供 name、pinned 或 asset_context_mode")
		return
	}
	if request.AssetContextMode != nil {
		mode, err := db.NormalizeAssetContextMode(*request.AssetContextMode)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		request.AssetContextMode = &mode
	}
	if request.Name != nil {
		name := strings.TrimSpace(*request.Name)
		if name == "" {
			writeErr(w, http.StatusBadRequest, "任务名称不能为空")
			return
		}
		if utf8.RuneCountInString(name) > maxTaskNameRunes {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("任务名称最多 %d 个字符", maxTaskNameRunes))
			return
		}
		request.Name = &name
	}
	task, err := s.m.UpdateTaskMetadata(taskID, db.TaskPatch{Name: request.Name, Pinned: request.Pinned, AssetContextMode: request.AssetContextMode})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if task == nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if request.AssetContextMode != nil {
		task.Notify()
	}
	writeJSON(w, http.StatusOK, taskDTO(task, s.resolvedTaskStatus(task)))
}
