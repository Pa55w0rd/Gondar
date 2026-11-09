package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gondar/db"
	"gondar/filepreview"
)

func artifactContentFixture(t *testing.T) (*Server, *db.TaskArtifact, int64) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no test database: %v", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	if err := pg.EnsureTaskArtifactsTable(); err != nil {
		t.Fatal(err)
	}
	task, err := pg.CreateTask("artifact content fixture", "local inert evidence", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pg.Exec(`DELETE FROM task_artifacts WHERE task_id=$1`, strconv.FormatInt(task.ID, 10))
		pg.DeleteTaskCascade(task.ID, true, true)
	})
	s := &Server{m: &Manager{dir: t.TempDir(), pg: pg}}
	root := s.taskArtifactsDir(strconv.FormatInt(task.ID, 10))
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	storage := filepath.Join("tasks", strconv.FormatInt(task.ID, 10), "artifacts", "proof.txt")
	if err := os.WriteFile(filepath.Join(s.m.dir, storage), []byte("<script>untrusted evidence remains text</script>\n原始证据"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &db.TaskArtifact{TaskID: strconv.FormatInt(task.ID, 10), Name: "proof.txt", Kind: "log", Storage: storage, SourceURL: "https://one.example.invalid/path?x=1"}
	if err := pg.InsertTaskArtifact(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	return s, a, task.ID
}

func contentRequest(handler http.HandlerFunc, task string, id int64) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://local.invalid/controlled", nil)
	r.SetPathValue("id", task)
	r.SetPathValue("aid", strconv.FormatInt(id, 10))
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func TestArtifactContentPreviewOwnerBoundaryAndInertText(t *testing.T) {
	s, a, _ := artifactContentFixture(t)
	w := contentRequest(s.serveArtifactContentPreview, a.TaskID, a.ID)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("preview HTTP: %d %s", w.Code, w.Body.String())
	}
	var p filepreview.Preview
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Text, "<script>") || p.Truncated {
		t.Fatalf("inert text changed: %+v", p)
	}
	if w := contentRequest(s.serveArtifactContentPreview, "foreign-task", a.ID); w.Code != 404 {
		t.Fatal("foreign task evidence exposed")
	}
	if err := os.WriteFile(filepath.Join(s.m.dir, "private.txt"), []byte("synthetic-platform-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.PG().Exec(`UPDATE task_artifacts SET storage_path='private.txt' WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	w = contentRequest(s.serveArtifactContentPreview, a.TaskID, a.ID)
	if w.Code != 400 || strings.Contains(w.Body.String(), "synthetic-platform-secret") {
		t.Fatalf("out-of-directory data read: %d %s", w.Code, w.Body.String())
	}
}

func TestArtifactSuggestionsRequireExactURLAndSameTask(t *testing.T) {
	s, a, taskID := artifactContentFixture(t)
	pg := s.m.PG()
	other, err := pg.CreateTask("other suggestion fixture", "isolation", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.DeleteTaskCascade(other.ID, true, true) })
	var asset1, asset2 int64
	if err := pg.QueryRow(`INSERT INTO assets(type,url,method,domain,task_ids) VALUES('endpoint',$1,'GET','one.example.invalid',$2) RETURNING id`, a.SourceURL, []int64{taskID}).Scan(&asset1); err != nil {
		t.Fatal(err)
	}
	if err := pg.QueryRow(`INSERT INTO assets(type,url,method,domain,task_ids) VALUES('endpoint',$1,'GET','one.example.invalid',$2) RETURNING id`, "https://one.example.invalid/path?x=2", []int64{taskID}).Scan(&asset2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{asset1, asset2}) })
	match, err := pg.AddFinding(taskID, 0, "exact", "explicit candidate", "low", "summary", "evidence", "worker", []int64{asset1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.AddFinding(taskID, 0, "same-host", "wrong query", "low", "summary", "evidence", "worker", []int64{asset2}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.AddFinding(other.ID, 0, "other-task", "wrong task", "low", "summary", "evidence", "worker", []int64{asset1}); err != nil {
		t.Fatal(err)
	}
	w := contentRequest(s.artifactFindingSuggestions, a.TaskID, a.ID)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var result struct {
		Suggestions []struct {
			FindingID int64 `json:"finding_id"`
		} `json:"suggestions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Suggestions) != 1 || result.Suggestions[0].FindingID != match {
		t.Fatalf("inexact suggestions: %s", w.Body.String())
	}
	persisted, err := pg.GetTaskArtifact(a.ID)
	if err != nil || persisted.FindingID != 0 {
		t.Fatal("suggestion assigned evidence automatically")
	}
}
