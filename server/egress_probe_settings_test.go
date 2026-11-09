package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gondar/traffic"
)

func egressSettingsServer(t *testing.T) (*Server, func(string, string, string, bool) *httptest.ResponseRecorder) {
	t.Helper()
	dir := t.TempDir()
	m, err := NewManager(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	for _, key := range []string{settingEgressProbeURL, settingProxyCheckTarget, settingProxyPoolEntries, settingProxyPoolStrategy} {
		old, existed, err := m.pg.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if existed {
				if err := m.pg.SetSetting(key, old); err != nil {
					t.Error(err)
				}
			} else if _, err := m.pg.Exec("DELETE FROM settings WHERE key=$1", key); err != nil {
				t.Error(err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, m, dir, dir, dir)
	t.Cleanup(func() { cancel(); s.Close() })
	token, err := signJWT(s.currentJWTKey())
	if err != nil {
		t.Fatal(err)
	}
	return s, func(method, path, body string, authenticated bool) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		if authenticated {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, request)
		return response
	}
}

func TestEgressProbeSettingPersistsAndClears(t *testing.T) {
	s, request := egressSettingsServer(t)
	for _, path := range []string{"/api/settings", "/api/settings/egress-probe/test"} {
		method := http.MethodPut
		if strings.HasSuffix(path, "/test") {
			method = http.MethodPost
		}
		if response := request(method, path, `{}`, false); response.Code != http.StatusUnauthorized {
			t.Fatal("probe settings must require authentication", response.Code)
		}
	}
	assert := func(body, want string) {
		t.Helper()
		response := request(http.MethodPut, "/api/settings", body, true)
		if response.Code != http.StatusOK {
			t.Fatal(response.Code, response.Body)
		}
		var values struct {
			URL string `json:"egress_probe_url"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &values); err != nil || values.URL != want {
			t.Fatal(values.URL, want, err)
		}
		// A new Manager instance reads settings, without a cached URL field.
		reloaded := &Manager{pg: s.m.pg}
		if reloaded.EgressProbeURL() != want {
			t.Fatal("persisted probe URL changed after a fresh reader")
		}
	}
	const custom = "http://10.0.0.5/ip"
	assert(`{"egress_probe_url":"  `+custom+`  "}`, custom)
	assert(`{}`, custom)
	assert(`{"egress_probe_url":null}`, custom)
	for _, invalid := range []string{"ftp://echo.invalid/ip", "http://user:DO_NOT_ECHO@echo.invalid/ip", "http://127.0.0.1/ip"} {
		body, _ := json.Marshal(map[string]string{settingEgressProbeURL: invalid})
		response := request(http.MethodPut, "/api/settings", string(body), true)
		if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "DO_NOT_ECHO") || s.m.EgressProbeURL() != custom {
			t.Fatal("invalid setting leaked credentials or changed the saved value", response.Code)
		}
	}
	assert(`{"egress_probe_url":""}`, "")
	if target, err := s.m.proxyCheckTarget(); err != nil || target != defaultProxyCheckTarget {
		t.Fatal("clear must restore previous default", target, err)
	}
}

func TestConfiguredEgressUsedByManualAndHealthProbe(t *testing.T) {
	s, request := egressSettingsServer(t)
	var mu sync.Mutex
	var destinations []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		destinations = append(destinations, r.URL.Host+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, `{"ip":"203.0.113.7"}`)
	}))
	defer proxy.Close()
	const custom = "http://echo.example.invalid/ip"
	if response := request(http.MethodPut, "/api/settings", `{"egress_probe_url":"`+custom+`"}`, true); response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body)
	}
	entry := traffic.PoolEntry{URL: proxy.URL, Enabled: true, Weight: 1}
	if err := s.saveProxyPool(proxyPoolState{Entries: []traffic.PoolEntry{entry}, Strategy: "round_robin"}); err != nil {
		t.Fatal(err)
	}
	assertResult := func(result []byte) {
		t.Helper()
		var value struct {
			OK bool   `json:"ok"`
			IP string `json:"exit_ip"`
		}
		if err := json.Unmarshal(result, &value); err != nil || !value.OK || value.IP != "203.0.113.7" {
			t.Fatal(string(result), err)
		}
	}
	body, _ := json.Marshal(map[string]string{"proxy": proxy.URL})
	response := request(http.MethodPost, "/api/settings/egress-probe/test", string(body), true)
	assertResult(response.Body.Bytes())
	// The host-environment helper uses only the custom service too.
	hostResult := s.probeConfiguredProxy(context.Background(), proxy.URL)
	if !hostResult.OK || hostResult.ExitIP != "203.0.113.7" {
		t.Fatal(hostResult)
	}
	// Explicit stale public targets cannot override an active unified setting.
	response = request(http.MethodPost, "/api/proxypool/check", `{"target":"https://api.ipify.org?format=json"}`, true)
	var pool struct {
		Target  string            `json:"target"`
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &pool); err != nil || pool.Target != custom || len(pool.Results) != 1 {
		t.Fatal(response.Body, err)
	}
	assertResult(pool.Results[0])
	// Background health checks use this same shared batch-probe path.
	results := s_probeAll(context.Background(), s.m, []traffic.PoolEntry{entry}, defaultProxyCheckTarget)
	if len(results) != 1 || !results[0].OK || results[0].ExitIP != "203.0.113.7" {
		t.Fatal(results)
	}
	if err := s.m.pg.SetSetting(settingEgressProbeURL, "http://echo.example.invalid/fail"); err != nil {
		t.Fatal(err)
	}
	failed := s.m.checkConfiguredPoolProxy(context.Background(), 0, proxy.URL, proxy.URL, defaultProxyCheckTarget, time.Second)
	if failed.OK || failed.Error == "" {
		t.Fatal("custom failure must remain a failure", failed)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(destinations) != 5 {
		t.Fatal("unexpected fallback or missing call", destinations)
	}
	for _, target := range destinations {
		if !strings.HasPrefix(target, "echo.example.invalid/") {
			t.Fatal("custom probe contacted another destination", target)
		}
	}
}
