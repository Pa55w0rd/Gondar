package mcphttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func resultClient(t *testing.T, raw string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(raw)})
	}))
	t.Cleanup(srv.Close)
	return &Client{server: "fixture", url: srv.URL, http: srv.Client()}
}

func TestToolResultSharedSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		isError         bool
	}{
		{name: "structured_only", raw: `{"structuredContent":{"items":[1]}}`, want: `{"items":[1]}`},
		{name: "text_first", raw: `{"content":[{"type":"text","text":"one"},{"type":"text","text":" two"}],"structuredContent":{"ignored":true}}`, want: "one two"},
		{name: "blank_text_fallback", raw: `{"content":[{"type":"text","text":" \n"}],"structuredContent":{"ok":true}}`, want: `{"ok":true}`},
		{name: "empty_object", raw: `{"structuredContent":{}}`, want: `{}`},
		{name: "empty_array", raw: `{"structuredContent":[]}`, want: `[]`},
		{name: "null", raw: `{"structuredContent":null}`, want: ""},
		{name: "absent", raw: `{}`, want: ""},
		{name: "structured_error", raw: `{"structuredContent":{"reason":"failed"},"isError":true}`, want: `{"reason":"failed"}`, isError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := resultClient(t, tc.raw)
			got, err := c.Call(context.Background(), "test", map[string]any{})
			if got != tc.want || (err != nil) != tc.isError {
				t.Fatalf("Call() = %q, %v; want %q, isError=%v", got, err, tc.want, tc.isError)
			}
			wrapped := c.wrap(remoteTool{Name: "test"})
			res, err := wrapped.Call(context.Background(), json.RawMessage(`{}`), nil)
			if err != nil || res.IsError != tc.isError || len(res.Content) != 1 || res.Content[0].Text != tc.want {
				t.Fatalf("wrapper result=%#v, err=%v; want text=%q, isError=%v", res, err, tc.want, tc.isError)
			}
		})
	}
}

func TestStructuredResultCaptureKeepsFullOutputAndError(t *testing.T) {
	full := `{"details":"` + strings.Repeat("evidence", 1000) + `"}`
	c := resultClient(t, `{"isError":true,"structuredContent":`+full+`}`)
	dir := t.TempDir()
	res, err := c.wrap(remoteTool{Name: "test"}).Call(context.Background(), json.RawMessage(`{}`), &actool.ToolContext{OutputDir: dir, MaxOutputChars: 64})
	if err != nil || !res.IsError || len(res.Content) != 1 {
		t.Fatalf("result=%#v, err=%v", res, err)
	}
	if res.Content[0].Text == full {
		t.Fatal("large structured output bypassed Capture")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("spill files=%v, err=%v", files, err)
	}
	saved, err := os.ReadFile(files[0])
	if err != nil || string(saved) != full {
		t.Fatalf("full output was not preserved: bytes=%d, err=%v", len(saved), err)
	}
}
