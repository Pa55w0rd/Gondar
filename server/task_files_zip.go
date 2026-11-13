package server

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

const (
	maxTaskZIPEntries       = 2000
	maxTaskZIPBytes   int64 = 256 << 20
)

var (
	taskZIPSlots    = make(chan struct{}, 2)
	errTaskZIPLimit = errors.New("目录超过 ZIP 导出上限（2000 条目、256 MiB），请进入子目录后导出")
)

type taskZIPPlan struct {
	Entries    []taskFileEntry
	Scan       taskFileScan
	Path       string
	Bytes      int64
	CapturedAt time.Time
}

func (plan *taskZIPPlan) add(entry taskFileEntry) error {
	if len(plan.Entries) >= maxTaskZIPEntries {
		return errTaskZIPLimit
	}
	if !entry.Dir {
		if entry.Size < 0 || entry.Size > maxTaskZIPBytes-plan.Bytes {
			return errTaskZIPLimit
		}
		plan.Bytes += entry.Size
	}
	plan.Entries = append(plan.Entries, entry)
	return nil
}

func prepareTaskZIP(ctx context.Context, dir *os.Root, rel string) (taskZIPPlan, error) {
	plan := taskZIPPlan{Path: rel, CapturedAt: time.Now()}
	scan, err := walkTaskFiles(ctx, dir, rel, taskFileWalkLimits{Entries: 10000, Depth: 32}, func(entry taskFileEntry) error {
		return plan.add(entry)
	})
	plan.Scan = scan
	if err == nil && scan.Truncated {
		err = errTaskZIPLimit
	}
	return plan, err
}

type taskFileContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r taskFileContextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

type taskFileContextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w taskFileContextWriter) Write(b []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(b)
}

// Close the ZIP only after every captured file and the manifest succeed. An
// interrupted export deliberately has no valid central directory/success tail.
func writeTaskZIP(ctx context.Context, dst io.Writer, ws *workspaceRoot, plan taskZIPPlan, taskID string) error {
	zw := zip.NewWriter(taskFileContextWriter{ctx, dst})
	for _, entry := range plan.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name, err := filepath.Rel(plan.Path, filepath.FromSlash(entry.Path))
		if err != nil || !filepath.IsLocal(name) {
			return errWorkspaceBoundary
		}
		header, err := zip.FileInfoHeader(entry.info)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(name)
		if entry.Dir {
			header.Name += "/"
			if _, err := zw.CreateHeader(header); err != nil {
				return err
			}
			continue
		}
		target, _, err := taskFileTarget(ws, entry.Path, false)
		if err != nil {
			return err
		}
		if target.info == nil || !os.SameFile(entry.info, target.info) || target.info.Size() != entry.Size || !target.info.ModTime().Equal(entry.info.ModTime()) {
			target.close()
			return errWorkspaceChanged
		}
		f, err := target.open(false)
		target.close()
		if err != nil {
			return err
		}
		header.Method = zip.Deflate
		out, err := zw.CreateHeader(header)
		if err == nil {
			_, err = io.CopyN(out, taskFileContextReader{ctx, f}, entry.Size)
		}
		if err == nil {
			var extra [1]byte
			n, readErr := f.Read(extra[:])
			if n != 0 || !errors.Is(readErr, io.EOF) {
				err = errWorkspaceChanged
			}
		}
		if err == nil {
			actual, statErr := f.Stat()
			if statErr != nil || !os.SameFile(entry.info, actual) || actual.Size() != entry.Size || !actual.ModTime().Equal(entry.info.ModTime()) {
				err = errWorkspaceChanged
			}
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	manifest, err := zw.Create("__gondar_export_" + uuid.NewString() + ".json")
	if err != nil {
		return err
	}
	err = json.NewEncoder(manifest).Encode(map[string]any{"task_id": taskID, "path": workspaceDisplayPath(plan.Path), "captured_at": plan.CapturedAt.UTC().Format(time.RFC3339Nano), "entries": len(plan.Entries), "bytes": plan.Bytes, "skipped_links": plan.Scan.SkippedLinks, "note": "仅导出该任务目录中的普通文件，符号链接未跟随；运行目录不是事务快照。"})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return zw.Close()
}

func (s *Server) taskFilesZIP(w http.ResponseWriter, r *http.Request) {
	select {
	case taskZIPSlots <- struct{}{}:
	default:
		writeErr(w, 429, "同时最多导出两个目录，请稍后重试")
		return
	}
	defer func() { <-taskZIPSlots }()
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
	scanCtx, scanCancel := context.WithTimeout(r.Context(), 5*time.Second)
	plan, err := prepareTaskZIP(scanCtx, dir, rel)
	scanCancel()
	if err != nil {
		if errors.Is(err, errTaskZIPLimit) {
			writeErr(w, 413, err.Error())
		} else if errors.Is(err, context.DeadlineExceeded) {
			writeErr(w, 408, "目录预检超时，请进入子目录后导出")
		} else if r.Context().Err() == nil {
			workspaceError(w, err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Minute))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", attachmentDisposition("gondar-task-"+r.PathValue("id")+".zip"))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if err := writeTaskZIP(ctx, w, ws, plan, r.PathValue("id")); err != nil {
		panic(http.ErrAbortHandler)
	}
}
