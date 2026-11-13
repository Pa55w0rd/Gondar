package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func atomicChatUploadRequest(t *testing.T, scope, id, name, content string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/chat/upload?scope="+scope+"&id="+id, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

func TestChatUploadConcurrentSameNamePreservesEveryAttachment(t *testing.T) {
	s, dir := workspaceTestServer(t)
	uploadDir := filepath.Join(dir, "sessions", "parallel", "uploads")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploadDir, "evidence.txt"), []byte("existing evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	const count = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan chatAttachment, count)
	for i := 0; i < count; i++ {
		content := fmt.Sprintf("new evidence %02d", i)
		r := atomicChatUploadRequest(t, "session", "parallel", "evidence.txt", content)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := httptest.NewRecorder()
			s.chatUpload(w, r)
			var response struct {
				Attachments []chatAttachment `json:"attachments"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != http.StatusOK || len(response.Attachments) != 1 {
				t.Errorf("upload: status=%d body=%s err=%v", w.Code, w.Body.String(), err)
				return
			}
			attachment := response.Attachments[0]
			actual, err := os.ReadFile(attachment.Abs)
			if err != nil || string(actual) != content || attachment.Size != int64(len(content)) {
				t.Errorf("attachment=%+v content=%q want=%q err=%v", attachment, actual, content, err)
			}
			results <- attachment
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	names := map[string]bool{}
	for attachment := range results {
		if names[attachment.Name] || attachment.Name == "evidence.txt" {
			t.Errorf("reused filename %q", attachment.Name)
		}
		names[attachment.Name] = true
	}
	if len(names) != count {
		t.Fatalf("saved attachments=%d want=%d", len(names), count)
	}
	actual, err := os.ReadFile(filepath.Join(uploadDir, "evidence.txt"))
	if err != nil || string(actual) != "existing evidence" {
		t.Fatalf("existing evidence changed: %q err=%v", actual, err)
	}
}

type failingChatUploadReader struct{}

func (failingChatUploadReader) Read(p []byte) (int, error) {
	return copy(p, "partial evidence"), io.ErrUnexpectedEOF
}

func TestChatUploadFailedCopyRemovesOnlyNewAttachment(t *testing.T) {
	s, dir := workspaceTestServer(t)
	if err := os.WriteFile(filepath.Join(dir, "evidence.txt"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := s.openWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer ws.root.Close()
	if _, _, err := saveChatUpload(ws, ".", "evidence.txt", failingChatUploadReader{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("copy error=%v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "evidence.txt" {
		t.Fatalf("copy failure left new attachment: %v err=%v", entries, err)
	}
	actual, err := os.ReadFile(filepath.Join(dir, "evidence.txt"))
	if err != nil || string(actual) != "retained" {
		t.Fatalf("existing attachment changed: %q err=%v", actual, err)
	}
}

func TestChatUploadHonorsWorkspaceLinksAndProtectedDirectories(t *testing.T) {
	for _, protected := range []bool{false, true} {
		t.Run(fmt.Sprintf("protected=%t", protected), func(t *testing.T) {
			s, dir := workspaceTestServer(t)
			target := t.TempDir()
			if protected {
				target = filepath.Join(dir, "traffic")
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			chatDir := filepath.Join(dir, "sessions", "linked")
			if err := os.MkdirAll(chatDir, 0o755); err != nil {
				t.Fatal(err)
			}
			workspaceTestLink(t, target, filepath.Join(chatDir, "uploads"), true)
			r := atomicChatUploadRequest(t, "session", "linked", "evidence.txt", "must not escape")
			w := httptest.NewRecorder()
			s.chatUpload(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("upload escaped boundary: status=%d body=%s", w.Code, w.Body.String())
			}
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatalf("protected/outside directory changed: %v err=%v", entries, err)
			}
		})
	}
}

func TestChatUploadRemovesMultipartSpillOnSuccessAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", fail), func(t *testing.T) {
			s, dir := workspaceTestServer(t)
			if fail {
				// A file in place of the session directory makes the upload fail
				// after multipart parsing, without opening an external path.
				if err := os.WriteFile(filepath.Join(dir, "sessions"), []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			r := atomicChatUploadRequest(t, "session", "spill", "evidence.txt", strings.Repeat("evidence", 1024))
			// Force real disk spill with a small memory threshold before handing
			// this already-parsed request to the same upload endpoint.
			if err := r.ParseMultipartForm(1); err != nil {
				t.Fatal(err)
			}
			hdr := r.MultipartForm.File["file"][0]
			f, err := hdr.Open()
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
			w := httptest.NewRecorder()
			s.chatUpload(w, r)
			if (w.Code == http.StatusOK) == fail {
				t.Fatalf("unexpected status=%d body=%s", w.Code, w.Body.String())
			}
			if f, err := hdr.Open(); err == nil {
				f.Close()
				t.Fatal("multipart spill was retained")
			} else if !os.IsNotExist(err) {
				t.Fatalf("spill removal error=%v", err)
			}
		})
	}
}

type gatedChatUploadBody struct {
	io.ReadCloser
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *gatedChatUploadBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.ReadCloser.Read(p)
}

func TestChatUploadInFlightSurvivesDeleteBarrierAndDrains(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{dir: dir, tasks: map[string]*Task{"7": {ID: "7"}}}
	s := &Server{m: m, engine: NewEngine(m)}
	r := atomicChatUploadRequest(t, "task", "7", "evidence.txt", "retained before deletion")
	gate := &gatedChatUploadBody{ReadCloser: r.Body, entered: make(chan struct{}), release: make(chan struct{})}
	r.Body = gate
	finished := make(chan struct{})
	w := httptest.NewRecorder()
	go func() { s.chatUpload(w, r); close(finished) }()
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("upload did not drain")
		}
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not enter multipart parsing")
	}
	if s.engine.inflightCount("7") != 1 || !s.engine.BeginDelete("7") {
		t.Fatal("upload was not registered before deletion barrier")
	}
	blocked := httptest.NewRecorder()
	s.chatUpload(blocked, atomicChatUploadRequest(t, "task", "7", "blocked.txt", "blocked"))
	if blocked.Code != http.StatusConflict || s.engine.inflightCount("7") != 1 {
		t.Fatalf("delete barrier admitted new upload: status=%d inflight=%d", blocked.Code, s.engine.inflightCount("7"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.waitTaskQuiescent(ctx, "7"); !errors.Is(err, context.Canceled) {
		t.Fatalf("active upload was not held by delete barrier: %v", err)
	}
	close(gate.release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not finish")
	}
	if w.Code != http.StatusOK || s.engine.inflightCount("7") != 0 {
		t.Fatalf("admitted upload did not drain: status=%d inflight=%d body=%s", w.Code, s.engine.inflightCount("7"), w.Body.String())
	}
	if err := s.waitTaskQuiescent(context.Background(), "7"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "tasks", "7", "uploads", "evidence.txt"))
	if err != nil || !strings.Contains(string(content), "retained before deletion") {
		t.Fatalf("admitted attachment lost: %q err=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tasks", "7", "uploads", "blocked.txt")); !os.IsNotExist(err) {
		t.Fatalf("blocked upload created a file: %v", err)
	}
}
