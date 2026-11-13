package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"gondar/agent"
	"gondar/db"
)

func TestLLMTestConfigPresenceAndCompleteProfile(t *testing.T) {
	p := &db.LLMProfile{ID: 8241, Format: "openai", Model: "stored-model", BaseURL: "http://stored.invalid/v1", Proxy: "http://proxy.invalid:8080", APIKey: "fixture-stored-secret", Streaming: false, ThinkingType: "enabled", ReasoningEffort: "high", SessionHeaderKey: "X-Session-Fixture", MaxTokens: 10000, MaxTokensField: "max_completion_tokens", ContextWindowK: 320, RatePerSecond: 3, RatePerMinute: 90, MaxConcurrent: 5}
	original := *p
	for _, tc := range []struct {
		name, body string
		check      func(*testing.T, agent.Config)
	}{
		{"profile only", `{"profile_id":8241}`, func(t *testing.T, c agent.Config) {
			if c.Provider() != p.Format || c.Model != p.Model || c.BaseURL != p.BaseURL || c.Proxy != p.Proxy || c.Stream || c.APIKey != p.APIKey || c.SessionHeaderKey != p.SessionHeaderKey || c.MaxTokens != p.MaxTokens || c.MaxTokensField != p.MaxTokensField || c.ThinkingType != p.ThinkingType || c.ReasoningEffort != p.ReasoningEffort || c.ContextWindowK != p.ContextWindowK || c.RatePerSecond != p.RatePerSecond || c.RatePerMinute != p.RatePerMinute || c.MaxConcurrent != p.MaxConcurrent || c.LimitOwnerID != p.ID || c.Retry != (agent.RetryConfig{}) {
				t.Fatal("connection config did not preserve the complete stored profile")
			}
		}},
		{"false and empty edits", `{"profile_id":8241,"streaming":false,"proxy":"","thinking_type":"","reasoning_effort":"","session_header_key":"","max_tokens":0,"rate_per_second":0,"max_concurrent":0}`, func(t *testing.T, c agent.Config) {
			if c.Stream || c.Proxy != "" || c.ThinkingType != "" || c.ReasoningEffort != "" || c.SessionHeaderKey != "" || c.MaxTokens != 0 || c.RatePerSecond != 0 || c.MaxConcurrent != 0 || c.Model != p.Model {
				t.Fatal("explicit false/zero/empty edits were treated as omitted")
			}
		}},
		{"partial override and blank key", `{"profile_id":8241,"model":"edited","api_key":""}`, func(t *testing.T, c agent.Config) {
			if c.Model != "edited" || c.BaseURL != p.BaseURL || c.APIKey != p.APIKey || c.Stream {
				t.Fatal("partial form edit lost inherited profile fields")
			}
		}},
		{"masked key", `{"profile_id":8241,"api_key":"sk-***hint"}`, func(t *testing.T, c agent.Config) {
			if c.APIKey != p.APIKey {
				t.Fatal("masked form value replaced the stored credential")
			}
		}},
		{"explicit temporary endpoint", `{"profile_id":8241,"provider":"anthropic","model":"edited","base_url":"http://edited.invalid/v1/messages","api_key":"new-fixture-secret","streaming":true}`, func(t *testing.T, c agent.Config) {
			if c.Provider() != "anthropic" || c.Model != "edited" || c.BaseURL != "http://edited.invalid" || c.APIKey != "new-fixture-secret" || !c.Stream {
				t.Fatal("explicit unsaved connection fields were not applied")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req llmTestRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatal(err)
			}
			loads := 0
			cfg, code, err := resolveLLMTestConfig(req, func(id int64) (*db.LLMProfile, error) { loads++; return p, nil }, "must-not-use-global")
			if err != nil || code != 200 || loads != 1 {
				t.Fatalf("resolution code=%d err=%v loads=%d", code, err, loads)
			}
			tc.check(t, cfg)
			if !reflect.DeepEqual(*p, original) {
				t.Fatal("connection test mutated a saved profile")
			}
		})
	}
}

func TestLLMTestConfigInvalidProfileCannotFallBack(t *testing.T) {
	for _, tc := range []struct {
		body string
		load func(int64) (*db.LLMProfile, error)
		code int
	}{
		{`{"profile_id":9}`, func(int64) (*db.LLMProfile, error) { return nil, nil }, 404},
		{`{"profile_id":9}`, func(int64) (*db.LLMProfile, error) { return nil, errors.New("private database diagnostic") }, 500},
		{`{"profile_id":0}`, func(int64) (*db.LLMProfile, error) { t.Fatal("invalid ID reached storage"); return nil, nil }, 400},
		{`{"profile_id":9,"model":""}`, func(int64) (*db.LLMProfile, error) { return &db.LLMProfile{ID: 9, APIKey: "stored"}, nil }, 400},
	} {
		var req llmTestRequest
		_ = json.Unmarshal([]byte(tc.body), &req)
		_, code, err := resolveLLMTestConfig(req, tc.load, "must-not-use-global")
		if code != tc.code || err == nil || strings.Contains(err.Error(), "private database") {
			t.Fatalf("invalid profile resolution code=%d err=%v", code, err)
		}
	}
}

