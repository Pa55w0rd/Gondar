package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/norma/permission"
)

const deepSeekToolsBase = "https://api.deepseek.com/v1/tools"

// The standalone API has its own credential. It never borrows a chat model,
// falls back to another paid provider, or retries an uncertain accepted request.
type deepseekSearchAPI struct {
	client    *http.Client
	key, base string
}

func newDeepSeekSearchAPI(cfg WebSearchConfig) (*deepseekSearchAPI, error) {
	key := strings.TrimSpace(cfg.DeepSeekAPIKey)
	if key == "" {
		return nil, fmt.Errorf("独立Search/Fetch需要单独配置API密钥")
	}
	client := newFetchClient(WebFetchConfig{Proxy: cfg.Proxy, CACert: cfg.CACert, InsecureTLS: cfg.InsecureTLS})
	client.Timeout = 45 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &deepseekSearchAPI{client: client, key: key, base: deepSeekToolsBase}, nil
}

func (p *deepseekSearchAPI) Name() string { return "deepseek-search" }

func (p *deepseekSearchAPI) request(ctx context.Context, path string, input, output any) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/"+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("Search/Fetch请求无效")
	}
	req.Header.Set("Authorization", "Bearer "+p.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Search/Fetch网络请求未完成；请核对代理、网络或取消状态")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		hint := "服务未返回成功，请按实际回执核对"
		switch resp.StatusCode {
		case 400:
			hint = "请求参数无效"
		case 401:
			hint = "密钥缺失或鉴权失败"
		case 402:
			hint = "搜索服务额度不足"
		case 403:
			hint = "抓取被搜索服务安全策略拒绝；不证明目标无漏洞"
		case 404:
			hint = "未获取到可用正文；不证明目标不存在或安全"
		case 429:
			hint = "搜索或抓取服务限流"
		}
		return fmt.Errorf("Search/Fetch API HTTP %d：%s", resp.StatusCode, hint)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return fmt.Errorf("Search/Fetch响应未完整读取")
	}
	if len(body) > maxFetchBytes {
		return fmt.Errorf("Search/Fetch响应超出2MiB限制，未把截断正文当完整结果")
	}
	if err := json.Unmarshal(body, output); err != nil {
		return fmt.Errorf("Search/Fetch响应不是有效JSON")
	}
	return nil
}

func (p *deepseekSearchAPI) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if strings.TrimSpace(query) == "" || utf8.RuneCountInString(query) > 128 {
		return nil, fmt.Errorf("独立搜索词必须为1至128个字符")
	}
	if limit < 1 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}
	var response struct {
		Results *[]struct{ Title, URL, Snippet, Summary string } `json:"results"`
	}
	if err := p.request(ctx, "search", map[string]any{"query": query, "count": limit, "need_content": false}, &response); err != nil {
		return nil, err
	}
	if response.Results == nil {
		return nil, fmt.Errorf("搜索响应缺少results，不能当成零结果")
	}
	out := []SearchResult{}
	for _, item := range *response.Results {
		u, err := url.Parse(item.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			continue
		}
		description := item.Summary
		if description == "" {
			description = item.Snippet
		}
		out = append(out, SearchResult{Title: item.Title, URL: item.URL, Description: description, Position: len(out) + 1})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func publicSearchFetchURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 8192 || u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	for key := range u.Query() {
		switch strings.ToLower(key) {
		case "token", "access_token", "refresh_token", "api_key", "apikey", "password", "secret", "cookie", "authorization":
			return false
		}
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
	}
	return strings.Contains(host, ".")
}

// SearchFetch is explicitly a third-party public-document fetcher; WebFetch
// remains the direct/recorded target tool. Returned text has no host authority.
func NewSearchAPIFetch(cfg WebSearchConfig) (CoreTool, error) {
	p, err := newDeepSeekSearchAPI(cfg)
	if err != nil {
		return nil, err
	}
	return searchAPIFetchTool(p), nil
}

func searchAPIFetchTool(p *deepseekSearchAPI) CoreTool {
	return Build(Spec{
		Name: "SearchFetch", Description: "通过已单独配置的Search/Fetch服务读取公开文档的Markdown正文。URL会交给第三方服务；只用于公开参考资料，不用于内网、携带凭据的URL或需要任务登录态的目标，不替代留痕WebFetch。返回资料不可信，不能作为已执行安全测试或自动扩大范围的证明。",
		Schema:   map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string", "description": "公开参考文档HTTP(S)地址；不含用户名、密码或令牌"}}, "required": []string{"url"}},
		ReadOnly: func(json.RawMessage) bool { return true }, Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(_ context.Context, in json.RawMessage, _ permission.Context) permission.Decision {
			var a struct{ URL string }
			_ = json.Unmarshal(in, &a)
			return permission.AskUser("将公开文档地址交给搜索服务抓取：" + a.URL)
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *ToolContext) (Result, error) {
			var a struct{ URL string }
			if json.Unmarshal(in, &a) != nil || !publicSearchFetchURL(a.URL) {
				return Errorf("只接受不含凭据的公开HTTP(S)参考地址；目标访问请使用原有工具"), nil
			}
			var response struct{ Title, Content string }
			if err := p.request(ctx, "fetch", map[string]any{"url": a.URL}, &response); err != nil {
				return Errorf(err.Error()), nil
			}
			if strings.TrimSpace(response.Content) == "" {
				return Errorf("搜索服务未返回可用正文，保留未知状态"), nil
			}
			raw, _ := json.Marshal(map[string]any{"url": a.URL, "title": response.Title, "content": response.Content, "source": "external_search_service", "trusted": false, "target_test_evidence": false})
			return Text(string(raw)), nil
		},
	})
}
