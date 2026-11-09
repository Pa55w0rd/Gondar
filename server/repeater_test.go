package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gondar/db"
	"gondar/intercept"
	"gondar/traffic"
)

func TestRepeaterParserBindingAndFraming(t *testing.T) {
	for _, tc := range []struct{ name, origin, raw string }{
		{"different host", "https://one.test", "GET / HTTP/1.1\r\nHost: two.test\r\n\r\n"},
		{"duplicate Host", "http://one.test", "GET / HTTP/1.1\r\nHost: one.test\r\nHost: one.test\r\n\r\n"},
		{"absolute mismatch", "http://one.test", "GET http://two.test/ HTTP/1.1\r\nHost: one.test\r\n\r\n"},
		{"scheme mismatch", "https://one.test", "GET http://one.test/ HTTP/1.1\r\nHost: one.test\r\n\r\n"},
		{"port mismatch", "https://one.test:8443", "GET / HTTP/1.1\r\nHost: one.test\r\n\r\n"},
		{"chunked", "http://one.test", "POST / HTTP/1.1\r\nHost: one.test\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n"},
		{"CONNECT", "http://one.test", "CONNECT one.test:80 HTTP/1.1\r\nHost: one.test\r\n\r\n"},
		{"upgrade", "http://one.test", "GET / HTTP/1.1\r\nHost: one.test\r\nUpgrade: websocket\r\n\r\n"},
		{"proxy header", "http://one.test", "GET / HTTP/1.1\r\nHost: one.test\r\nProxy-Authorization: x\r\n\r\n"},
		{"private marker", "http://one.test", "GET / HTTP/1.1\r\nHost: one.test\r\nX-Gondar-Internal-Repeater: x\r\n\r\n"},
		{"folded header", "http://one.test", "GET / HTTP/1.1\r\nHost: one.test\r\n X: x\r\n\r\n"},
		{"origin credentials", "http://u:p@one.test", "GET / HTTP/1.1\r\nHost: one.test\r\n\r\n"},
		{"origin path", "http://one.test/a", "GET / HTTP/1.1\r\nHost: one.test\r\n\r\n"},
		{"body bound", "http://one.test", "POST / HTTP/1.1\r\nHost: one.test\r\n\r\n" + strings.Repeat("a", repeaterBodyLimit+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := parseRepeaterRequest(tc.origin, tc.raw); err == nil {
				t.Fatal("unsafe request accepted")
			}
		})
	}
	for _, tc := range []struct{ origin, host, target string }{
		{"https://ONE.test", "one.test:443", "https://one.test/a?q=1"},
		{"http://[2001:db8::1]:8080", "[2001:0db8::1]:8080", "http://[2001:db8::1]:8080/a?q=1"},
		{"https://例子.测试", "xn--fsqu00a.xn--0zwm56d", "https://xn--fsqu00a.xn--0zwm56d/a?q=1"},
	} {
		req, raw, err := parseRepeaterRequest(tc.origin, "POST /a?q=1 HTTP/1.1\nHost: "+tc.host+"\nContent-Length: 9999\nCookie: session=synthetic\n\n中文")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if req.URL.String() != tc.target || string(body) != "中文" || req.ContentLength != 6 || !strings.Contains(raw, "Content-Length: 6\r\n") {
			t.Fatalf("semantic normalization mismatch: %s %q %d", req.URL, body, req.ContentLength)
		}
		if !strings.Contains(raw, "Cookie: session=synthetic") || !strings.Contains(raw, "Accept-Encoding: identity") {
			t.Fatal("ordinary headers lost")
		}
	}
}

// Only loopback targets and a per-test recording proxy are used. No engine,
// scheduler, background sync, external target or model is started by this helper.
func repeaterTestServer(t *testing.T) (*Server, http.Handler, func(string, string, any) *httptest.ResponseRecorder) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	tr, err := traffic.Open(t.TempDir(), addr)
	if err != nil {
		pg.Close()
		t.Fatal(err)
	}
	m := &Manager{pg: pg, assets: pg.Assets(), tasks: map[string]*Task{}, interceptor: intercept.New(pg), traffic: tr, trafficOn: true}
	_ = m.interceptor.IsToolEnabled("WebFetch") // load enabled tools before replacing the rule cache
	// Explicit in-memory empty policy: do not call an unwired LLM fallback.
	m.interceptor.SetRulesInMemory([]db.InterceptRule{})
	s := &Server{m: m}
	mux := http.NewServeMux()
	s.registerRepeater(mux)
	stopped := make(chan error, 1)
	go func() { stopped <- tr.Start() }()
	readyCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		c, err := (&net.Dialer{}).DialContext(readyCtx, "tcp", addr)
		if err == nil {
			c.Close()
			break
		}
		select {
		case <-readyCtx.Done():
			t.Fatalf("recording proxy not ready: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Cleanup(func() {
		for id := range m.tasks {
			n, _ := strconv.ParseInt(id, 10, 64)
			_ = pg.DeleteTask(n)
		}
		_ = tr.Close()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("proxy did not stop")
		}
		_ = pg.Close()
	})
	do := func(method, path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(b))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	return s, mux, do
}

