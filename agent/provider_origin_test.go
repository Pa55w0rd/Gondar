package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderRedirectOriginPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		bad            bool
	}{
		{name: "same", from: "https://example.test/v1", to: "https://example.test/v2"},
		{name: "default_https_port", from: "https://EXAMPLE.test/v1", to: "https://example.test:443/v2"},
		{name: "default_http_port", from: "http://example.test:80/v1", to: "http://example.test/v2"},
		{name: "ipv6", from: "http://[::1]/v1", to: "http://[0:0:0:0:0:0:0:1]:80/v2"},
		{name: "host", from: "https://example.test/v1", to: "https://other.test/v1", bad: true},
		{name: "port", from: "https://example.test/v1", to: "https://example.test:8443/v1", bad: true},
		{name: "downgrade", from: "https://example.test/v1", to: "http://example.test:443/v1", bad: true},
		{name: "userinfo", from: "https://example.test/v1", to: "https://user:pass@example.test/v1", bad: true},
		{name: "initial_userinfo", from: "https://user:pass@example.test/v1", to: "https://example.test/v1", bad: true},
		{name: "scheme", from: "https://example.test/v1", to: "file:///tmp/key", bad: true},
		{name: "missing_host", from: "https://example.test/v1", to: "/relative", bad: true},
		{name: "invalid_port", from: "https://example.test/v1", to: "https://example.test:65536/v1", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from, err := url.Parse(tc.from)
			if err != nil {
				t.Fatal(err)
			}
			to, err := url.Parse(tc.to)
			if err != nil {
				t.Fatal(err)
			}
			err = sameOriginProviderRedirect(&http.Request{URL: to}, []*http.Request{{URL: from}})
			if (err != nil) != tc.bad {
				t.Fatalf("redirect error=%v, reject=%v", err, tc.bad)
			}
		})
	}
}

func TestProviderHTTPRedirectDoesNotLeakCredentials(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var outsideCalls, keyLeaks, sessionLeaks atomic.Int32
			outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				outsideCalls.Add(1)
				if r.Header.Get("X-Api-Key") == "synthetic-model-key" {
					keyLeaks.Add(1)
				}
				if r.Header.Get("X-Session-ID") == "synthetic-session" {
					sessionLeaks.Add(1)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer outside.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, outside.URL+"/capture", status)
			}))
			defer source.Close()
			client, err := quotaAwareHTTPClient("", "")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, source.URL+"/v1/messages", strings.NewReader(`{"prompt":"synthetic-private-prompt"}`))
			req.Header.Set("X-Api-Key", "synthetic-model-key")
			req.Header.Set("X-Session-ID", "synthetic-session")
			resp, err := client.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if outsideCalls.Load() != 0 {
				t.Fatalf("cross-origin requests=%d, key leaks=%d, session leaks=%d", outsideCalls.Load(), keyLeaks.Load(), sessionLeaks.Load())
			}
			if err == nil || !strings.Contains(err.Error(), "cross-origin") {
				t.Fatalf("expected origin rejection, got %v", err)
			}
		})
	}
}

func TestProviderHTTPSameOriginRedirectPreservesRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			http.Redirect(w, r, "/nested/messages", http.StatusTemporaryRedirect)
			return
		}
		data, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || string(data) != "synthetic-prompt" || r.Header.Get("X-Api-Key") != "synthetic-model-key" || r.Header.Get("X-Session-ID") != "synthetic-session" {
			t.Error("same-origin redirect lost method, body or headers")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client, err := quotaAwareHTTPClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader("synthetic-prompt"))
	req.Header.Set("X-Api-Key", "synthetic-model-key")
	req.Header.Set("X-Session-ID", "synthetic-session")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestProviderHTTPRedirectLoopIsBounded(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/loop", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	client, err := quotaAwareHTTPClient("", "")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/loop", strings.NewReader("synthetic-prompt"))
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "10 redirects") || calls.Load() != 10 {
		t.Fatalf("requests=%d, error=%v", calls.Load(), err)
	}
}
