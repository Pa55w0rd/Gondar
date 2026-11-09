package proxyprobe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateEchoURL(t *testing.T) {
	for _, raw := range []string{" https://echo.example/ip ", "http://10.0.0.5:8080/ip", "http://[fd00::1]/ip"} {
		if _, err := ValidateEchoURL(raw); err != nil {
			t.Fatalf("valid echo URL: %v", err)
		}
	}
	for _, raw := range []string{"", "/ip", "ftp://echo.example/ip", "http://secret:password@echo.example/ip", "http://echo.example/ip#fragment",
		"http://localhost/ip", "http://localhost./ip", "http://a.localhost/ip", "http://127.0.0.1/ip", "http://[::1]/ip",
		"http://[fe80::1%25eth0]/ip", "http://169.254.169.254/latest", "http://0.0.0.0/", "http://224.0.0.1/",
		"http://127.1/", "http://2130706433/", "http://0x7f000001/", "http://echo.example:99999/", "http://echo.example/\n"} {
		if _, err := ValidateEchoURL(raw); err == nil {
			t.Errorf("invalid echo URL accepted: %q", raw)
		} else if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "password") {
			t.Fatal("validation error exposed credentials")
		}
	}
}

func TestParseExitIP(t *testing.T) {
	for _, body := range []string{"203.0.113.7\n", `{"ip":"203.0.113.7"}`, `{"origin":"203.0.113.7"}`} {
		if ip, err := ParseExitIP([]byte(body)); err != nil || ip != "203.0.113.7" {
			t.Fatalf("parse %q = %q, %v", body, ip, err)
		}
	}
	if ip, err := ParseExitIP([]byte(`{"ip":"2001:db8::1"}`)); err != nil || ip != "2001:db8::1" {
		t.Fatal(ip, err)
	}
	for _, body := range []string{`{"ip":12}`, `{"ip":"not-an-ip"}`, `{"ip":`, "<html>proxy error</html>", "203.0.113.7, 203.0.113.8"} {
		if _, err := ParseExitIP([]byte(body)); err == nil {
			t.Fatalf("invalid response accepted: %q", body)
		}
	}
}

func TestProbeEchoUsesOnlyConfiguredService(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Host != "echo.example.invalid" {
			t.Errorf("unexpected public fallback: %s", r.URL.Host)
		}
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, `{"ip":"203.0.113.7"}`)
	}))
	defer proxy.Close()
	for _, path := range []string{"/ip", "/fail"} {
		before := calls.Load()
		result := ProbeEcho(context.Background(), proxy.URL, "http://echo.example.invalid"+path, time.Second)
		if (path == "/ip") != result.OK || calls.Load()-before != 1 {
			t.Fatal("configured probe or failure changed destination", result, calls.Load()-before)
		}
		if result.OK && result.ExitIP != "203.0.113.7" {
			t.Fatal(result)
		}
	}
}

func TestProbeEchoRedirectBoundaryAndResponseLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		location  string
		body      string
		wantOK    bool
		wantCalls int32
	}{
		{"same origin", "/final", `{"ip":"203.0.113.8"}`, true, 2},
		{"other host", "http://other.example.invalid/final", "", false, 1},
		{"other port", "http://echo.example.invalid:81/final", "", false, 1},
		{"other scheme", "https://echo.example.invalid/final", "", false, 1},
		{"userinfo", "http://leak:credential@echo.example.invalid/final", "", false, 1},
		{"too large", "", strings.Repeat("x", maxEchoBytes+1), false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/first" && tc.location != "" {
					w.Header().Set("Location", tc.location)
					w.WriteHeader(http.StatusFound)
					return
				}
				io.WriteString(w, tc.body)
			}))
			defer proxy.Close()
			result := ProbeEcho(context.Background(), proxy.URL, "http://echo.example.invalid/first?token=DO_NOT_ECHO", time.Second)
			if result.OK != tc.wantOK || calls.Load() != tc.wantCalls {
				t.Fatal(result, calls.Load())
			}
			raw, _ := json.Marshal(result)
			if strings.Contains(string(raw), "DO_NOT_ECHO") || strings.Contains(string(raw), "credential") {
				t.Fatal("probe response exposed URL credentials")
			}
		})
	}
}

func TestProbeEchoCancellation(t *testing.T) {
	entered := make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer proxy.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- ProbeEcho(ctx, proxy.URL, "http://echo.example.invalid/ip", 5*time.Second) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("controlled proxy was not contacted")
	}
	cancel()
	select {
	case result := <-done:
		if result.OK {
			t.Fatal("cancelled probe succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled probe did not return")
	}
}

func TestFetchExitIPUsesJSONParser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ip":"203.0.113.9"}`)
	}))
	defer server.Close()
	if ip, err := fetchExitIP(context.Background(), server.Client(), server.URL); err != nil || ip != "203.0.113.9" {
		t.Fatal(ip, err)
	}
}

func TestProbeMasksProxyCredentials(t *testing.T) {
	result := Probe(context.Background(), "http://private-user:DO_NOT_ECHO@127.0.0.1:1", "http://echo.invalid/ip", "")
	raw, _ := json.Marshal(result)
	if result.OK || strings.Contains(string(raw), "private-user") || strings.Contains(string(raw), "DO_NOT_ECHO") {
		t.Fatal("proxy diagnostics exposed credentials")
	}
}