func repeaterTestTask(t *testing.T, s *Server, ruleKind, pattern string) *Task {
	t.Helper()
	opts := db.TaskCreateOptions{}
	if ruleKind != "" {
		opts.InterceptRules = []db.TaskInterceptRuleInput{{Action: "allow", Kind: ruleKind, Pattern: pattern, Enabled: true}}
	}
	task, err := s.m.CreateTaskWithOptions("synthetic Repeater", "loopback fixtures only", opts)
	if err != nil {
		t.Fatal(err)
	}
	return task
}
func repeaterTestInput(task *Task, target, path string) repeaterSendInput {
	u, _ := url.Parse(target)
	return repeaterSendInput{TaskID: task.ID, Origin: target, RawRequest: "GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\nCookie: synthetic=history\r\n\r\n", TimeoutSeconds: 5}
}
func decodeRepeaterRun(t *testing.T, w *httptest.ResponseRecorder) *db.RepeaterRun {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	var run db.RepeaterRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	return &run
}

func TestRepeaterManagedLoopbackHistoryAndLimits(t *testing.T) {
	s, _, do := repeaterTestServer(t)
	var hits, redirected atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.Write([]byte("wrong target")) }))
	defer second.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get(traffic.RepeaterCaptureHeader) != "" {
			t.Error("private correlation leaked")
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, second.URL, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.URL.Path == "/large" {
			w.Write([]byte(strings.Repeat("a", repeaterResponseLimit+123)))
			return
		}
		w.Write([]byte("synthetic response"))
	}))
	defer target.Close()
	task := repeaterTestTask(t, s, "exact_url", target.URL+"/")
	run := decodeRepeaterRun(t, do("POST", "/api/repeater/send", repeaterTestInput(task, target.URL, "/")))
	if run.State != "completed" || run.TrafficID == "" || run.Status != 200 || string(run.ResponseBody) != "synthetic response" {
		t.Fatalf("managed send incomplete: %+v", run)
	}
	req, resp, err := s.m.Traffic().Get(run.TrafficID)
	if err != nil || !strings.Contains(req, "Cookie: synthetic=history") || !strings.Contains(resp, "synthetic response") || strings.Contains(req, traffic.RepeaterCaptureHeader) {
		t.Fatalf("incorrect exact capture: %v %q %q", err, req, resp)
	}
	taskN, _ := strconv.ParseInt(task.ID, 10, 64)
	saved, err := s.m.pg.GetRepeaterRun(taskN, run.ID)
	if err != nil || saved == nil || saved.Request != run.Request || saved.CompletedAt == nil {
		t.Fatalf("history incomplete: %+v %v", saved, err)
	}
	current, err := s.m.pg.GetTask(taskN)
	if err != nil || current.Status != "created" {
		t.Fatalf("manual send changed lifecycle: %+v %v", current, err)
	}
	if _, err := s.m.assets.CreateTaskInterceptRule(taskN, "allow", "exact_ip", "127.0.0.1", "synthetic", true); err != nil {
		t.Fatal(err)
	}
	redirect := decodeRepeaterRun(t, do("POST", "/api/repeater/send", repeaterTestInput(task, target.URL, "/redirect")))
	if redirect.Status != 302 || redirected.Load() != 0 {
		t.Fatal("redirect followed")
	}
	large := decodeRepeaterRun(t, do("POST", "/api/repeater/send", repeaterTestInput(task, target.URL, "/large")))
	if large.State != "completed" || !large.Truncated || len(large.ResponseBody) != repeaterResponseLimit || large.ResponseSHA256 == "" {
		t.Fatalf("response bound failed: %+v", large)
	}
	other := repeaterTestTask(t, s, "", "")
	if w := do("GET", fmtRepeaterPath("/api/repeater/runs/"+strconv.FormatInt(run.ID, 10), other.ID), nil); w.Code != 404 {
		t.Fatalf("cross-task history leaked: %d", w.Code)
	}
	w := do("GET", fmtRepeaterPath("/api/repeater/runs", task.ID), nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "synthetic=history") {
		t.Fatal("history list leaked raw credential body")
	}
	if hits.Load() != 3 {
		t.Fatalf("unexpected request count: %d", hits.Load())
	}
}

