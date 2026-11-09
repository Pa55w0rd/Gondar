package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
)

func TestConfigFromSanitizesPastedValues(t *testing.T) {
	for _, tc := range []struct {
		provider, suffix string
		format           llm.Format
	}{
		{"openai", "/chat/completions", llm.FormatOpenAI},
		{"openai-responses", "/responses", llm.FormatOpenAIResponses},
		{"anthropic", "/v1/messages", llm.FormatAnthropic},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			cfg := ConfigFrom(" \ufeff"+tc.provider+"\u200b ", " \u2060模型-α\u200c ",
				" \ufeffhttps://gateway.example/v1"+tc.suffix+"\u200b/ ",
				" \ufefftest-\u200bkey\u2060 ", " \ufeffsocks5://127.0.0.1:1080\u200b ")
			if cfg.Format != tc.format || cfg.Provider() != tc.provider {
				t.Fatalf("format=%q provider=%q", cfg.Format, cfg.Provider())
			}
			if cfg.BaseURL != "https://gateway.example/v1" || cfg.Model != "模型-α" || cfg.APIKey != "test-key" || cfg.Proxy != "socks5://127.0.0.1:1080" {
				t.Fatalf("pasted configuration was not normalized: %+v", cfg)
			}
		})
	}
	if got := CleanConfigValue(" \ufeff模型 A-α/乙\u200b "); got != "模型 A-α/乙" {
		t.Fatalf("visible Unicode and internal spaces must be preserved: %q", got)
	}
}

func TestFromEnvSanitizesBeforeProviderInference(t *testing.T) {
	t.Setenv("GONDAR_LLM_PROVIDER", "\ufeff\u200b")
	t.Setenv("ANTHROPIC_API_KEY", " \u200b ")
	t.Setenv("OPENAI_API_KEY", " \ufefftest-key\u2060 ")
	t.Setenv("GONDAR_LLM_MODEL", " \u200b模型-α ")
	t.Setenv("GONDAR_LLM_BASE_URL", " \ufeffhttps://gateway.example/v1\u200b ")
	t.Setenv("GONDAR_LLM_PROXY", " \u2060http://127.0.0.1:1080 ")
	t.Setenv("GONDAR_LLM_STREAM", "false")
	cfg, ok := FromEnv()
	if !ok || cfg.Format != llm.FormatOpenAI || cfg.APIKey != "test-key" || cfg.Model != "模型-α" || cfg.BaseURL != "https://gateway.example/v1" || cfg.Proxy != "http://127.0.0.1:1080" || cfg.Stream {
		t.Fatalf("environment inference did not use normalized values: ok=%v cfg=%+v", ok, cfg)
	}
	t.Setenv("OPENAI_API_KEY", "\ufeff\u200b")
	if _, ok := FromEnv(); ok {
		t.Fatal("invisible-only credentials must not configure a provider")
	}
}

func TestDirectProviderConfigSanitizesActualRequest(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("request path or key mismatch: %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "模型-α" {
			t.Errorf("request model=%q decode=%v", body.Model, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fixture","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()
	provider, err := (Config{Format: llm.Format(" \ufeffopenai\u200b "), BaseURL: " \ufeff" + upstream.URL + "/v1\u200b ", APIKey: " \ufefftest-key\u2060 ", Model: " \u200b模型-α ", Proxy: "\ufeff\u200b"}).NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, _, _, err := provider.Complete(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("fixture")}})
	if err != nil || msg.Text() != "ok" || requests.Load() != 1 {
		t.Fatalf("completion=%q requests=%d err=%v", msg.Text(), requests.Load(), err)
	}
}
