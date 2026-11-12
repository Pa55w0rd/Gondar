package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReportRecoverySettingsRoundTrip(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := New(t.Context(), m, t.TempDir(), t.TempDir(), t.TempDir())
	defer s.Close()
	for _, enabled := range []bool{false, true} {
		body := `{"report_recovery_enabled":false}`
		if enabled {
			body = `{"report_recovery_enabled":true}`
		}
		put := httptest.NewRecorder()
		s.putSettings(put, httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body)))
		if put.Code != http.StatusOK {
			t.Fatalf("save failed: %d %s", put.Code, put.Body.String())
		}
		get := httptest.NewRecorder()
		s.getSettings(get, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
		var result struct {
			Enabled bool `json:"report_recovery_enabled"`
		}
		if err := json.Unmarshal(get.Body.Bytes(), &result); err != nil || result.Enabled != enabled {
			t.Fatalf("setting lost: %+v %v", result, err)
		}
	}
}
