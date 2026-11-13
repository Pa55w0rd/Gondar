package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClipboardUploadNamesRetainUsableExtensions(t *testing.T) {
	for _, tc := range []struct {
		name, mime, want string
	}{
		{"", "image/png", "pasted-file-1.png"},
		{"blob", "image/jpeg", "pasted-file-1.jpg"},
		{"BLOB", "image/webp", "pasted-file-1.webp"},
		{".", "image/png; charset=binary", "pasted-file-1.png"},
		{"", "not a MIME type", "pasted-file-1.bin"},
		{"", "application/x-unknown-clipboard-format", "pasted-file-1.bin"},
		{"截图.png", "image/png", "截图.png"},
		{"report.txt", "image/jpeg", "report.txt"},
	} {
		hdr := &multipart.FileHeader{Filename: tc.name, Header: textproto.MIMEHeader{"Content-Type": {tc.mime}}}
		if got := chatUploadName(hdr, 0); got != tc.want {
			t.Errorf("name(%q, %q) = %q, want %q", tc.name, tc.mime, got, tc.want)
		}
	}
}

func TestClipboardUploadDoesNotClobberPreviousImage(t *testing.T) {
	dir := t.TempDir()
	hdr := &multipart.FileHeader{Filename: "blob", Header: textproto.MIMEHeader{"Content-Type": {"image/png"}}}
	name := chatUploadName(hdr, 0)
	first := filepath.Join(dir, name)
	if err := os.WriteFile(first, []byte("first image"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{dir: dir}}
	ws, err := s.openWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer ws.root.Close()
	if got, _, err := saveChatUpload(ws, ".", name, strings.NewReader("second image")); err != nil || got != "pasted-file-1-1.png" {
		t.Fatalf("repeated paste path=%q err=%v", got, err)
	}
	if content, err := os.ReadFile(first); err != nil || string(content) != "first image" {
		t.Fatalf("previous clipboard evidence changed: %q %v", content, err)
	}
}

func TestClipboardImageUsesExistingChatUploadEndpoint(t *testing.T) {
	dir := t.TempDir()
	s := &Server{m: &Manager{dir: dir}}
	for _, expected := range []string{"pasted-file-1.png", "pasted-file-1-1.png"} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {`form-data; name="file"; filename="blob"`},
			"Content-Type":        {"image/png"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte("clipboard image")); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/chat/upload?scope=session&id=clipboard-test", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		w := httptest.NewRecorder()
		s.chatUpload(w, r)
		var response struct {
			Attachments []chatAttachment `json:"attachments"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != http.StatusOK || len(response.Attachments) != 1 {
			t.Fatalf("upload: status=%d body=%s err=%v", w.Code, w.Body.String(), err)
		}
		attachment := response.Attachments[0]
		if attachment.Name != expected || attachment.Path != "uploads/"+expected {
			t.Fatalf("attachment=%+v want filename=%q", attachment, expected)
		}
		content, err := os.ReadFile(filepath.Join(dir, "sessions", "clipboard-test", "uploads", expected))
		if err != nil || string(content) != "clipboard image" {
			t.Fatalf("saved clipboard image=%q err=%v", content, err)
		}
	}
}
