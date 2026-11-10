package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gondar/db"
)

func TestBuiltinTaskScenesHTTPListProtectionAndCopy(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = m.Close() })
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())
	token, err := signJWT(s.currentJWTKey())
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	w := do("GET", "/api/task-templates", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list %d: %s", w.Code, w.Body.String())
	}
	var list struct {
		Templates []db.TaskTemplate `json:"templates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var ctf db.TaskTemplate
	count := 0
	for _, row := range list.Templates {
		if row.BuiltinKey == "" {
			continue
		}
		count++
		if row.BuiltinKey == "source_audit" {
			t.Fatal("retired source-audit template remains available")
		}
		if row.Scene == nil || row.Scene.TimeoutSeconds <= 0 {
			t.Fatalf("missing real defaults: %+v", row)
		}
		if row.BuiltinKey == "ctf" {
			ctf = row
		}
	}
	if count != 7 || ctf.ID == 0 || !ctf.Scene.SeedFirstIntent || ctf.Scene.CoverageEnabled {
		t.Fatalf("scene defaults incorrect: count=%d CTF=%+v", count, ctf)
	}
	for _, method := range []string{"PATCH", "DELETE"} {
		w = do(method, fmt.Sprintf("/api/task-templates/%d", ctf.ID), map[string]any{"goal": "overwrite"})
		if w.Code != http.StatusForbidden {
			t.Fatalf("builtin %s status=%d body=%s", method, w.Code, w.Body.String())
		}
	}
	w = do("POST", "/api/task-templates", map[string]any{
		"name": fmt.Sprintf("场景自定义-%d", time.Now().UnixNano()), "description": ctf.Description, "goal": ctf.Goal,
		"builtin_key": "ctf", // read-only metadata from clients must not be persisted
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("copy %d: %s", w.Code, w.Body.String())
	}
	var copy db.TaskTemplate
	if err := json.Unmarshal(w.Body.Bytes(), &copy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.DeleteTaskTemplate(copy.ID) })
	if copy.BuiltinKey != "" || copy.Scene != nil {
		t.Fatalf("client forged builtin identity: %+v", copy)
	}
	w = do("PATCH", fmt.Sprintf("/api/task-templates/%d", copy.ID), map[string]any{"goal": "custom goal"})
	if w.Code != http.StatusOK {
		t.Fatalf("custom copy is not editable: %d %s", w.Code, w.Body.String())
	}
}
