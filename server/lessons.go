package server

import (
	"errors"
	"gondar/db"
	"net/http"
	"strconv"
)

func (s *Server) listTaskLessons(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "任务不存在")
		return
	}
	id, _ := strconv.ParseInt(t.ID, 10, 64)
	items, err := s.m.PG().ListTaskLessons(id)
	if err != nil {
		writeErr(w, 500, "读取经验失败")
		return
	}
	writeJSON(w, 200, map[string]any{"lessons": items})
}

func (s *Server) reviewTaskLesson(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "任务不存在")
		return
	}
	id, _ := strconv.ParseInt(t.ID, 10, 64)
	lid, err := strconv.ParseInt(r.PathValue("lid"), 10, 64)
	if err != nil || lid <= 0 {
		writeErr(w, 400, "经验编号无效")
		return
	}
	var args struct {
		Revision  int64  `json:"revision"`
		Status    string `json:"status"`
		Shared    bool   `json:"shared"`
		Note      string `json:"note"`
		ValidDays int    `json:"valid_days"`
	}
	if err := decode(r, &args); err != nil {
		writeErr(w, 400, "复核参数无效")
		return
	}
	lesson, err := s.m.PG().ReviewLesson(id, lid, args.Revision, args.Status, args.Shared, args.Note, args.ValidDays)
	if errors.Is(err, db.ErrLessonConflict) {
		writeErr(w, 409, err.Error())
		return
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, lesson)
}
