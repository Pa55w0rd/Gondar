package mcphttp

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testRPCFrameLimit = 8 << 20

func TestHTTPRejectsOversizedJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":"`+strings.Repeat("x", testRPCFrameLimit)+`"}`)
	}))
	defer srv.Close()
	c := &Client{url: srv.URL, http: srv.Client()}
	_, err := c.call(context.Background(), "tools/list", nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected frame limit, got %v", err)
	}
}

func TestSSERejectsOversizedMultiLineEvent(t *testing.T) {
	var stream strings.Builder
	stream.WriteString("data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":\"")
	chunk := strings.Repeat("x", 64<<10)
	for i := 0; i < 130; i++ {
		stream.WriteString(chunk)
		stream.WriteString("\ndata: ")
	}
	stream.WriteString("\"}\n\n")
	_, err := parseSSE(strings.NewReader(stream.String()), 1)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected cumulative event limit, got %v", err)
	}
}

func TestLegacySSERejectsOversizedEndpointAndResponseLines(t *testing.T) {
	endpoint := "data: /message?session=" + strings.Repeat("x", testRPCFrameLimit) + "\n"
	_, err := readSSEEndpoint(bufio.NewReader(strings.NewReader(endpoint)), "https://example.test/sse")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected endpoint line limit, got %v", err)
	}
	c := &Client{streamDone: make(chan struct{}), streamErr: make(chan error, 1), pending: make(map[int]chan *rpcResponse)}
	response := `data: {"jsonrpc":"2.0","id":1,"result":"` + strings.Repeat("x", testRPCFrameLimit) + "\"}\n\n"
	c.readLegacySSE(bufio.NewReader(strings.NewReader(response)))
	select {
	case err := <-c.streamErr:
		if !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("expected response line limit, got %v", err)
		}
	default:
		t.Fatal("oversized legacy response did not fail its stream")
	}
}
