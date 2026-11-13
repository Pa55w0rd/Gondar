package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gondar/db"
)

func TestFindingStatsFilteredKeepsGlobalOptionsAndLegacyCounts(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	marker := fmt.Sprintf("__filtered_stats_%d__", time.Now().UnixNano())
	defer m.pg.Exec(`DELETE FROM findings WHERE vulnclass=$1`, marker)
	for _, severity := range []string{"high", "low"} {
		if _, err := m.pg.AddFinding(0, 0, marker, "", severity, "", "", "fixture", nil); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{m: m}
	read := func(query string) db.FindingStats {
		t.Helper()
		out := httptest.NewRecorder()
		s.findingStats(out, httptest.NewRequest(http.MethodGet, "/api/exploration/findings/stats?"+query, nil))
		if out.Code != 200 {
			t.Fatalf("status %d: %s", out.Code, out.Body.String())
		}
		var stats db.FindingStats
		if err := json.Unmarshal(out.Body.Bytes(), &stats); err != nil {
			t.Fatal(err)
		}
		return stats
	}
	legacy := read("severity=high") // old API ignores filters unless explicitly requested
	filtered := read("filtered=1&severity=high&vulnclass=" + marker + "&page=999&limit=1")
	if legacy.Filtered != nil || filtered.Total != legacy.Total || filtered.Filtered == nil || filtered.Filtered.Total != 1 || filtered.Filtered.High != 1 || filtered.Filtered.Low != 0 {
		t.Fatalf("global/current filter/pagination meanings mixed: legacy=%+v filtered=%+v", legacy, filtered)
	}
	if len(filtered.VulnClasses) != len(legacy.VulnClasses) {
		t.Fatal("current filter narrowed the global selector options")
	}
}
