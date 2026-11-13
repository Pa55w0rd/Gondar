package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func taskFilesFixture(t *testing.T) (*Server, string, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	for _, id := range []string{"1", "2"} {
		if err := os.MkdirAll(filepath.Join(dir, "tasks", id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	task := &Task{ID: "1", Paused: true, Status: "failed"}
	s := &Server{m: &Manager{dir: dir, tasks: map[string]*Task{"1": task, "2": {ID: "2"}}}, engine: &Engine{}}
	mux := http.NewServeMux()
	s.registerTaskFileRoutes(mux)
	return s, dir, mux
}

func taskFilesRequest(t *testing.T, h http.Handler, id, action, rel string) *httptest.ResponseRecorder {
	t.Helper()
	out := httptest.NewRecorder()
	h.ServeHTTP(out, httptest.NewRequest(http.MethodGet, "/api/tasks/"+id+"/files/"+action+"?path="+url.QueryEscape(rel), nil))
	return out
}

func TestTaskFilesReadOnlyRootAndTaskExistence(t *testing.T) {
	s, dir, h := taskFilesFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "tasks", "1", "proof.txt"), []byte("<script>fixture</script>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks", "2", "secret.txt"), []byte("other task secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := taskFilesRequest(t, h, "1", "read", "proof.txt")
	var content struct {
		Content string `json:"content"`
	}
	if read.Code != 200 || json.Unmarshal(read.Body.Bytes(), &content) != nil || content.Content != "<script>fixture</script>" {
		t.Fatal("task text preview changed contents")
	}
	for _, action := range []string{"list", "read", "download", "recent", "zip"} {
		for _, rel := range []string{"../2/secret.txt", "sub/../../2/secret.txt", "/tasks/2/secret.txt", `C:\tasks\2\secret.txt`, `..\2\secret.txt`} {
			out := taskFilesRequest(t, h, "1", action, rel)
			if out.Code != 403 || strings.Contains(out.Body.String(), "other task secret") {
				t.Fatalf("%s %q: status=%d body=%s", action, rel, out.Code, out.Body.String())
			}
		}
	}
	for _, id := range []string{"active", "01", "999"} {
		out := taskFilesRequest(t, h, id, "list", "")
		if out.Code != 400 && out.Code != 404 {
			t.Fatalf("invalid/missing task %q accepted", id)
		}
	}
	s.m.mu.Lock()
	delete(s.m.tasks, "2")
	s.m.mu.Unlock()
	if out := taskFilesRequest(t, h, "2", "list", ""); out.Code != 404 {
		t.Fatal("orphan files exposed for deleted/archived task")
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		out := httptest.NewRecorder()
		h.ServeHTTP(out, httptest.NewRequest(method, "/api/tasks/1/files/read?path=proof.txt", strings.NewReader("replacement")))
		if out.Code != 405 {
			t.Fatalf("write method %s accepted: %d", method, out.Code)
		}
	}
	if task, _ := s.m.Task("1"); !task.Paused || task.Status != "failed" || s.engine.inflightCount("1") != 0 || s.engine.taskFileReadCount("1") != 0 {
		t.Fatal("read-only files changed task lifecycle or leaked operation count")
	}
	unauth := httptest.NewRecorder()
	s.requireAuth(h).ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/tasks/1/files/read?path=proof.txt&token=ignored", nil))
	if unauth.Code != 401 {
		t.Fatal("file route allowed anonymous or query-token access")
	}
}

func TestTaskFilesRejectCrossTaskAndRootJunctions(t *testing.T) {
	s, dir, h := taskFilesFixture(t)
	outside := filepath.Join(dir, "tasks", "2")
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("other task secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceTestLink(t, outside, filepath.Join(dir, "tasks", "1", "escape"), true)
	for _, action := range []string{"list", "read", "download", "recent", "zip"} {
		rel := "escape"
		if action == "read" || action == "download" {
			rel += "/secret.txt"
		}
		out := taskFilesRequest(t, h, "1", action, rel)
		if out.Code != 403 || strings.Contains(out.Body.String(), "other task secret") {
			t.Fatalf("junction %s escaped: %d %s", action, out.Code, out.Body.String())
		}
	}
	recent := taskFilesRequest(t, h, "1", "recent", "")
	var listing struct {
		Entries []taskFileEntry `json:"entries"`
		Scan    taskFileScan    `json:"scan"`
	}
	if recent.Code != 200 || json.Unmarshal(recent.Body.Bytes(), &listing) != nil || len(listing.Entries) != 0 || len(listing.Scan.SkippedLinks) != 1 {
		t.Fatal("recent scan followed a cross-task junction")
	}
	// Even a configured task root may not alias another task.
	rootAlias := filepath.Join(dir, "tasks", "3")
	workspaceTestLink(t, outside, rootAlias, true)
	s.m.mu.Lock()
	s.m.tasks["3"] = &Task{ID: "3"}
	s.m.mu.Unlock()
	if out := taskFilesRequest(t, h, "3", "list", ""); out.Code != 403 {
		t.Fatal("task-root junction exposed another task")
	}
}

func TestTaskFileDeleteBarrierPinsAdmittedReader(t *testing.T) {
	s, dir, _ := taskFilesFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "tasks", "1", "proof.txt"), []byte("retained proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/tasks/1/files/read", nil)
	r.SetPathValue("id", "1")
	ws, done, ok := s.openTaskFiles(httptest.NewRecorder(), r)
	if !ok {
		t.Fatal("reader was not admitted")
	}
	if s.engine.taskFileReadCount("1") != 1 || s.engine.inflightCount("1") != 0 {
		done()
		t.Fatal("admitted reader missing from deletion drain")
	}
	if !s.engine.BeginDelete("1") {
		done()
		t.Fatal("could not install deletion barrier")
	}
	denied := httptest.NewRecorder()
	_, _, allowed := s.openTaskFiles(denied, r)
	if allowed || denied.Code != 409 {
		done()
		t.Fatal("reader bypassed deletion barrier")
	}
	target, _, err := taskFileTarget(ws, "proof.txt", true)
	if err != nil {
		done()
		t.Fatal(err)
	}
	f, err := target.open(false)
	target.close()
	if err != nil {
		done()
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	f.Close()
	done()
	if err != nil || string(data) != "retained proof" || s.engine.inflightCount("1") != 0 || s.engine.taskFileReadCount("1") != 0 {
		t.Fatal("admitted read failed or retained its drain count")
	}
}

func TestTaskFilesBoundedListingScanAndZIPBudget(t *testing.T) {
	_, dir, h := taskFilesFixture(t)
	rootPath := filepath.Join(dir, "tasks", "1")
	for i := 0; i < maxTaskFileList+1; i++ {
		if err := os.WriteFile(filepath.Join(rootPath, fmt.Sprintf("%04d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := taskFilesRequest(t, h, "1", "list", "")
	var listing struct {
		Entries   []taskFileEntry `json:"entries"`
		Truncated bool            `json:"truncated"`
	}
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &listing) != nil || len(listing.Entries) != maxTaskFileList || !listing.Truncated {
		t.Fatal("directory list is not bounded/reported")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	seen := 0
	scan, err := walkTaskFiles(context.Background(), root, ".", taskFileWalkLimits{Entries: 3, Depth: 2}, func(taskFileEntry) error { seen++; return nil })
	if err != nil || seen != 3 || scan.Scanned != 3 || !scan.Truncated || scan.Reason != "entries" {
		t.Fatalf("traversal work not bounded: %+v seen=%d err=%v", scan, seen, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := walkTaskFiles(ctx, root, ".", taskFileWalkLimits{Entries: 10, Depth: 2}, func(taskFileEntry) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal("scan ignored cancellation")
	}
	if _, err := prepareTaskZIP(context.Background(), root, "."); !errors.Is(err, errTaskZIPLimit) {
		t.Fatal("ZIP entry cap did not reject oversized plan")
	}
	plan := taskZIPPlan{}
	if err := plan.add(taskFileEntry{wsEntry: wsEntry{Size: maxTaskZIPBytes}}); err != nil {
		t.Fatal(err)
	}
	if err := plan.add(taskFileEntry{wsEntry: wsEntry{Size: 1}}); !errors.Is(err, errTaskZIPLimit) || plan.Bytes != maxTaskZIPBytes || len(plan.Entries) != 1 {
		t.Fatal("ZIP byte bound overflowed or retained rejected entry")
	}
}

func TestTaskZIPRealHTTPAndChangedFileCannotLookComplete(t *testing.T) {
	s, dir, h := taskFilesFixture(t)
	rootPath := filepath.Join(dir, "tasks", "1")
	if err := os.Mkdir(filepath.Join(rootPath, "intent-42"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(rootPath, "intent-42", "proof.txt")
	if err := os.WriteFile(file, []byte("saved proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	response, err := http.Get(server.URL + "/api/tasks/1/files/zip?path=intent-42")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("ZIP HTTP failed: %d %v", response.StatusCode, err)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	found, manifest := false, false
	for _, file := range archive.File {
		if file.Name == "proof.txt" {
			reader, err := file.Open()
			if err != nil {
				t.Fatal(err)
			}
			contents, err := io.ReadAll(reader)
			reader.Close()
			if err != nil || string(contents) != "saved proof" {
				t.Fatal("ZIP lost proof contents")
			}
			found = true
		}
		if strings.HasPrefix(file.Name, "__gondar_export_") {
			manifest = true
		}
		if strings.Contains(file.Name, "..") || strings.HasPrefix(file.Name, "/") {
			t.Fatal("ZIP names escaped selected directory")
		}
	}
	if !found || !manifest || s.engine.inflightCount("1") != 0 || s.engine.taskFileReadCount("1") != 0 {
		t.Fatal("successful ZIP lost manifest/files or retained task operation")
	}
	r := httptest.NewRequest(http.MethodGet, "/api/tasks/1/files/zip", nil)
	r.SetPathValue("id", "1")
	ws, done, ok := s.openTaskFiles(httptest.NewRecorder(), r)
	if !ok {
		t.Fatal("open failed")
	}
	defer done()
	target, rel, err := taskFileTarget(ws, "intent-42", true)
	if err != nil {
		t.Fatal(err)
	}
	defer target.close()
	root, err := target.openDirectory()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	plan, err := prepareTaskZIP(context.Background(), root, rel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("changed proof with a different size"), 0o600); err != nil {
		t.Fatal(err)
	}
	var interrupted bytes.Buffer
	if err := writeTaskZIP(context.Background(), &interrupted, ws, plan, "1"); err == nil {
		t.Fatal("changed file reported success")
	}
	if _, err := zip.NewReader(bytes.NewReader(interrupted.Bytes()), int64(interrupted.Len())); err == nil {
		t.Fatal("failed ZIP retained a complete central directory")
	}
}

type cancelTaskZIPWriter struct {
	cancel context.CancelFunc
	bytes.Buffer
}

func (w *cancelTaskZIPWriter) Write(data []byte) (int, error) {
	if len(data) > 64 {
		data = data[:64]
	}
	n, _ := w.Buffer.Write(data)
	w.cancel()
	return n, context.Canceled // real transports can fail during a buffered flush
}

func TestTaskFilesRecentOrderDepthAndZIPCancellation(t *testing.T) {
	s, dir, h := taskFilesFixture(t)
	rootPath := filepath.Join(dir, "tasks", "1")
	for _, name := range []string{"older.txt", "newer.txt"} {
		p := filepath.Join(rootPath, name)
		if err := os.WriteFile(p, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		modified := time.Unix(1700000000, 0)
		if name == "newer.txt" {
			modified = modified.Add(time.Hour)
		}
		if err := os.Chtimes(p, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(rootPath, "a", "b", "c"), 0o700); err != nil {
		t.Fatal(err)
	}
	out := taskFilesRequest(t, h, "1", "recent", "")
	var listing struct {
		Entries []taskFileEntry `json:"entries"`
		Scan    taskFileScan    `json:"scan"`
	}
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &listing) != nil || len(listing.Entries) != 2 || listing.Entries[0].Name != "newer.txt" || listing.Scan.Truncated {
		t.Fatal("recent files did not use global captured modification order")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	scan, err := walkTaskFiles(context.Background(), root, ".", taskFileWalkLimits{Entries: 100, Depth: 1}, func(taskFileEntry) error { return nil })
	if err != nil || !scan.Truncated || scan.Reason != "depth" {
		t.Fatal("deep directory scan did not stop/report its boundary")
	}
	r := httptest.NewRequest(http.MethodGet, "/api/tasks/1/files/zip", nil)
	r.SetPathValue("id", "1")
	ws, done, ok := s.openTaskFiles(httptest.NewRecorder(), r)
	if !ok {
		t.Fatal("open failed")
	}
	defer done()
	plan, err := prepareTaskZIP(context.Background(), root, ".")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &cancelTaskZIPWriter{cancel: cancel}
	if err := writeTaskZIP(ctx, writer, ws, plan, "1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("stream cancellation ignored: %v", err)
	}
	if _, err := zip.NewReader(bytes.NewReader(writer.Bytes()), int64(writer.Len())); err == nil {
		t.Fatal("cancelled ZIP looked complete")
	}
	for len(taskZIPSlots) < cap(taskZIPSlots) {
		taskZIPSlots <- struct{}{}
	}
	busy := taskFilesRequest(t, h, "1", "zip", "")
	for len(taskZIPSlots) > 0 {
		<-taskZIPSlots
	}
	if busy.Code != 429 {
		t.Fatal("ZIP concurrency was not bounded")
	}
}

func TestTaskFilesPinnedRootDoesNotFollowReplacement(t *testing.T) {
	s, dir, _ := taskFilesFixture(t)
	original := filepath.Join(dir, "tasks", "1")
	if err := os.WriteFile(filepath.Join(original, "proof.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/tasks/1/files/read", nil)
	r.SetPathValue("id", "1")
	ws, done, ok := s.openTaskFiles(httptest.NewRecorder(), r)
	if !ok {
		t.Fatal("open failed")
	}
	defer done()
	if err := os.Rename(original, original+"-moved"); err != nil {
		t.Skipf("当前文件系统不允许重命名已固定的目录：%v", err)
	}
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original, "proof.txt"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	target, _, err := taskFileTarget(ws, "proof.txt", true)
	if err != nil {
		t.Fatal(err)
	}
	defer target.close()
	f, err := target.open(false)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "original" {
		t.Fatal("pinned root followed a replacement directory")
	}
}
