package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/idna"
	"gondar/db"
	"gondar/intercept"
	"gondar/traffic"
)

const repeaterRequestLimit = db.RepeaterRequestLimit
const repeaterBodyLimit = 64 << 10
const repeaterResponseLimit = db.RepeaterResponseLimit

var repeaterPermits = make(chan struct{}, 4)

type repeaterSendInput struct {
	TaskID          string `json:"task_id"`
	Origin          string `json:"origin"`
	RawRequest      string `json:"raw_request"`
	SourceTrafficID string `json:"source_traffic_id,omitempty"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
}

func (s *Server) registerRepeater(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/repeater/policy", s.repeaterPolicy)
	mux.HandleFunc("POST /api/repeater/send", s.repeaterSend)
	mux.HandleFunc("GET /api/repeater/runs", s.repeaterRuns)
	mux.HandleFunc("GET /api/repeater/runs/{id}", s.repeaterRun)
}

func repeaterTaskID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("请选择有效任务")
	}
	return id, nil
}

func repeaterAuthority(authority, scheme string) (string, error) {
	u, err := url.Parse(scheme + "://" + authority)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("主机格式无效")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else {
		host, err = idna.Lookup.ToASCII(host)
		if err != nil || host == "" {
			return "", errors.New("域名格式无效")
		}
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("端口无效")
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}

// A semantic HTTP/1 editor: content length is recomputed, framing/upgrade and
// proxy headers are rejected. It is deliberately not a byte-level raw socket.
func parseRepeaterRequest(origin, raw string) (*http.Request, string, error) {
	if len(raw) > repeaterRequestLimit {
		return nil, "", errors.New("原始请求超过128 KiB")
	}
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, "", errors.New("连接目标只能是HTTP(S)协议、主机和端口，不含路径、查询或凭据")
	}
	authority, err := repeaterAuthority(u.Host, u.Scheme)
	if err != nil {
		return nil, "", err
	}
	cut, sep := strings.Index(raw, "\r\n\r\n"), 4
	if cut < 0 {
		cut, sep = strings.Index(raw, "\n\n"), 2
	}
	if cut < 0 {
		cut, sep = len(raw), 0
	}
	head, body := raw[:cut], raw[cut+sep:]
	if len(head) > 64<<10 || len(body) > repeaterBodyLimit {
		return nil, "", errors.New("请求头或正文超过64 KiB")
	}
	lines := strings.Split(strings.ReplaceAll(head, "\r\n", "\n"), "\n")
	var normalized strings.Builder
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, "", errors.New("缺少HTTP请求行")
	}
	normalized.WriteString(lines[0] + "\r\n")
	hostHeader := ""
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.ContainsRune(line, '\r') {
			return nil, "", errors.New("不支持折叠或非法请求头")
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, "", errors.New("请求头格式无效")
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if key == "host" {
			if hostHeader != "" {
				return nil, "", errors.New("Host头只能有一个")
			}
			hostHeader = value
		}
		if key == "content-length" {
			continue
		}
		if key == "transfer-encoding" || key == "trailer" || key == "upgrade" || strings.HasPrefix(key, "proxy-") || strings.EqualFold(key, traffic.RepeaterCaptureHeader) {
			return nil, "", errors.New("不支持分块、升级或代理控制请求头")
		}
		if key == "connection" {
			if !strings.EqualFold(value, "close") && !strings.EqualFold(value, "keep-alive") {
				return nil, "", errors.New("不支持Connection扩展")
			}
			continue
		}
		normalized.WriteString(line + "\r\n")
	}
	hostAuthority, err := repeaterAuthority(hostHeader, u.Scheme)
	if err != nil || hostHeader == "" || hostAuthority != authority {
		return nil, "", errors.New("Host头必须与连接目标的主机和端口一致")
	}
	if len(body) > 0 {
		fmt.Fprintf(&normalized, "Content-Length: %d\r\n", len(body))
	}
	normalized.WriteString("\r\n" + body)
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(normalized.String())))
	if err != nil {
		return nil, "", errors.New("HTTP请求格式无效：" + err.Error())
	}
	if req.Method == "CONNECT" || req.ProtoMajor != 1 {
		return nil, "", errors.New("仅支持普通HTTP/1请求，不支持CONNECT")
	}
	if req.URL.IsAbs() {
		other, err := repeaterAuthority(req.URL.Host, req.URL.Scheme)
		if err != nil || req.URL.Scheme != u.Scheme || other != authority {
			return nil, "", errors.New("绝对请求URL不能改变连接目标")
		}
	} else if !strings.HasPrefix(req.RequestURI, "/") {
		return nil, "", errors.New("请求路径必须以 / 开头")
	}
	if req.URL.Fragment != "" || req.URL.User != nil {
		return nil, "", errors.New("请求URL不能携带片段或凭据")
	}
	host, _, _ := net.SplitHostPort(authority)
	if u.Port() != "" {
		u.Host = authority
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	u.Path = req.URL.Path
	u.RawPath = req.URL.RawPath
	u.RawQuery = req.URL.RawQuery
	req.URL = u
	req.Host = u.Host
	req.RequestURI = ""
	req.Close = true
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "identity")
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Gondar-Repeater")
	}
	data, err := httputil.DumpRequestOut(req, true)
	if err != nil || len(data) > repeaterRequestLimit {
		return nil, "", errors.New("规范化请求超过128 KiB或无法编码")
	}
	return req, string(data), nil
}

func (s *Server) repeaterScope(taskID int64, u *url.URL) ([]db.AssetInterceptRule, error) {
	global, err := s.m.pg.ListAssetInterceptRules()
	if err != nil {
		return nil, err
	}
	block, allow, err := s.m.assets.TaskInterceptRulesSplit(taskID)
	if err != nil {
		return nil, err
	}
	strict := []db.AssetInterceptRule{}
	for _, r := range allow {
		if r.Enabled && (r.Kind == "exact_domain" || r.Kind == "exact_ip" || r.Kind == "exact_url" || r.Kind == "cidr") {
			strict = append(strict, r)
		}
	}
	if u == nil {
		return strict, nil
	}
	if len(strict) == 0 {
		return nil, errors.New("任务没有启用精确允许规则，请在任务范围规则中配置exact_domain/exact_ip/exact_url或CIDR允许项")
	}
	host := u.Hostname()
	domains, ips := []string{}, []string{}
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip.String())
	} else {
		domains = append(domains, host)
	}
	decision := db.EvaluateAssetGate(append(global, block...), strict, domains, ips, []string{u.String()})
	if !decision.Allowed {
		return nil, errors.New(decision.Reason)
	}
	return strict, nil
}

func (s *Server) repeaterPolicy(w http.ResponseWriter, r *http.Request) {
	id, err := repeaterTaskID(r.URL.Query().Get("task_id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	task, err := s.m.pg.GetTask(id)
	if err != nil || task == nil {
		writeErr(w, 404, "任务不存在")
		return
	}
	allow, err := s.repeaterScope(id, nil)
	if err != nil {
		writeErr(w, 503, "无法读取任务范围")
		return
	}
	usage, err := s.m.pg.RepeaterHistoryUsage(id)
	if err != nil {
		writeErr(w, 503, "无法读取历史保留配额")
		return
	}
	writeJSON(w, 200, map[string]any{"allow_rules": allow, "capture_enabled": s.m.TrafficEnabled() && s.m.Traffic() != nil, "request_limit": repeaterRequestLimit, "response_limit": repeaterResponseLimit, "redirects_followed": false, "run_limit": db.RepeaterRunLimit, "storage_limit": db.RepeaterStorageLimit, "run_count": usage.Count, "reserved_bytes": usage.ReservedBytes})
}

func (s *Server) repeaterRuns(w http.ResponseWriter, r *http.Request) {
	id, err := repeaterTaskID(r.URL.Query().Get("task_id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	task, err := s.m.pg.GetTask(id)
	if err != nil || task == nil {
		writeErr(w, 404, "任务不存在")
		return
	}
	runs, err := s.m.pg.ListRepeaterRuns(id)
	if err != nil {
		writeErr(w, 500, "无法读取历史")
		return
	}
	writeJSON(w, 200, map[string]any{"runs": runs})
}
func (s *Server) repeaterRun(w http.ResponseWriter, r *http.Request) {
	id, err := repeaterTaskID(r.URL.Query().Get("task_id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	runID, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "历史编号无效")
		return
	}
	task, err := s.m.pg.GetTask(id)
	if err != nil || task == nil {
		writeErr(w, 404, "任务不存在")
		return
	}
	run, err := s.m.pg.GetRepeaterRun(id, runID)
	if err != nil || run == nil {
		writeErr(w, 404, "记录不存在")
		return
	}
	writeJSON(w, 200, run)
}

func (s *Server) repeaterSend(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var in repeaterSendInput
	if err := dec.Decode(&in); err != nil {
		writeErr(w, 400, "发送参数无效或过大")
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		writeErr(w, 400, "只接受一个参数对象")
		return
	}
	id, err := repeaterTaskID(in.TaskID)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	task, ok := s.m.Task(strconv.FormatInt(id, 10))
	if !ok {
		writeErr(w, 404, "任务不存在")
		return
	}
	req, raw, err := parseRepeaterRequest(in.Origin, in.RawRequest)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if len(in.SourceTrafficID) > 200 {
		writeErr(w, 400, "来源流量编号无效")
		return
	}
	if in.SourceTrafficID != "" {
		if s.m.Traffic() == nil {
			writeErr(w, 404, "来源流量不可用")
			return
		}
		if _, _, err := s.m.Traffic().Get(in.SourceTrafficID); err != nil {
			writeErr(w, 404, "来源流量不存在")
			return
		}
	}
	run := &db.RepeaterRun{TaskID: id, SourceTrafficID: in.SourceTrafficID, URL: req.URL.String(), Method: req.Method, Request: raw, State: "running"}
	if err := s.m.pg.CreateRepeaterRun(r.Context(), run); err != nil {
		if errors.Is(err, db.ErrRepeaterQuota) {
			writeErr(w, 409, err.Error())
			return
		}
		if errors.Is(err, db.ErrTaskArchiveState) {
			writeErr(w, 409, "任务正在归档或恢复，未发送")
			return
		}
		writeErr(w, 503, "无法保存请求，未执行发送")
		return
	}
	start := time.Now()
	finish := func() {
		run.ElapsedMS = time.Since(start).Milliseconds()
		run.ResponseBytes = len(run.ResponseBody)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.m.pg.FinishRepeaterRun(ctx, run); err != nil {
			writeErr(w, 503, "无法保存执行结果，请刷新历史核对")
			return
		}
		_, _ = task.Store.AppendActivity(db.Activity{Worker: "HTTP Repeater", Kind: "tool_result", Tool: "http_request", ToolUseID: fmt.Sprintf("repeater-%d", run.ID), IsError: run.State != "completed", Summary: fmt.Sprintf("HTTP Repeater #%d %s status=%d", run.ID, run.State, run.Status), Detail: fmt.Sprintf(`{"repeater_run_id":%d,"traffic_id":%q,"state":%q,"truncated":%t}`, run.ID, run.TrafficID, run.State, run.Truncated)})
		if r.Context().Err() == nil {
			writeJSON(w, 200, run)
		}
	}
	defer finish()
	fail := func(state string, err error) { run.State = state; run.Error = err.Error() }
	if _, err := s.repeaterScope(id, req.URL); err != nil {
		fail("blocked", err)
		return
	}
	if task.Guard == nil {
		fail("blocked", errors.New("任务拦截服务未就绪"))
		return
	}
	ctx := intercept.WithTaskContext(r.Context(), task.ID, "HTTP Repeater", func(a db.Activity) {
		// The approval store retains the complete call for review. Ordinary task
		// activity only points to that record, rather than duplicating cookies.
		var detail struct {
			PendingID int64 `json:"pending_id"`
		}
		_ = json.Unmarshal([]byte(a.Detail), &detail)
		metadata, _ := json.Marshal(map[string]any{"pending_id": detail.PendingID, "tool": "WebFetch", "input": map[string]any{"repeater_run_id": run.ID, "method": req.Method}})
		a.Detail = string(metadata)
		a.Worker = "HTTP Repeater"
		_, _ = task.Store.AppendActivity(a)
	})
	input, _ := json.Marshal(map[string]any{"url": req.URL.String(), "method": req.Method, "raw_request": raw, "repeater_run_id": run.ID})
	_, _ = task.Store.AppendActivity(db.Activity{Worker: "HTTP Repeater", Kind: "tool_use", Tool: "http_request", ToolUseID: fmt.Sprintf("repeater-%d", run.ID), Summary: fmt.Sprintf("HTTP Repeater #%d %s", run.ID, req.Method)})
	// WebFetch is the existing HTTP policy surface, not a new bypass tool name.
	blocked, message, updated := task.Guard.Hooks().PreToolUse(ctx, "WebFetch", input)
	defer func() {
		result, _ := json.Marshal(map[string]any{"run_id": run.ID, "state": run.State, "status": run.Status, "truncated": run.Truncated})
		task.Guard.Hooks().PostToolUse(ctx, "WebFetch", input, result, run.State != "completed")
	}()
	if ctx.Err() != nil {
		fail("canceled", ctx.Err())
		return
	}
	if blocked {
		fail("blocked", errors.New(message))
		return
	}
	if len(updated) > 0 && !bytes.Equal(updated, input) {
		fail("blocked", errors.New("策略修改了请求，请复核后重新发送"))
		return
	}
	if ctx.Err() != nil {
		fail("canceled", ctx.Err())
		return
	}
	if _, err := s.repeaterScope(id, req.URL); err != nil {
		fail("blocked", err)
		return
	}
	visible, err := s.m.pg.GetTask(id)
	if err != nil || visible == nil {
		fail("blocked", errors.New("任务已移除，未发送"))
		return
	}
	tr := s.m.Traffic()
	if tr == nil || !s.m.TrafficEnabled() {
		fail("blocked", errors.New("请先启用流量录制；Repeater不会绕过录制代理直连"))
		return
	}
	if !tr.RepeaterCaptureSupported(req.URL.Hostname()) {
		fail("blocked", errors.New("该主机处于录制透传模式，无法安全剥离关联标识，未发送"))
		return
	}
	select {
	case repeaterPermits <- struct{}{}:
		defer func() { <-repeaterPermits }()
	case <-ctx.Done():
		fail("canceled", ctx.Err())
		return
	}
	seconds := in.TimeoutSeconds
	if seconds <= 0 {
		seconds = 20
	}
	if seconds > 60 {
		seconds = 60
	}
	sendCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	ticket := tr.NewRepeaterCapture(req.URL.Hostname())
	defer ticket.Close()
	req = req.WithContext(sendCtx)
	req.Header.Set(traffic.RepeaterCaptureHeader, ticket.Token())
	proxy, err := url.Parse(s.m.ProxyAddr())
	if err != nil || proxy.Scheme == "" {
		fail("error", errors.New("录制代理配置无效"))
		return
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if pem, err := os.ReadFile(s.m.ProxyCACert()); err == nil {
		roots.AppendCertsFromPEM(pem)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 64 << 10, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, ResponseHeaderTimeout: time.Duration(seconds) * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer func() {
		captureCtx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		result := ticket.Wait(captureCtx)
		run.TrafficID = result.TrafficID
		run.CaptureNote = result.Note
	}()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			fail("canceled", ctx.Err())
		} else {
			fail("error", err)
		}
		return
	}
	defer resp.Body.Close()
	run.Status = resp.StatusCode
	var head strings.Builder
	fmt.Fprintf(&head, "%s %s\r\n", resp.Proto, resp.Status)
	_ = resp.Header.Write(&head)
	run.ResponseHead = head.String()
	if len(run.ResponseHead) > 64<<10 {
		run.ResponseHead = run.ResponseHead[:64<<10]
		run.Truncated = true
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, repeaterResponseLimit+1))
	if len(body) > repeaterResponseLimit {
		body = body[:repeaterResponseLimit]
		run.Truncated = true
	}
	run.ResponseBody = body
	sum := sha256.Sum256(body)
	run.ResponseSHA256 = hex.EncodeToString(sum[:])
	run.State = "completed"
	if readErr != nil {
		if ctx.Err() != nil {
			fail("canceled", ctx.Err())
		} else {
			fail("error", readErr)
		}
	}
}