func TestLLMTestStoredProfileRealEndpointModelProxyAndHeader(t *testing.T) {
	var calls atomic.Int32
	sessions := make(chan string, 2)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Host != "connection-fixture.invalid" || r.URL.Path != "/v1/chat/completions" || body["model"] != "stored-wire-model" || body["stream"] != false || body["max_completion_tokens"] != float64(8192) || body["reasoning_effort"] != "high" {
			t.Error("connection request reached the wrong endpoint/model/protocol or output field")
		}
		if r.Header.Get("Authorization") != "Bearer fixture-not-a-real-key" {
			t.Error("stored credential was not used")
		}
		session := r.Header.Get("X-Connection-Fixture")
		sessions <- session
		if !strings.HasPrefix(session, "conntest-") {
			t.Error("connection did not use an isolated session header")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer proxy.Close()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	p := &db.LLMProfile{Name: "connection-fixture", Format: "openai", Model: "stored-wire-model", BaseURL: "http://connection-fixture.invalid/v1", Proxy: proxy.URL, APIKey: "fixture-not-a-real-key", Streaming: false, MaxTokensField: "max_completion_tokens", ReasoningEffort: "high", SessionHeaderKey: "X-Connection-Fixture"}
	id, err := m.pg.SaveProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })
	s := newAdmissionTestServer(m, nil)
	previousSession := ""
	for range 2 {
		out := httptest.NewRecorder()
		s.testLLM(out, httptest.NewRequest(http.MethodPost, "/api/llm/test", strings.NewReader(fmt.Sprintf(`{"profile_id":%d}`, id))))
		if out.Code != 200 || !strings.Contains(out.Body.String(), `"ok":true`) || strings.Contains(out.Body.String(), p.APIKey) {
			t.Fatalf("connection response code=%d did not confirm the actual request safely", out.Code)
		}
		session := <-sessions
		if session == "" || session == previousSession {
			t.Fatal("connection tests shared a session identity")
		}
		previousSession = session
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected connection attempts: %d", calls.Load())
	}
	var editedCalls atomic.Int32
	edited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		editedCalls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/v1/chat/completions" || body["model"] != "edited-wire-model" || r.Header.Get("Authorization") != "Bearer edited-fixture-key" || r.Header.Get("X-Connection-Fixture") != "" {
			t.Error("unsaved endpoint/model/key/header edits did not reach the actual request")
		}
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
	}))
	defer edited.Close()
	editBody, _ := json.Marshal(map[string]any{"profile_id": id, "model": "edited-wire-model", "base_url": edited.URL + "/v1", "proxy": "", "api_key": "edited-fixture-key", "session_header_key": "", "streaming": false})
	editedResponse := httptest.NewRecorder()
	s.testLLM(editedResponse, httptest.NewRequest(http.MethodPost, "/api/llm/test", strings.NewReader(string(editBody))))
	if editedResponse.Code != 200 || !strings.Contains(editedResponse.Body.String(), `"ok":true`) || editedCalls.Load() != 1 || calls.Load() != 2 {
		t.Fatal("unsaved profile override tested the stored endpoint instead")
	}
	stored, err := m.pg.ProfileByID(id)
	if err != nil || stored.Model != p.Model || stored.APIKey != p.APIKey || stored.Proxy != p.Proxy {
		t.Fatal("temporary connection form persisted profile edits")
	}
	unknown := httptest.NewRecorder()
	s.testLLM(unknown, httptest.NewRequest(http.MethodPost, "/api/llm/test", strings.NewReader(`{"profile_id":9223372036854775806}`)))
	if unknown.Code != 404 || calls.Load() != 2 {
		t.Fatal("unknown stored profile made a fallback outbound request")
	}
}

func TestLLMTestDiagnosticHidesCredential(t *testing.T) {
	cfg := agent.Config{APIKey: "fixture-hidden", Proxy: "http://person:fixture-pass@proxy.invalid:8080"}
	got := llmTestDiagnostic("failed fixture-hidden person:fixture-pass", cfg)
	if strings.Contains(got, "fixture-hidden") || strings.Contains(got, "fixture-pass") {
		t.Fatal("connection diagnostic exposed a credential")
	}
}