func fmtRepeaterPath(path, taskID string) string { return path + "?task_id=" + taskID }

func TestRepeaterScopeGuardAndCaptureFailClosed(t *testing.T) {
	s, _, do := repeaterTestServer(t)
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte("no send expected")) }))
	defer target.Close()
	for _, tc := range []struct{ name, kind, pattern string }{
		{"no allow", "", ""}, {"fuzzy-only", "fuzzy_url", "127.0.0.1"}, {"outside exact domain", "exact_domain", "elsewhere.invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := repeaterTestTask(t, s, tc.kind, tc.pattern)
			run := decodeRepeaterRun(t, do("POST", "/api/repeater/send", repeaterTestInput(task, target.URL, "/")))
			if run.State != "blocked" {
				t.Fatalf("scope bypass: %+v", run)
			}
		})
	}
	task := repeaterTestTask(t, s, "exact_ip", "127.0.0.1")
	s.m.interceptor.SetRulesInMemory([]db.InterceptRule{{Enabled: true, MatchTarget: "tool_name", MatchType: "exact", Pattern: "WebFetch", Action: "deny", Message: "synthetic-deny"}})
	run := decodeRepeaterRun(t, do("POST", "/api/repeater/send", repeaterTestInput(task, target.URL, "/")))
	if run.State != "blocked" || !strings.Contains(run.Error, "synthetic-deny") {
		t.Fatalf("existing guard bypassed: %+v", run)
	}
	s.m.interceptor.SetRulesInMemory([]db.InterceptRule{})
	s.m.mu.Lock()
	s.m.trafficOn = false
	s.m.mu.Unlock()
	run = decodeRepeaterRun(t, do("POST", "/api/repeater/send", repeaterTestInput(task, target.URL, "/")))
	if run.State != "blocked" || !strings.Contains(run.Error, "录制") {
		t.Fatalf("recording bypassed: %+v", run)
	}
	if hits.Load() != 0 {
		t.Fatalf("unauthorized sends: %d", hits.Load())
	}
}

func TestRepeaterCancelReachesTargetAndPersists(t *testing.T) {
	s, handler, do := repeaterTestServer(t)
	entered := make(chan struct{})
	targetCanceled := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(targetCanceled)
	}))
	defer target.Close()
	task := repeaterTestTask(t, s, "exact_ip", "127.0.0.1")
	in := repeaterTestInput(task, target.URL, "/")
	in.TimeoutSeconds = 60
	b, _ := json.Marshal(in)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/api/repeater/send", bytes.NewReader(b)).WithContext(ctx)
	done := make(chan struct{})
	go func() { defer close(done); handler.ServeHTTP(httptest.NewRecorder(), r) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("target not reached")
	}
	numeric, _ := strconv.ParseInt(task.ID, 10, 64)
	if _, err := s.m.pg.Exec(`UPDATE tasks SET paused=true WHERE id=$1`, numeric); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.QueueTaskArchive(numeric); err == nil || !strings.Contains(err.Error(), "Repeater") {
		t.Fatalf("active Replay archivable: %v", err)
	}
	cancel()
	select {
	case <-targetCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not release upstream")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not terminate")
	}
	runs, err := s.m.pg.ListRepeaterRuns(numeric)
	if err != nil || len(runs) != 1 || runs[0].State != "canceled" || runs[0].CompletedAt == nil {
		t.Fatalf("cancellation history lost: %+v %v", runs, err)
	}
	if w := do("GET", fmtRepeaterPath("/api/repeater/policy", task.ID), nil); w.Code != 200 {
		t.Fatalf("task unusable after cancellation: %d", w.Code)
	}
}

