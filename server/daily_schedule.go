package server

import (
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"time"
)

func (s *Server) listDailySchedules(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "任务编号无效")
		return
	}
	if _, ok := s.m.Task(r.PathValue("id")); !ok {
		writeErr(w, 404, "任务不存在")
		return
	}
	items, err := s.m.PG().ListDailySchedules(id)
	if err != nil {
		writeErr(w, 500, "读取计划失败")
		return
	}
	writeJSON(w, 200, map[string]any{"schedules": items})
}

func (s *Server) createDailySchedule(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "任务编号无效")
		return
	}
	if _, ok := s.m.Task(r.PathValue("id")); !ok {
		writeErr(w, 404, "任务不存在")
		return
	}
	var in struct {
		DailyAt        string `json:"daily_at"`
		Timezone       string `json:"timezone"`
		MaxRuns        int    `json:"max_runs"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "计划参数无效")
		return
	}
	plan, err := s.m.PG().CreateDailySchedule(id, in.DailyAt, in.Timezone, in.MaxRuns, in.TimeoutSeconds, time.Now())
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, plan)
}

func (s *Server) cancelDailySchedule(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "任务编号无效")
		return
	}
	plan, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil || plan <= 0 {
		writeErr(w, 400, "计划编号无效")
		return
	}
	if _, ok := s.m.Task(r.PathValue("id")); !ok {
		writeErr(w, 404, "任务不存在")
		return
	}
	err = s.m.PG().CancelDailySchedule(id, plan)
	if err == sql.ErrNoRows {
		writeErr(w, 404, "计划不存在")
		return
	}
	if err != nil {
		writeErr(w, 500, "取消计划失败")
		return
	}
	writeJSON(w, 200, map[string]any{"canceled": true})
}

func (sc *Scheduler) fireDailyTasks() {
	created := false
	for i := 0; i < 20; i++ {
		t, err := sc.pg.MaterializeDailySchedule(time.Now())
		if err != nil {
			log.Printf("[daily-schedule] 生成任务失败：%v", err)
			break
		}
		if t == nil {
			break
		}
		created = true
	}
	if created {
		sc.s.m.LoadExisting()
		sc.s.reconcileConcurrency()
	}
}
