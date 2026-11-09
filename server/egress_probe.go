package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"gondar/proxyprobe"
)

const settingEgressProbeURL = "egress_probe_url"

func normalizeEgressProbeSetting(value *string) error {
	if value == nil {
		return nil
	}
	if strings.TrimSpace(*value) == "" {
		*value = ""
		return nil
	}
	clean, err := proxyprobe.ValidateEchoURL(*value)
	if err == nil {
		*value = clean
	}
	return err
}

func (m *Manager) egressProbeConfig() (string, error) {
	if m == nil || m.pg == nil {
		return "", nil
	}
	raw, _, err := m.pg.GetSetting(settingEgressProbeURL)
	return strings.TrimSpace(raw), err
}

// EgressProbeURL is safe to include in authenticated settings responses. An
// invalid manually stored value is never echoed or used as a public fallback.
func (m *Manager) EgressProbeURL() string {
	raw, err := m.egressProbeConfig()
	if err != nil || raw == "" {
		return ""
	}
	clean, err := proxyprobe.ValidateEchoURL(raw)
	if err != nil {
		return ""
	}
	return clean
}

// The installation connectivity test keeps its existing default behavior. Once
// an operator selects an echo service, only that service is requested.
func (s *Server) probeConfiguredProxy(ctx context.Context, proxy string) proxyprobe.Result {
	target, err := s.m.egressProbeConfig()
	if err != nil {
		return proxyprobe.Result{Error: "无法读取 IP 探测服务设置"}
	}
	if target != "" {
		return proxyprobe.ProbeEcho(ctx, proxy, target, 10*time.Second)
	}
	return proxyprobe.Probe(ctx, proxy, "", defaultProxyCheckTarget)
}

func (m *Manager) proxyCheckTarget() (string, error) {
	custom, err := m.egressProbeConfig()
	if err != nil || custom != "" {
		return custom, err
	}
	return settingOr(m.pg, settingProxyCheckTarget, defaultProxyCheckTarget), nil
}

func (m *Manager) checkConfiguredPoolProxy(ctx context.Context, index int, masked, raw, target string, timeout time.Duration) proxyCheckResult {
	custom, err := m.egressProbeConfig()
	if err != nil {
		return proxyCheckResult{Index: index, URL: masked, Error: "无法读取 IP 探测服务设置", CheckedAt: time.Now().Format(time.RFC3339)}
	}
	if custom == "" {
		return checkOneProxy(ctx, index, masked, raw, target, timeout)
	}
	result := proxyprobe.ProbeEcho(ctx, raw, custom, timeout)
	return proxyCheckResult{Index: index, URL: masked, OK: result.OK, LatencyMS: result.LatencyMS,
		ExitIP: result.ExitIP, Error: result.Error, CheckedAt: time.Now().Format(time.RFC3339)}
}

// testEgressProbe tests a saved echo destination without mutating settings. A
// missing proxy uses the saved global egress; an explicit empty string is direct.
func (s *Server) testEgressProbe(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Proxy *string `json:"proxy"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeErr(w, http.StatusBadRequest, "探测请求 JSON 无效或超过大小限制")
		return
	}
	target, err := s.m.egressProbeConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "无法读取 IP 探测服务设置")
		return
	}
	if target == "" {
		target = defaultProxyCheckTarget
	}
	proxy := s.m.GlobalProxy()
	if request.Proxy != nil {
		proxy = proxyprobe.Normalize(*request.Proxy)
	}
	if _, err := proxyprobe.Validate(proxy); err != nil {
		writeErr(w, http.StatusBadRequest, "代理地址格式无效")
		return
	}
	writeJSON(w, http.StatusOK, proxyprobe.ProbeEcho(r.Context(), proxy, target, 10*time.Second))
}
