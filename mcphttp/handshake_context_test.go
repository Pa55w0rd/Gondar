package mcphttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLegacySSEHandshakeHonorsContext(t *testing.T) {
	for _, phase := range []string{"response_headers", "endpoint_event"} {
		t.Run(phase, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "endpoint_event" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.(http.Flusher).Flush()
				}
				close(started)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer srv.Close()
			defer close(release)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				c, err := NewSSE(ctx, "fixture", srv.URL+"/sse", nil, false)
				if c != nil {
					_ = c.Close()
				}
				result <- err
			}()
			<-started
			cancel()
			select {
			case err := <-result:
				if err == nil || !strings.Contains(err.Error(), "canceled") {
					t.Fatalf("expected cancellation, got %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("SSE handshake ignored canceled caller context")
			}
		})
	}
}
