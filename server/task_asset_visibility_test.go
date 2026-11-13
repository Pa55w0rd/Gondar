package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"gondar/agent"
	"gondar/db"
)

func visibilityServerFixture(t *testing.T) *Server {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("资料来源集成回归需要专用 PostgreSQL")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	m := &Manager{dir: t.TempDir(), pg: pg, assets: pg.Assets(), tasks: map[string]*Task{}}
	return &Server{m: m, engine: NewEngine(m)}
}

func TestTaskSourcesInputSeedsRemainVisibleWithCoverageDisabled(t *testing.T) {
	s := visibilityServerFixture(t)
	for _, target := range []string{"https://seed.fixture.invalid/path", "http://198.51.100.87:8080"} {
		for _, coverage := range []bool{true, false} {
			t.Run(fmt.Sprintf("target=%s/coverage=%t", target, coverage), func(t *testing.T) {
				task, err := s.m.CreateTaskWithOptions(target, "仅合成输入种子，不发目标请求", db.TaskCreateOptions{AssetContextMode: db.AssetContextTaskSources, CoverageEnabled: &coverage})
				if err != nil {
					t.Fatal(err)
				}
				id, _ := strconv.ParseInt(task.ID, 10, 64)
				t.Cleanup(func() { s.m.pg.DeleteTaskCascade(id, true, true) })
				s.seed(task, target)
				scopes, err := s.m.assets.ListTaskScope(id)
				if err != nil || len(scopes) != 1 || scopes[0].Source != "manual" {
					t.Fatalf("input seed scope=%+v err=%v", scopes, err)
				}
				if scopes[0].Kind == "subdomain" && scopes[0].Domain != "seed.fixture.invalid" {
					t.Fatalf("domain seed expanded: %+v", scopes[0])
				}
				if scopes[0].Kind == "ip" && scopes[0].Net != "198.51.100.87/32" {
					t.Fatalf("IP seed expanded: %+v", scopes[0])
				}
				assets, err := s.m.assets.QueryDSLInScope("", "", id, 100, 0)
				if err != nil || len(assets) == 0 {
					t.Fatalf("input seed hidden by source mode: %+v err=%v", assets, err)
				}
				for _, asset := range assets {
					if asset.Type == "service" && asset.Domain != "seed.fixture.invalid" {
						t.Fatalf("unexpected source material: %+v", asset)
					}
				}
			})
		}
	}
}

func TestTaskAssetContextPatchPreservesPauseAndAPIIntersection(t *testing.T) {
	s := visibilityServerFixture(t)
	task, err := s.m.CreateTaskWithOptions("https://mode-api.fixture.invalid", "仅合成资料，不运行任务", db.TaskCreateOptions{AssetContextMode: db.AssetContextTaskSources})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	defer s.m.pg.DeleteTaskCascade(id, true, true)
	s.seed(task, task.Description)
	rows, err := s.m.assets.QueryDSLInScope("", "service", id, 100, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("seed service=%+v err=%v", rows, err)
	}
	visible := rows[0]
	var hidden int64
	if err := s.m.pg.QueryRow(`INSERT INTO assets(type,url,domain,root_domain) VALUES('service',$1,$2,$3) RETURNING id`, visible.URL+"/hidden", visible.Domain, visible.RootDomain).Scan(&hidden); err != nil {
		t.Fatal(err)
	}
	defer s.m.assets.DeleteByIDs([]int64{hidden})
	company, _, err := s.m.pg.Companies().UpsertCompany("mode-api-fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.m.pg.Companies().DeleteCompany(company)
	if _, err := s.m.pg.Exec(`UPDATE assets SET company_id=$1 WHERE id=ANY($2)`, company, []int64{visible.ID, hidden}); err != nil {
		t.Fatal(err)
	}
	query := url.Values{"task_id": {task.ID}, "type": {"service"}, "company_id": {fmt.Sprint(company)}, "dsl": {fmt.Sprintf(`url="%s" OR url="%s"`, visible.URL, visible.URL+"/hidden")}}
	w := httptest.NewRecorder()
	s.listAssets(w, httptest.NewRequest(http.MethodGet, "/api/assets?"+query.Encode(), nil))
	var page struct {
		Total  int        `json:"total"`
		Assets []db.Asset `json:"assets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != http.StatusOK || page.Total != 1 || len(page.Assets) != 1 || page.Assets[0].ID != visible.ID {
		t.Fatalf("HTTP candidate intersection failed: status=%d body=%s err=%v", w.Code, w.Body.String(), err)
	}
	if err := s.m.pg.SetPaused(id, true); err != nil {
		t.Fatal(err)
	}
	task.updateLifecycle(func(state *taskLifecycleState) { state.Paused = true })
	s.engine.Pause(task.ID, agent.AbortPausedByUser)
	path := filepath.Join(s.m.dir, "tasks", task.ID, "retained-evidence.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("retained attachment"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPatch, "/api/tasks/"+task.ID, bytes.NewBufferString(body))
		r.SetPathValue("id", task.ID)
		w := httptest.NewRecorder()
		s.updateTaskMetadata(w, r)
		return w
	}
	for _, mode := range []string{db.AssetContextDeclaredScope, db.AssetContextTaskSources} {
		w := patch(`{"asset_context_mode":"` + mode + `"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("patch mode=%s status=%d body=%s", mode, w.Code, w.Body.String())
		}
		if !s.engine.IsPaused(task.ID) || !task.lifecycleSnapshot().Paused || task.lifecycleSnapshot().AssetContextMode != mode {
			t.Fatalf("source selection changed execution/pause: %+v", task.lifecycleSnapshot())
		}
		persisted, err := s.m.pg.GetTask(id)
		if err != nil || !persisted.Paused || persisted.AssetContextMode != mode {
			t.Fatalf("persisted state=%+v err=%v", persisted, err)
		}
	}
	if got := patch(`{"name":"renamed"}`); got.Code != http.StatusOK || task.lifecycleSnapshot().AssetContextMode != db.AssetContextTaskSources {
		t.Fatalf("omitted mode overwrote selection: %d %s", got.Code, got.Body.String())
	}
	if got := patch(`{"asset_context_mode":"global"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode status=%d body=%s", got.Code, got.Body.String())
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "retained attachment" {
		t.Fatalf("mode change touched existing attachment: %q err=%v", content, err)
	}
}
