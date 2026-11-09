package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func hostenvVersionTestSocket(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gnd-hv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

func TestHostenvVersionReadOnlyAndFixedRoute(t *testing.T) {
	sock := hostenvVersionTestSocket(t, "versions.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /versions/nuclei", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		wantQuery := ""
		if calls.Load() == 2 {
			wantQuery = "refresh=1"
		}
		if r.URL.RawQuery != wantQuery {
			t.Errorf("forwarded client source overrides: %s", r.URL)
		}
		writeJSON(w, 200, map[string]any{"tool_id": "nuclei", "latest_version": "v1.2.3", "resolution_id": "fixed-server-plan"})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Setenv("GONDAR_HOSTD_SOCKET", sock)
	s := &Server{}
	r := httptest.NewRequest("GET", "/api/hostenv/versions/nuclei?repo=attacker/evil&url=https://attacker.invalid", nil)
	r.SetPathValue("id", "nuclei")
	w := httptest.NewRecorder()
	s.hostenvVersion(w, r)
	if w.Code != 200 || calls.Load() != 1 || !strings.Contains(w.Body.String(), "fixed-server-plan") {
		t.Fatalf("code=%d body=%s calls=%d", w.Code, w.Body, calls.Load())
	}
	r = httptest.NewRequest("GET", "/api/hostenv/versions/unknown", nil)
	r.SetPathValue("id", "unknown")
	w = httptest.NewRecorder()
	s.hostenvVersion(w, r)
	if w.Code != 400 || calls.Load() != 1 {
		t.Fatal("unknown tool reached helper")
	}
	r = httptest.NewRequest("GET", "/api/hostenv/versions/nuclei?refresh=1&repo=evil&url=https://attacker.invalid", nil)
	r.SetPathValue("id", "nuclei")
	w = httptest.NewRecorder()
	s.hostenvVersion(w, r)
	if w.Code != 200 || calls.Load() != 2 {
		t.Fatalf("manual refresh was not forwarded: %d calls=%d", w.Code, calls.Load())
	}
}

func TestHostenvSingleAndBatchForwardOnlyResolutionIDs(t *testing.T) {
	sock := hostenvVersionTestSocket(t, "plans.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["tool_id"] != "nuclei" || payload["resolution_id"] != "fixed-plan" || payload["version"] != "" || payload["url"] != "" || payload["sha256"] != "" {
			t.Errorf("unexpected install payload: %+v", payload)
		}
		accepted.Add(1)
		writeJSON(w, 201, map[string]string{"id": "job-fixed"})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Setenv("GONDAR_HOSTD_SOCKET", sock)
	s := &Server{}
	w := httptest.NewRecorder()
	s.hostenvInstall(w, httptest.NewRequest("POST", "/api/hostenv/install", strings.NewReader(`{"id":"nuclei","resolution_id":"fixed-plan","version":"latest","url":"https://attacker.invalid"}`)))
	if w.Code != 201 {
		t.Fatalf("single: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	s.hostenvInstallBatch(w, httptest.NewRequest("POST", "/api/hostenv/install-batch", strings.NewReader(`{"ids":["nuclei","nuclei"],"resolutions":{"nuclei":"fixed-plan"}}`)))
	if w.Code != 200 || accepted.Load() != 2 {
		t.Fatalf("batch: %d %s calls=%d", w.Code, w.Body, accepted.Load())
	}
}

func TestHostenvManagedRoutesRequireJWT(t *testing.T) {
	s := testKey([]byte("synthetic-hostenv-signing-key-only"))
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/hostenv/versions/nuclei"},
		{"POST", "/api/hostenv/install"},
		{"POST", "/api/hostenv/install-batch"},
	} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"id":"nuclei","resolution_id":"forged"}`)))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s unprotected: %d", tc.method, tc.path, w.Code)
		}
	}
}

func TestHostenvBatchRejectedResultsUseArrays(t *testing.T) {
	t.Setenv("GONDAR_HOSTD_SOCKET", startFakeHostd(t))
	w := httptest.NewRecorder()
	s := &Server{}
	s.hostenvInstallBatch(w, httptest.NewRequest("POST", "/api/hostenv/install-batch", strings.NewReader(`{"ids":["sqlmap"]}`)))
	var result struct {
		Started []json.RawMessage `json:"started"`
		Skipped []json.RawMessage `json:"skipped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || result.Started == nil || len(result.Started) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("invalid array contract: %d %s", w.Code, w.Body)
	}
}

func TestHostenvBatchKeepsAcceptedIDsAfterHelperDisconnect(t *testing.T) {
	sock := hostenvVersionTestSocket(t, "partial.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 201, map[string]string{"id": "already-accepted"})
		_ = ln.Close()
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Setenv("GONDAR_HOSTD_SOCKET", sock)
	w := httptest.NewRecorder()
	s := &Server{}
	s.hostenvInstallBatch(w, httptest.NewRequest("POST", "/api/hostenv/install-batch", strings.NewReader(`{"ids":["nmap","ffuf","nmap","curl"]}`)))
	var result struct {
		Started []struct {
			JobID string `json:"job_id"`
		} `json:"started"`
		Skipped []struct {
			ToolID string `json:"tool_id"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(result.Started) != 1 || result.Started[0].JobID != "already-accepted" || len(result.Skipped) != 2 {
		t.Fatalf("accepted jobs lost: %d %s", w.Code, w.Body)
	}
}
