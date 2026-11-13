package server

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"gondar/selfupdate"
)

type updateApplyRequest struct {
	RequestID     string `json:"request_id"`
	TargetVersion string `json:"target_version"`
}

var errUpdateAcceptanceStorage = errors.New("无法保存更新受理记录，未启动安装")

type updateRuntime struct {
	Version      string `json:"version"`
	BootID       string `json:"boot_id"`
	BinarySHA256 string `json:"binary_sha256"`
}

func (h *updateHub) runtimeIdentity() updateRuntime {
	if h.runtime != nil {
		return h.runtime()
	}
	return updateRuntime{Version: BuildVersion, BootID: selfupdate.BootID(), BinarySHA256: selfupdate.RuntimeBinarySHA256()}
}

func (h *updateHub) saveOperation(op selfupdate.Operation) error {
	if h.persist != nil {
		return h.persist(op)
	}
	return selfupdate.SaveOperation(op)
}

func (h *updateHub) lastOperation() (selfupdate.Operation, bool) {
	if h.load != nil {
		return h.load()
	}
	return selfupdate.LoadOperation()
}

func validUpdateRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func sameUpdateTarget(op selfupdate.Operation, target string) bool {
	return op.Repository == selfupdate.Repo && op.TargetVersion == strings.TrimPrefix(target, "v")
}

// beginOperation persists acknowledgement before work leaves the HTTP request.
// Only the last operation is retained; reusing its request ID cannot reinstall it.
func (h *updateHub) beginOperation(req updateApplyRequest, target string) (selfupdate.Operation, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	previous := h.operation
	if stored, ok := h.lastOperation(); ok {
		if stored.RequestID == req.RequestID {
			previous = &stored
		} else if stored.Phase == "activating" || stored.Phase == "settling" {
			return selfupdate.Operation{}, false, errors.New("上次更新尚在稳定启动确认中")
		}
	}
	if req.RequestID != "" && previous != nil && previous.RequestID == req.RequestID {
		if !sameUpdateTarget(*previous, target) {
			return selfupdate.Operation{}, false, errors.New("同一更新请求不能更改目标版本")
		}
		return *previous, false, nil
	}
	if h.running || h.cur.Phase == selfupdate.PhaseStaged {
		return selfupdate.Operation{}, false, errors.New("已有一个更新正在进行中或等待重启")
	}
	if req.RequestID == "" {
		req.RequestID = uuid.NewString() // old clients still get a tracked operation
	}
	identity := h.runtimeIdentity()
	op := selfupdate.Operation{ID: uuid.NewString(), RequestID: req.RequestID, Repository: selfupdate.Repo, FromVersion: identity.Version, TargetVersion: strings.TrimPrefix(target, "v"), Asset: selfupdate.AssetName(target, runtime.GOOS, runtime.GOARCH), AcceptedBootID: identity.BootID, Phase: "accepted", UpdatedAt: time.Now().Unix()}
	if err := h.saveOperation(op); err != nil {
		return selfupdate.Operation{}, false, errUpdateAcceptanceStorage
	}
	h.operation, h.running = &op, true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: "准备中…", Version: target, OperationID: op.ID, RequestID: op.RequestID}
	h.fanout(h.cur)
	return op, true, nil
}

func updateAcknowledgement(w http.ResponseWriter, op selfupdate.Operation) {
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "target": op.TargetVersion, "operation_id": op.ID, "request_id": op.RequestID, "operation": op})
}

// updateStatus is authenticated by Handler, just like version/update/check.
// An operation receipt remains readable after marker removal and process restart.
func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) {
	operationID, requestID := r.URL.Query().Get("operation_id"), r.URL.Query().Get("request_id")
	h := updHub
	h.mu.Lock()
	var op selfupdate.Operation
	if h.operation != nil {
		op = *h.operation
	}
	progress := h.cur
	h.mu.Unlock()
	if stored, ok := h.lastOperation(); ok && !(op.ID == stored.ID && op.Phase == "failed") && (op.ID == "" || stored.ID == op.ID || operationID == stored.ID || requestID == stored.RequestID) {
		op = stored
	}
	if op.ID == "" || operationID != "" && op.ID != operationID || requestID != "" && op.RequestID != requestID {
		writeErr(w, http.StatusNotFound, "尚未找到本次更新受理记录")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"operation": op, "runtime": h.runtimeIdentity(), "progress": progress})
}

func (h *updateHub) stageOperation(ctx context.Context, client *http.Client, release *selfupdate.Release, current string, op selfupdate.Operation) {
	stage := h.stage
	if stage == nil {
		stage = selfupdate.StageWithOperation
	}
	err := stage(ctx, client, release, current, op, h.publish)
	if err != nil {
		op.Phase, op.Error = "failed", err.Error()
		_ = h.saveOperation(op)
	}
	h.finish(err)
	if err == nil {
		if h.restart != nil {
			h.restart()
		} else {
			time.Sleep(1500 * time.Millisecond)
			requestRestart()
		}
	}
}
