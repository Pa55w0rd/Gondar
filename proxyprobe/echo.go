package proxyprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const maxEchoBytes = 4096

// ValidateEchoURL allows an operator-selected intranet or public IP echo service.
// Credentials and fragments are rejected; validation errors never echo the input.
func ValidateEchoURL(raw string) (string, error) {
	if strings.ContainsAny(raw, "\r\n\t") {
		return "", errors.New("IP 探测地址不能包含控制字符")
	}
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\t ") {
		return "", errors.New("IP 探测地址需为不超过 2048 字节的 HTTP(S) URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Hostname() == "" || u.Opaque != "" {
		return "", errors.New("IP 探测地址不是合法的绝对 URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("IP 探测地址只支持 HTTP / HTTPS")
	}
	if u.User != nil || u.Fragment != "" || u.RawFragment != "" {
		return "", errors.New("IP 探测地址不能包含用户凭据或片段")
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", errors.New("IP 探测地址端口无效")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("IP 探测地址端口无效")
		}
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if strings.Contains(host, "%") || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", errors.New("IP 探测地址不能指向本机或链路本地地址")
	}
	if ip := net.ParseIP(host); ip != nil {
		if blockedEchoIP(ip) {
			return "", errors.New("IP 探测地址不能指向本机或链路本地地址")
		}
	} else if legacyNumericHost(host) {
		return "", errors.New("IP 探测地址须使用标准 IP 或域名")
	}
	return u.String(), nil
}

func legacyNumericHost(host string) bool {
	if strings.HasPrefix(host, "0x") {
		return true
	}
	for _, ch := range host {
		if ch != '.' && (ch < '0' || ch > '9') {
			return false
		}
	}
	return true
}

func blockedEchoIP(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast()
}

// Direct echo probes also check the resolved address, preventing DNS rebinding
// to a loopback or metadata endpoint. RFC1918 services remain supported.
func echoDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil || blockedEchoIP(net.ParseIP(host)) {
		return errors.New("拒绝访问本机或链路本地探测地址")
	}
	return nil
}

func echoPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

func echoRedirect(next *http.Request, via []*http.Request) error {
	if len(via) >= 3 || len(via) == 0 {
		return errors.New("IP 探测重定向次数过多")
	}
	if _, err := ValidateEchoURL(next.URL.String()); err != nil {
		return err
	}
	first := via[0].URL
	if !strings.EqualFold(next.URL.Scheme, first.Scheme) ||
		!strings.EqualFold(next.URL.Hostname(), first.Hostname()) || echoPort(next.URL) != echoPort(first) {
		return errors.New("IP 探测不允许跨源重定向")
	}
	next.Header.Del("Referer")
	return nil
}

// ParseExitIP accepts a plain IP or a JSON IP string; arbitrary response text is
// never returned to the UI as an address.
func ParseExitIP(body []byte) (string, error) {
	value := strings.TrimSpace(string(body))
	if strings.HasPrefix(value, "{") {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			return "", errors.New("IP 探测响应不是合法 JSON")
		}
		value = ""
		for _, key := range []string{"ip", "origin", "query"} {
			if raw, ok := fields[key]; ok {
				if err := json.Unmarshal(raw, &value); err != nil {
					return "", errors.New("IP 探测响应的地址字段不是字符串")
				}
				break
			}
		}
	}
	ip := net.ParseIP(strings.TrimSpace(value))
	if ip == nil {
		return "", errors.New("IP 探测服务未返回有效 IP")
	}
	return ip.String(), nil
}

// ProbeEcho requests exactly one configured echo service. It has no fallback
// destination and preserves normal HTTPS certificate verification.
func ProbeEcho(ctx context.Context, proxyURL, target string, timeout time.Duration) Result {
	res := Result{Proxy: maskedProbeProxy(proxyURL)}
	clean, err := ValidateEchoURL(target)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if timeout <= 0 || timeout > 20*time.Second {
		timeout = 10 * time.Second
	}
	if _, err := Validate(proxyURL); err != nil {
		res.Error = "代理地址格式无效"
		return res
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			res.Scheme = u.Scheme
		}
	}
	client, err := ClientFor(proxyURL, timeout)
	if err != nil {
		res.Error = "无法创建探测客户端"
		return res
	}
	if proxyURL == "" {
		// An explicit direct test must not inherit HTTP(S)_PROXY.
		tr := client.Transport.(*http.Transport)
		tr.Proxy = nil
		dialer := &net.Dialer{Timeout: timeout, Control: echoDialControl}
		tr.DialContext = dialer.DialContext
	}
	defer client.CloseIdleConnections()
	client.CheckRedirect = echoRedirect
	return probeEchoWithClient(ctx, client, clean, res)
}

func probeEchoWithClient(ctx context.Context, client *http.Client, target string, res Result) Result {
	ctx, cancel := context.WithTimeout(ctx, client.Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		res.Error = "IP 探测请求无效"
		return res
	}
	request.Header.Set("Accept", "application/json, text/plain")
	start := time.Now()
	response, err := client.Do(request)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		// net/http errors include the full URL; do not return tokens in its query.
		res.Error = "IP 探测请求失败"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			res.Error = "IP 探测超时"
		}
		res.Hint = hintFor(res.Scheme, err.Error())
		return res
	}
	defer response.Body.Close()
	res.Status = response.StatusCode
	if response.StatusCode != http.StatusOK {
		res.Error = fmt.Sprintf("IP 探测服务返回 HTTP %d", response.StatusCode)
		return res
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxEchoBytes+1))
	if err != nil {
		res.Error = "读取 IP 探测响应失败"
		return res
	}
	if len(body) > maxEchoBytes {
		res.Error = "IP 探测响应超过 4096 字节"
		return res
	}
	res.ExitIP, err = ParseExitIP(body)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	return res
}

func maskedProbeProxy(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	return u.String()
}
