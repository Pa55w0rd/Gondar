package server

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gondar/db"
	"gondar/report"
)

const maxWordFindings = 100
const maxWordBytes int64 = 200 << 20

type exportBudget struct {
	out       io.Writer
	remaining int64
	ctx       context.Context
}

func (b *exportBudget) Write(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		return 0, errors.New("Word 导出超出大小限制，请减少勾选数量")
	}
	n, err := b.out.Write(p)
	b.remaining -= int64(n)
	return n, err
}

// Complete the archive before exposing a download. A failed member must never
// turn into a superficially successful but incomplete report bundle.
func writeWordBundle(ctx context.Context, dest string, fs []*db.DBFinding, render func(*db.DBFinding, io.Writer) error) (err error) {
	if len(fs) > maxWordFindings {
		return errors.New("单次最多导出100份Word报告")
	}
	file, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			file.Close()
			os.Remove(dest)
		}
	}()
	zw := zip.NewWriter(&exportBudget{out: file, remaining: maxWordBytes, ctx: ctx})
	remaining := maxWordBytes
	for _, finding := range fs {
		if err = ctx.Err(); err != nil {
			return err
		}
		name := strings.TrimSuffix(report.FindingFilename(finding), ".md") + ".docx"
		var entry io.Writer
		entry, err = zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			return err
		}
		limit := min(remaining, 32<<20)
		member := &exportBudget{out: entry, remaining: limit, ctx: ctx}
		if err = render(finding, member); err != nil {
			return fmt.Errorf("漏洞 #%d Word 生成失败: %w", finding.ID, err)
		}
		remaining -= limit - member.remaining
	}
	if err = zw.Close(); err != nil {
		return err
	}
	err = file.Close()
	return err
}

func (s *Server) findingsWordExport(w http.ResponseWriter, r *http.Request, filter db.FindingFilter, ids []int64, scope string) {
	if len(ids) > maxWordFindings {
		writeErr(w, 413, "单次最多导出100份Word报告，请缩小筛选或勾选范围")
		return
	}
	if scope != "selected" {
		page, total, err := s.m.pg.ListFindingsPage(filter, maxWordFindings+1, 0)
		if err != nil {
			writeErr(w, 500, "查询导出范围失败")
			return
		}
		if len(page) > maxWordFindings || total > maxWordFindings {
			writeErr(w, 413, "单次最多导出100份Word报告，请缩小筛选或勾选范围")
			return
		}
		for _, f := range page {
			ids = append(ids, f.ID)
		}
	}
	fs := []*db.DBFinding{}
	if len(ids) > 0 {
		var err error
		fs, err = s.m.pg.ListFindingsForExport(filter, ids)
		if err != nil {
			writeErr(w, 500, "读取导出报告失败")
			return
		}
		wanted := map[int64]bool{}
		for _, id := range ids {
			wanted[id] = true
		}
		if len(fs) != len(wanted) {
			writeErr(w, 409, "导出范围已变化，请刷新后重试")
			return
		}
	}
	stage, err := os.MkdirTemp("", "gondar-word-export-")
	if err != nil {
		writeErr(w, 500, "创建导出目录失败")
		return
	}
	defer os.RemoveAll(stage)
	if err = s.evidenceStore().StageFindingsExport(r.Context(), fs, stage, false); err != nil {
		evidenceError(w, err)
		return
	}
	now := time.Now()
	dest := filepath.Join(stage, "reports.zip")
	err = writeWordBundle(r.Context(), dest, fs, func(f *db.DBFinding, out io.Writer) error {
		doc, err := s.findingWordDocument(f, now)
		if err != nil {
			return err
		}
		return doc.Encode(out)
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	file, err := os.Open(dest)
	if err != nil {
		writeErr(w, 500, "读取导出文件失败")
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", attachmentDisposition("gondar-findings-"+now.Format("20060102-150405")+".zip"))
	http.ServeContent(w, r, "reports.zip", now, file)
}
