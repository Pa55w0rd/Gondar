package mcphttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// All endpoints and credentials are synthetic loopback fixtures. A redirect must
// be rejected before the recipient sees API-key headers, session IDs or RPC data.
func TestHTTPRejectsCrossOriginBeforeSendingCredentials(t *testing.T) {
	for _, phase := range []string{"initialize", "notifications/initialized", "tools/call", "close"} {
		t.Run(phase, func(t *testing.T) {
			var outsideCalls atomic.Int32
			var keyLeaks, sessionLeaks atomic.Int32
			outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				outsideCalls.Add(1)
				if r.Header.Get("X-API-Key") == "synthetic-key" {
					keyLeaks.Add(1)
				}
				if r.Header.Get("Mcp-Session-Id") == "synthetic-session" {
					sessionLeaks.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			}))
			defer outside.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-Key") != "synthetic-key" {
					t.Error("configured endpoint did not receive its key")
				}
				if r.Method == http.MethodDelete {
					if phase == "close" {
						http.Redirect(w, r, outside.URL+"/capture", http.StatusTemporaryRedirect)
						return
					}
					w.WriteHeader(http.StatusNoContent)
					return
				}
				var req rpcRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if req.Method == phase {
					http.Redirect(w, r, outside.URL+"/capture", http.StatusTemporaryRedirect)
					return
				}
				if req.ID == 0 {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Mcp-Session-Id", "synthetic-session")
				_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)})
			}))
			defer source.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := New(ctx, "fixture", source.URL+"/mcp", map[string]string{"X-API-Key": "synthetic-key"}, false)
			if phase == "tools/call" || phase == "close" {
				if err != nil {
					t.Fatalf("initialize: %v", err)
				}
				if phase == "tools/call" {
					_, err = c.Call(ctx, "fixture", map[string]any{"evidence": "synthetic-private-data"})
				}
			}
			if c != nil {
				_ = c.Close()
			}
			if outsideCalls.Load() != 0 {
				t.Fatalf("sent %d request(s) outside the configured MCP origin; key leaks=%d, session leaks=%d", outsideCalls.Load(), keyLeaks.Load(), sessionLeaks.Load())
			}
			if phase != "close" && (err == nil || !strings.Contains(err.Error(), "cross-origin")) {
				t.Fatalf("expected origin rejection, got %v", err)
			}
		})
	}
}

func TestHTTPPreservesSameOriginRedirects(t *testing.T) {
	var calls, closes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			http.Redirect(w, r, "/nested/mcp", http.StatusTemporaryRedirect)
			return
		}
		if r.Header.Get("X-API-Key") != "synthetic-key" {
			t.Error("same-origin redirect lost configured header")
		}
		if r.Method == http.MethodDelete {
			closes.Add(1)
			if r.Header.Get("Mcp-Session-Id") != "synthetic-session" {
				t.Error("close lost session ID")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.ID == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := json.RawMessage(`{}`)
		if req.Method == "tools/call" {
			calls.Add(1)
			if r.Header.Get("Mcp-Session-Id") != "synthetic-session" {
				t.Error("tool call lost session ID")
			}
			result = json.RawMessage(`{"content":[{"type":"text","text":"fixture-ok"}]}`)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "synthetic-session")
		_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := New(ctx, "fixture", srv.URL+"/mcp", map[string]string{"X-API-Key": "synthetic-key"}, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Call(ctx, "fixture", nil)
	if err != nil || got != "fixture-ok" {
		t.Fatalf("tool call = %q, %v", got, err)
	}
	_ = c.Close()
	if calls.Load() != 1 || closes.Load() != 1 {
		t.Fatalf("calls=%d, closes=%d", calls.Load(), closes.Load())
	}
}
