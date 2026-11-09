package mcphttp

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSSEMessageEndpointOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, base, endpoint, want string
		bad                        bool
	}{
		{name: "relative", base: "https://example.test/nested/sse", endpoint: "message?id=1", want: "https://example.test/nested/message?id=1"},
		{name: "root_relative", base: "https://example.test/sse", endpoint: "/messages", want: "https://example.test/messages"},
		{name: "case_and_default_port", base: "https://EXAMPLE.test/sse", endpoint: "https://example.test:443/messages", want: "https://example.test:443/messages"},
		{name: "explicit_base_port", base: "http://example.test:80/sse", endpoint: "http://example.test/messages", want: "http://example.test/messages"},
		{name: "ipv6", base: "http://[::1]/sse", endpoint: "http://[0:0:0:0:0:0:0:1]:80/messages", want: "http://[0:0:0:0:0:0:0:1]:80/messages"},
		{name: "other_host", base: "https://example.test/sse", endpoint: "https://other.test/messages", bad: true},
		{name: "other_port", base: "https://example.test/sse", endpoint: "https://example.test:8443/messages", bad: true},
		{name: "scheme_relative_other_host", base: "https://example.test/sse", endpoint: "//other.test/messages", bad: true},
		{name: "downgrade", base: "https://example.test/sse", endpoint: "http://example.test:443/messages", bad: true},
		{name: "userinfo", base: "https://example.test/sse", endpoint: "https://user:password@example.test/messages", bad: true},
		{name: "other_scheme", base: "https://example.test/sse", endpoint: "file:///tmp/message", bad: true},
		{name: "invalid_port", base: "https://example.test/sse", endpoint: "https://example.test:65536/messages", bad: true},
		{name: "invalid_base", base: "/sse", endpoint: "/messages", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readSSEEndpoint(bufio.NewReader(strings.NewReader("event: endpoint\ndata: "+tc.endpoint+"\n\n")), tc.base)
			if tc.bad {
				if err == nil {
					t.Fatalf("accepted invalid endpoint: %q", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("endpoint = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// Use custom headers as well as Authorization: net/http strips some standard
// credentials across redirects, but arbitrary MCP API-key headers need a guard.
func TestSSERejectsCrossOriginBeforeSendingHeaders(t *testing.T) {
	for _, mode := range []string{"announced_endpoint", "stream_redirect", "message_redirect"} {
		t.Run(mode, func(t *testing.T) {
			var outsideCalls atomic.Int32
			outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				outsideCalls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer outside.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-Key") != "synthetic-key" {
					t.Error("source did not receive configured custom header")
				}
				if mode == "stream_redirect" || r.URL.Path == "/message" {
					http.Redirect(w, r, outside.URL+"/message", http.StatusTemporaryRedirect)
					return
				}
				endpoint := "/message"
				if mode == "announced_endpoint" {
					endpoint = outside.URL + "/message"
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpoint)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer source.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := NewSSE(ctx, "test", source.URL+"/sse", map[string]string{"X-API-Key": "synthetic-key"}, false)
			if c != nil {
				_ = c.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "cross-origin") {
				t.Fatalf("expected origin rejection, got %v", err)
			}
			if outsideCalls.Load() != 0 {
				t.Fatalf("sent %d request(s) outside the configured origin", outsideCalls.Load())
			}
		})
	}
}