func TestRepeaterRoutesRequireAuthAndExactDomainScope(t *testing.T) {
	s, _, _ := repeaterTestServer(t)
	task := repeaterTestTask(t, s, "exact_domain", "console.example.test")
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	for _, host := range []string{"example.test", "api.example.test", "sub.console.example.test"} {
		u, _ := url.Parse("https://" + host + "/")
		if _, err := s.repeaterScope(id, u); err == nil {
			t.Fatalf("expanded scope to %s", host)
		}
	}
	u, _ := url.Parse("https://console.example.test/api")
	if _, err := s.repeaterScope(id, u); err != nil {
		t.Fatal(err)
	}
	block, err := s.m.pg.CreateAssetInterceptRule("exact_domain", "console.example.test", "synthetic block", true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.m.pg.Exec(`DELETE FROM asset_intercept_rules WHERE id=$1`, block.ID)
	if _, err := s.repeaterScope(id, u); err == nil {
		t.Fatal("allow bypassed global block")
	}
	s.jwtKey.Store([]byte("synthetic-repeater-only-key-32-bytes"))
	handler := s.Handler()
	for _, path := range []string{"/api/repeater/policy", "/api/repeater/runs", "/api/repeater/runs/1", "/api/repeater/send"} {
		method := "GET"
		if strings.HasSuffix(path, "/send") {
			method = "POST"
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		if w.Code != 401 {
			t.Fatalf("unprotected %s: %d", path, w.Code)
		}
	}
	token, err := signJWT(s.currentJWTKey())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", fmtRepeaterPath("/api/repeater/policy", task.ID), nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"redirects_followed":false`) {
		t.Fatalf("registered policy route: %d %s", w.Code, w.Body.String())
	}
}

func TestRepeaterPendingApprovalCancelAndScopeRecheck(t *testing.T) {
	s, handler, _ := repeaterTestServer(t)
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	s.m.interceptor.SetRulesInMemory([]db.InterceptRule{{Enabled: true, MatchTarget: "tool_name", MatchType: "regex", Pattern: "^WebFetch$", Action: "ask", Message: "synthetic approval"}})
	for _, cancelApproval := range []bool{true, false} {
		t.Run(strconv.FormatBool(cancelApproval), func(t *testing.T) {
			task := repeaterTestTask(t, s, "exact_ip", "127.0.0.1")
			in := repeaterTestInput(task, target.URL, "/")
			b, _ := json.Marshal(in)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest("POST", "/api/repeater/send", bytes.NewReader(b)).WithContext(ctx)
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); handler.ServeHTTP(w, r) }()
			var pending db.InterceptPending
			waitCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for pending.ID == 0 {
				rows, err := s.m.pg.ListPendingIntercepts()
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					if row.TaskID != nil && *row.TaskID == task.ID {
						pending = row
						break
					}
				}
				if pending.ID != 0 {
					break
				}
				select {
				case <-waitCtx.Done():
					t.Fatal("approval not created")
				case <-tick.C:
				}
			}
			if !bytes.Contains(pending.ToolInput, []byte("synthetic=history")) {
				t.Fatal("approval missing complete input")
			}
			numeric, _ := strconv.ParseInt(task.ID, 10, 64)
			if cancelApproval {
				cancel()
			} else {
				if _, err := s.m.pg.Exec(`DELETE FROM task_intercept_rules WHERE task_id=$1`, numeric); err != nil {
					t.Fatal(err)
				}
				if err := s.m.interceptor.Decide(pending.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-waitCtx.Done():
				t.Fatal("approval did not unblock")
			}
			runs, err := s.m.pg.ListRepeaterRuns(numeric)
			if err != nil || len(runs) != 1 {
				t.Fatalf("history: %+v %v", runs, err)
			}
			expected := "blocked"
			if cancelApproval {
				expected = "canceled"
			}
			if runs[0].State != expected {
				t.Fatalf("post-approval state %s != %s", runs[0].State, expected)
			}
			var activity string
			if err := s.m.pg.QueryRow(`SELECT COALESCE(string_agg(detail::text,''),'') FROM activity WHERE exploration_id=$1`, task.ExpID).Scan(&activity); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(activity, "synthetic=history") {
				t.Fatal("raw credential copied into ordinary activity")
			}
		})
	}
	if hits.Load() != 0 {
		t.Fatal("canceled or revoked approval sent network request")
	}
}

func TestRepeaterQuotaRejectsBeforeNetworkSend(t *testing.T) {
	s, _, do := repeaterTestServer(t)
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	task := repeaterTestTask(t, s, "exact_ip", "127.0.0.1")
	numeric, _ := strconv.ParseInt(task.ID, 10, 64)
	if _, err := s.m.pg.Exec(`INSERT INTO http_repeater_runs(task_id,url,method,request,state) SELECT $1,'http://one.invalid/','GET','existing evidence','completed' FROM generate_series(1,$2::int)`, numeric, db.RepeaterRunLimit); err != nil {
		t.Fatal(err)
	}
	w := do("POST", "/api/repeater/send", repeaterTestInput(task, target.URL, "/"))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "配额") || hits.Load() != 0 {
		t.Fatalf("quota rejection after send: %d %s hits=%d", w.Code, w.Body.String(), hits.Load())
	}
	usage, err := s.m.pg.RepeaterHistoryUsage(numeric)
	if err != nil || usage.Count != db.RepeaterRunLimit {
		t.Fatalf("quota rejection deleted evidence: %+v %v", usage, err)
	}
}
