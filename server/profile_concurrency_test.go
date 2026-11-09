package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"gondar/agent"
	"gondar/db"
)

func TestProfileConcurrencyAPICompatibility(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	s := newAdmissionTestServer(m, nil)
	id, err := m.pg.SaveProfile(&db.LLMProfile{Name: "runtime-cap-api", Format: "openai", Model: "synthetic", APIKey: "local-test-only", MaxConcurrent: 7})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	for _, tc := range []struct {
		extra      string
		code, want int
	}{
		{"", 200, 7}, {`,"max_concurrent":0`, 200, 0}, {`,"max_concurrent":12`, 200, 12},
		{`,"max_concurrent":-1`, 400, 12}, {`,"max_concurrent":1025`, 400, 12},
	} {
		body := fmt.Sprintf(`{"id":%d,"name":"runtime-cap-api","format":"openai","model":"synthetic"%s}`, id, tc.extra)
		r := httptest.NewRequest("POST", "/api/llm/profiles", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.pgSaveProfile(w, r)
		if w.Code != tc.code {
			t.Fatalf("response %d: %s", w.Code, w.Body.String())
		}
		p, err := m.pg.ProfileByID(id)
		if err != nil || p == nil || p.MaxConcurrent != tc.want {
			t.Fatalf("stored cap=%+v err=%v want %d", p, err, tc.want)
		}
		cfg, ok := s.loadProfileConfig(id)
		if !ok || cfg.MaxConcurrent != tc.want || cfg.LimitOwnerID != id {
			t.Fatalf("loaded config cap=%d owner=%d ok=%v", cfg.MaxConcurrent, cfg.LimitOwnerID, ok)
		}
		raw, _ := json.Marshal(llmProfileDTO(p))
		var dto map[string]any
		if err := json.Unmarshal(raw, &dto); err != nil {
			t.Fatal(err)
		}
		if dto["max_concurrent"] != float64(tc.want) {
			t.Fatalf("DTO missing cap: %s", raw)
		}
		if strings.Contains(string(raw), "local-test-only") {
			t.Fatal("DTO exposed API credential")
		}
	}
}

func TestLegacyLLMSaveKeepsConcurrencyAndOwner(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	oldActive, err := m.pg.ActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := m.pg.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		if p.Name == "default" {
			t.Skip("legacy fixture requires no pre-existing default profile")
		}
	}
	id, err := m.pg.SaveProfile(&db.LLMProfile{Name: "default", Format: "openai", Model: "synthetic", APIKey: "synthetic-only", MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, id)
		if oldActive != nil {
			m.pg.SetActiveProfile(oldActive.ID)
		}
		m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id)
	})
	s := newAdmissionTestServer(m, nil)
	// The legacy payload has no concurrency field, and must preserve the stored
	// cap while producing the same profile owner identity as normal loads.
	if err := s.saveLLMConfig(agent.ConfigFrom("openai", "new-synthetic", "", "synthetic-only", "")); err != nil {
		t.Fatal(err)
	}
	cfg, ok := s.loadLLMConfig()
	if !ok || cfg.MaxConcurrent != 4 || cfg.LimitOwnerID != id {
		t.Fatalf("legacy cap=%d owner=%d ok=%v", cfg.MaxConcurrent, cfg.LimitOwnerID, ok)
	}
}
