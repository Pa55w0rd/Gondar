package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"gondar/db"
)

func TestListModelsSanitizesPastedValues(t *testing.T) {
	for _, tc := range []struct {
		provider, suffix, path, header, key string
	}{
		{"openai", "/v1/chat/completions", "/v1/models", "Authorization", "Bearer test-key"},
		{"openai-responses", "/v1/responses", "/v1/models", "Authorization", "Bearer test-key"},
		{"anthropic", "/v1/messages", "/v1/models", "x-api-key", "test-key"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path || r.Header.Get(tc.header) != tc.key {
					t.Errorf("model list request path=%q header=%q", r.URL.Path, r.Header.Get(tc.header))
				}
				_, _ = w.Write([]byte(`{"data":[{"id":"fixture-model"}]}`))
			}))
			defer upstream.Close()
			body, _ := json.Marshal(map[string]string{"provider": " \ufeff" + tc.provider + "\u200b ", "base_url": " \ufeff" + upstream.URL + tc.suffix + "\u2060/ ", "api_key": " \u200btest-key\ufeff ", "proxy": "\ufeff\u200b"})
			out := httptest.NewRecorder()
			(&Server{}).pgListModels(out, httptest.NewRequest(http.MethodPost, "/api/llm/models", bytes.NewReader(body)))
			if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), `"models":["fixture-model"]`) {
				t.Fatalf("model list response=%d %s", out.Code, out.Body.String())
			}
		})
	}
}

func TestListModelsSanitizesProxy(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Host != "127.0.0.1:1" || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected proxied request: %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"proxied-model"}]}`))
	}))
	defer proxy.Close()
	body, _ := json.Marshal(map[string]string{"provider": "openai", "base_url": "http://127.0.0.1:1/v1", "api_key": "test-key", "proxy": " \ufeff" + proxy.URL + "\u200b "})
	out := httptest.NewRecorder()
	(&Server{}).pgListModels(out, httptest.NewRequest(http.MethodPost, "/api/llm/models", bytes.NewReader(body)))
	if calls.Load() != 1 || !strings.Contains(out.Body.String(), `"models":["proxied-model"]`) {
		t.Fatalf("proxy calls=%d response=%s", calls.Load(), out.Body.String())
	}
}

func TestListModelsRejectsInvisibleOnlyKey(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	body, _ := json.Marshal(map[string]string{"provider": "openai", "base_url": upstream.URL, "api_key": " \ufeff\u200b "})
	out := httptest.NewRecorder()
	(&Server{}).pgListModels(out, httptest.NewRequest(http.MethodPost, "/api/llm/models", bytes.NewReader(body)))
	if calls.Load() != 0 || !strings.Contains(out.Body.String(), `"ok":false`) {
		t.Fatalf("invisible key must not send requests: calls=%d response=%s", calls.Load(), out.Body.String())
	}
}

func TestListModelsSanitizesStoredKeyFallback(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("isolated postgres unavailable: %v", err)
	}
	defer m.Close()
	profileID, err := m.pg.SaveProfile(&db.LLMProfile{Name: "sanitize-" + uuid.NewString(), Format: "openai", Model: "fixture", APIKey: " \ufeffsaved-\u200bkey "})
	if err != nil {
		t.Fatal(err)
	}
	defer m.pg.DeleteProfile(profileID)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer saved-key" {
			t.Errorf("stored key was not cleaned: %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"stored-key-model"}]}`))
	}))
	defer upstream.Close()
	body, _ := json.Marshal(map[string]any{"provider": "openai", "base_url": upstream.URL, "api_key": "\ufeff\u200b", "profile_id": profileID})
	out := httptest.NewRecorder()
	(&Server{m: m}).pgListModels(out, httptest.NewRequest(http.MethodPost, "/api/llm/models", bytes.NewReader(body)))
	if !strings.Contains(out.Body.String(), `"models":["stored-key-model"]`) {
		t.Fatalf("stored-key fallback response=%s", out.Body.String())
	}
}
