package server

import (
	"errors"
	"gondar/db"
	"net/http"
	"strconv"
)

func (s *Server) listTaskResources(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "任务不存在")
		return
	}
	id, _ := strconv.ParseInt(t.ID, 10, 64)
	items, err := s.m.PG().ListTaskResources(id)
	if err != nil {
		writeErr(w, 500, "读取资源台账失败")
		return
	}
	writeJSON(w, 200, map[string]any{"resources": items})
}

func (s *Server) patchTaskResource(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "任务不存在")
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	id, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "资源编号无效")
		return
	}
	var args struct {
		Revision    int64 `json:"revision"`
		State, Note string
		EvidenceIDs []int64 `json:"evidence_ids"`
	}
	if err := decode(r, &args); err != nil {
		writeErr(w, 400, "资源参数无效")
		return
	}
	pt, err := s.m.PG().GetTask(taskID)
	if err != nil || pt == nil {
		writeErr(w, 404, "任务不存在")
		return
	}
	item, err := s.m.PG().Exploration(pt.ExplorationID).UpdateTaskResource(taskID, id, args.Revision, args.State, args.Note, args.EvidenceIDs)
	if errors.Is(err, db.ErrResourceConflict) {
		writeErr(w, 409, err.Error())
		return
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, item)
}
