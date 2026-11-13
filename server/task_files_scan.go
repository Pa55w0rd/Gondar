package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

var taskRecentSlots = make(chan struct{}, 2)

type taskFileScan struct {
	Scanned      int      `json:"scanned"`
	SkippedLinks []string `json:"skipped_links"`
	Truncated    bool     `json:"truncated"`
	Reason       string   `json:"truncation_reason,omitempty"`
}

type taskFileWalkLimits struct{ Entries, Depth int }

// Both the number of returned files and the work spent discovering them are
// bounded. Directory and metadata reads use pinned handles; no link is walked.
func walkTaskFiles(ctx context.Context, dir *os.Root, rel string, limits taskFileWalkLimits, visit func(taskFileEntry) error) (taskFileScan, error) {
	result := taskFileScan{SkippedLinks: []string{}}
	var walk func(*os.Root, string, int) error
	walk = func(root *os.Root, current string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := root.Open(".")
		if err != nil {
			return err
		}
		defer f.Close()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries, err := f.ReadDir(128)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return err
				}
				if result.Scanned >= limits.Entries {
					result.Truncated, result.Reason = true, "entries"
					return nil
				}
				result.Scanned++
				info, err := root.Lstat(entry.Name())
				if err != nil {
					return err
				}
				name := filepath.Join(current, entry.Name())
				if workspaceIsLink(info) {
					result.SkippedLinks = append(result.SkippedLinks, workspaceDisplayPath(name))
					continue
				}
				if !info.IsDir() && !info.Mode().IsRegular() {
					return errWorkspaceType
				}
				if err := visit(taskFileFromInfo(name, info)); err != nil {
					return err
				}
				if !info.IsDir() {
					continue
				}
				if depth >= limits.Depth {
					result.Truncated, result.Reason = true, "depth"
					return nil
				}
				child, err := taskFileOpenDir(root, entry.Name(), info)
				if err != nil {
					return err
				}
				err = walk(child, name, depth+1)
				child.Close()
				if err != nil || result.Truncated {
					return err
				}
			}
			if errors.Is(err, io.EOF) || len(entries) == 0 {
				return nil
			}
		}
	}
	err := walk(dir, rel, 0)
	return result, err
}

func (s *Server) taskFilesRecent(w http.ResponseWriter, r *http.Request) {
	select {
	case taskRecentSlots <- struct{}{}:
	default:
		writeErr(w, 429, "最近文件查询较多，请稍后重试")
		return
	}
	defer func() { <-taskRecentSlots }()
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
	limit := findingPaginationParam(r.URL.Query().Get("limit"), 100, 200)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	files := []taskFileEntry{}
	scan, err := walkTaskFiles(ctx, dir, rel, taskFileWalkLimits{Entries: 10000, Depth: 32}, func(entry taskFileEntry) error {
		if !entry.Dir {
			files = append(files, entry)
		}
		return nil
	})
	if errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil {
		scan.Truncated, scan.Reason = true, "deadline"
	} else if err != nil {
		if r.Context().Err() == nil {
			workspaceError(w, err)
		}
		return
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].MTime == files[j].MTime {
			return files[i].Path < files[j].Path
		}
		return files[i].MTime > files[j].MTime
	})
	if len(files) > limit {
		files = files[:limit]
	}
	writeJSON(w, 200, map[string]any{"task_id": r.PathValue("id"), "path": workspaceDisplayPath(rel), "entries": files, "scan": scan, "limit": limit, "captured_at": time.Now().UnixMilli(), "read_only": true})
}
