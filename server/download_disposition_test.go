package server

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachmentDispositionPreservesUTF8AndSafeFallback(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"报告 最终+100%.docx", "报告 最终+100%.docx"},
		{`report "quoted"; final.csv`, `report "quoted"; final.csv`},
		{"report\r\nX-Test: injected.csv", "reportX-Test: injected.csv"},
		{"../目录\\报告.png", ".._目录_报告.png"},
		{"", "download"},
	} {
		header := attachmentDisposition(tc.name)
		for _, r := range header {
			if r < 32 || r > 126 {
				t.Fatalf("non-ASCII/control byte in header: %q", header)
			}
		}
		kind, params, err := mime.ParseMediaType(header)
		if err != nil || kind != "attachment" || params["filename"] != tc.want {
			t.Errorf("header=%q parsed=%q %v err=%v", header, kind, params, err)
		}
		if !strings.Contains(header, `filename="`) || !strings.Contains(header, "filename*=UTF-8''") {
			t.Fatalf("missing fallback or UTF-8 filename: %s", header)
		}
	}
}

func TestWorkspaceDownloadPreservesChineseFilename(t *testing.T) {
	dir := t.TempDir()
	name := "测试 报告+%.txt"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("saved evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{dir: dir}}
	w := httptest.NewRecorder()
	s.wsDownload(w, httptest.NewRequest(http.MethodGet, "/api/workspace/download?path="+url.QueryEscape(name), nil))
	_, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
	if w.Code != http.StatusOK || err != nil || params["filename"] != name || w.Body.String() != "saved evidence" {
		t.Fatalf("download: status=%d filename=%q body=%q err=%v", w.Code, params["filename"], w.Body.String(), err)
	}
}
