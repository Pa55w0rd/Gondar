package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSearchAPIKeyIndependentAndWriteOnly(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	if err := m.SetWebSearch(true, "deepseek-search", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if m.WebSearchOpts().Enabled {
		t.Fatal("missing independent key did not disable search")
	}
	if err := m.pg.SetSetting(settingDeepSeekSearchKey, "fixture-independent-search-key"); err != nil {
		t.Fatal(err)
	}
	opts := m.WebSearchOpts()
	if !opts.Enabled || opts.DeepSeekAPIKey != "fixture-independent-search-key" || opts.DeepSeekModel != "" || opts.DeepSeekBaseURL != "" {
		t.Fatalf("search coupled to chat: %+v", opts)
	}
	s := newAdmissionTestServer(m, nil)
	body, err := json.Marshal(s.settingsPayload())
	if err != nil || !strings.Contains(string(body), `"deepseek_search_key_set":true`) || strings.Contains(string(body), "fixture-independent-search-key") {
		t.Fatal("settings exposed secret or lost configured status")
	}
	if err := m.pg.SetSetting(settingDeepSeekSearchKey, ""); err != nil {
		t.Fatal(err)
	}
	if m.WebSearchOpts().Enabled || s.searchAPIKeyConfigured() {
		t.Fatal("cleared key not effective")
	}
}
