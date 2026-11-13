package server

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"gondar/agent"
	"gondar/db"
)

// Pointers distinguish an omitted field from a deliberate false/zero/empty edit.
// A blank API key keeps the stored key, matching the existing profile form.
type llmTestRequest struct {
	ProfileID        *int64   `json:"profile_id"`
	Provider         *string  `json:"provider"`
	Format           *string  `json:"format"`
	Model            *string  `json:"model"`
	BaseURL          *string  `json:"base_url"`
	Proxy            *string  `json:"proxy"`
	APIKey           *string  `json:"api_key"`
	Streaming        *bool    `json:"streaming"`
	ThinkingType     *string  `json:"thinking_type"`
	ReasoningEffort  *string  `json:"reasoning_effort"`
	SessionHeaderKey *string  `json:"session_header_key"`
	MaxTokens        *int     `json:"max_tokens"`
	MaxTokensField   *string  `json:"max_tokens_field"`
	ContextWindowK   *int     `json:"context_window_k"`
	RatePerSecond    *float64 `json:"rate_per_second"`
	RatePerMinute    *float64 `json:"rate_per_minute"`
	MaxConcurrent    *int     `json:"max_concurrent"`
}

func resolveLLMTestConfig(req llmTestRequest, load func(int64) (*db.LLMProfile, error), fallbackKey string) (agent.Config, int, error) {
	p := db.LLMProfile{Streaming: true}
	if req.ProfileID != nil {
		if *req.ProfileID <= 0 {
			return agent.Config{}, http.StatusBadRequest, errors.New("profile_id 必须为正整数")
		}
		stored, err := load(*req.ProfileID)
		if err != nil {
			return agent.Config{}, http.StatusInternalServerError, errors.New("读取模型配置失败")
		}
		if stored == nil {
			return agent.Config{}, http.StatusNotFound, errors.New("指定的模型配置不存在")
		}
		p = *stored // one coherent snapshot; never mutate the saved profile
	} else {
		p.APIKey = fallbackKey
	}
	if req.Provider != nil && req.Format != nil && !strings.EqualFold(agent.CleanConfigValue(*req.Provider), agent.CleanConfigValue(*req.Format)) {
		return agent.Config{}, http.StatusBadRequest, errors.New("provider 与 format 不一致")
	}
	for _, pair := range []struct{ dst, src *string }{
		{&p.Format, req.Format}, {&p.Format, req.Provider}, {&p.Model, req.Model},
		{&p.BaseURL, req.BaseURL}, {&p.Proxy, req.Proxy}, {&p.ThinkingType, req.ThinkingType},
		{&p.ReasoningEffort, req.ReasoningEffort}, {&p.SessionHeaderKey, req.SessionHeaderKey},
		{&p.MaxTokensField, req.MaxTokensField},
	} {
		if pair.src != nil {
			*pair.dst = agent.CleanConfigValue(*pair.src)
		}
	}
	if (req.Model != nil && p.Model == "") || ((req.Provider != nil || req.Format != nil) && p.Format == "") {
		return agent.Config{}, http.StatusBadRequest, errors.New("模型和接口格式不能显式设为空")
	}
	p.Format = strings.ToLower(agent.CleanConfigValue(p.Format))
	if p.Format != "" && p.Format != "anthropic" && p.Format != "openai" && p.Format != "openai-responses" {
		return agent.Config{}, http.StatusBadRequest, errors.New("模型接口格式无效")
	}
	if req.APIKey != nil {
		key := agent.CleanConfigValue(*req.APIKey)
		if key != "" && !strings.Contains(key, "***") && !strings.Contains(key, "•••") {
			p.APIKey = key
		}
	}
	if req.Streaming != nil {
		p.Streaming = *req.Streaming
	}
	for _, pair := range []struct{ dst, src *int }{
		{&p.MaxTokens, req.MaxTokens}, {&p.ContextWindowK, req.ContextWindowK}, {&p.MaxConcurrent, req.MaxConcurrent},
	} {
		if pair.src != nil {
			*pair.dst = *pair.src
		}
	}
	if req.RatePerSecond != nil {
		p.RatePerSecond = *req.RatePerSecond
	}
	if req.RatePerMinute != nil {
		p.RatePerMinute = *req.RatePerMinute
	}
	if p.MaxTokens < 0 || p.ContextWindowK < 0 || p.MaxConcurrent < 0 || p.MaxConcurrent > 1024 || p.RatePerSecond < 0 || p.RatePerMinute < 0 {
		return agent.Config{}, http.StatusBadRequest, errors.New("模型限制配置无效")
	}
	cfg := agent.ConfigFrom(p.Format, p.Model, p.BaseURL, p.APIKey, p.Proxy)
	cfg.Stream, cfg.ThinkingType, cfg.ReasoningEffort = p.Streaming, p.ThinkingType, p.ReasoningEffort
	cfg.MaxTokens, cfg.MaxTokensField = p.MaxTokens, p.MaxTokensField
	cfg.SessionHeaderKey, cfg.ContextWindowK = p.SessionHeaderKey, p.ContextWindowK
	cfg.RatePerSecond, cfg.RatePerMinute = p.RatePerSecond, p.RatePerMinute
	cfg.MaxConcurrent, cfg.LimitOwnerID = p.MaxConcurrent, p.ID
	// Connection tests retain their short deadline and no profile retry chain.
	if cfg.APIKey == "" {
		return cfg, http.StatusOK, errors.New("未提供 API Key")
	}
	return cfg, http.StatusOK, nil
}

func llmTestDiagnostic(text string, cfg agent.Config) string {
	if cfg.APIKey != "" {
		text = strings.ReplaceAll(text, cfg.APIKey, "[已隐藏密钥]")
	}
	for _, raw := range []string{cfg.Proxy, cfg.BaseURL} {
		if u, err := url.Parse(raw); err == nil && u.User != nil {
			text = strings.ReplaceAll(text, u.User.String(), "[已隐藏凭据]")
		}
	}
	return text
}
