package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"gondar/db"
)

// These fixtures exercise the real HTTP provider and SDK loop. Fixture calls
// invoke only in-memory counters: no command, file, model service or target runs.
func integrityFrame(delta map[string]any, finish any) string {
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	return "data: " + string(raw) + "\n\n"
}

func integrityToolFrame(index int, id, name, args string) string {
	call := map[string]any{"index": index, "function": map[string]any{"arguments": args}}
	if id != "" {
		call["id"], call["type"] = id, "function"
	}
	if name != "" {
		call["function"].(map[string]any)["name"] = name
	}
	return integrityFrame(map[string]any{"tool_calls": []any{call}}, nil)
}

func integrityUsage(input, output, cached int) string {
	raw, _ := json.Marshal(map[string]any{"choices": []any{}, "usage": map[string]any{
		"prompt_tokens": input, "completion_tokens": output,
		"prompt_tokens_details": map[string]any{"cached_tokens": cached},
	}})
	return "data: " + string(raw) + "\n\n"
}

func integrityToolTurn(interleave bool) string {
	frames := []string{integrityFrame(map[string]any{"content": "开始保存两条受控观察。"}, nil)}
	if interleave {
		frames = append(frames,
			integrityToolFrame(0, "fixture_a", "count_a", `{"label":`),
			integrityToolFrame(1, "fixture_b", "count_b", `{"label":`),
			integrityFrame(map[string]any{"content": "工具参数仍在交错到达。"}, nil),
			integrityToolFrame(0, "", "", `"a"}`),
			integrityToolFrame(1, "", "", `"b"}`))
	} else {
		frames = append(frames,
			integrityToolFrame(0, "fixture_a", "count_a", `{"label":`),
			integrityToolFrame(0, "", "", `"a"}`),
			integrityFrame(map[string]any{"content": "第一条参数完成，再保存第二条。"}, nil),
			integrityToolFrame(1, "fixture_b", "count_b", `{"label":`),
			integrityToolFrame(1, "", "", `"b"}`))
	}
	frames = append(frames, integrityFrame(map[string]any{}, "tool_calls"), integrityUsage(11, 7, 3), "data: [DONE]\n\n")
	return strings.Join(frames, "")
}

func integrityCounter(name, label string, count *atomic.Int64) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: "只在内存中保存受控测试回执", Schema: obj(map[string]any{"label": str("受控标签")}, "label"),
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(_ context.Context, raw json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var input struct {
				Label string `json:"label"`
			}
			if err := json.Unmarshal(raw, &input); err != nil || input.Label != label {
				return actool.Errorf(fmt.Sprintf("合成工具参数受损：%s", raw)), nil
			}
			return actool.Text(fmt.Sprintf("receipt:%s:%d", label, count.Add(1))), nil
		},
	})
}

func integrityProvider(t *testing.T, server *httptest.Server) llm.Provider {
	t.Helper()
	provider, err := (Config{Format: llm.FormatOpenAI, BaseURL: server.URL,
		APIKey: "synthetic-stream-only", Model: "fixture", MaxConcurrent: 1,
		Retry: RetryConfig{ConnectAttempts: -1, EmptyAttempts: -1}}).NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func integrityReceipts(t *testing.T, rows []db.Activity, counts ...*atomic.Int64) {
	t.Helper()
	for index, count := range counts {
		if count.Load() != 1 {
			t.Errorf("counter %d executed %d times, want exactly once", index, count.Load())
		}
	}
	receipts := map[string]int{}
	for _, row := range rows {
		if row.Kind == "tool_result" && !row.IsError {
			receipts[row.Detail]++
		}
	}
	for _, want := range []string{"receipt:a:1", "receipt:b:1"} {
		if receipts[want] != 1 {
			t.Errorf("saved receipt %q count=%d; receipts=%v", want, receipts[want], receipts)
		}
	}
}

func TestStreamIntegrityInterleavedToolsAndUsage(t *testing.T) {
	for _, interleave := range []bool{false, true} {
		t.Run(map[bool]string{false: "sequential_arguments", true: "interleaved_arguments"}[interleave], func(t *testing.T) {
			var requests atomic.Int64
			var a, b atomic.Int64
			var bodiesMu sync.Mutex
			var bodies []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read synthetic request: %v", err)
				}
				bodiesMu.Lock()
				bodies = append(bodies, string(body))
				bodiesMu.Unlock()
				n := requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if n == 1 {
					_, _ = io.WriteString(w, integrityToolTurn(interleave))
					return
				}
				if n != 2 {
					t.Errorf("whole turn was unexpectedly replayed: request=%d", n)
				}
				_, _ = io.WriteString(w, integrityFrame(map[string]any{"content": "两条回执已保存。"}, nil)+
					integrityFrame(map[string]any{}, "stop")+integrityUsage(13, 2, 5)+"data: [DONE]\n\n")
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var rows []db.Activity
			_, reason, err := captureRun(ctx, agentcore.Options{Provider: integrityProvider(t, server),
				Tools:          []actool.CoreTool{integrityCounter("count_a", "a", &a), integrityCounter("count_b", "b", &b)},
				PermissionMode: permission.ModeBypass, MaxTurns: 3, WorkingDir: t.TempDir()}, "仅保存合成回执", func(row db.Activity) { rows = append(rows, row) })
			if err != nil || reason != harness.ReasonCompleted || requests.Load() != 2 {
				t.Fatalf("reason=%s err=%v requests=%d", reason, err, requests.Load())
			}
			integrityReceipts(t, rows, &a, &b)
			bodiesMu.Lock()
			wireBodies := append([]string(nil), bodies...)
			bodiesMu.Unlock()
			if len(wireBodies) != 2 || !strings.Contains(wireBodies[1], "receipt:a:1") || !strings.Contains(wireBodies[1], "receipt:b:1") {
				t.Errorf("next model turn lost saved receipts; captured requests=%d", len(wireBodies))
			}
			terminal := 0
			for _, row := range rows {
				if row.Kind != "result" {
					continue
				}
				terminal++
				if row.IsError || row.InputTokens == nil || *row.InputTokens != 24 || row.OutputTokens == nil || *row.OutputTokens != 9 || row.CacheReadTokens == nil || *row.CacheReadTokens != 8 {
					t.Errorf("usage-only frames lost or duplicated: %+v", row)
				}
			}
			if terminal != 1 {
				t.Errorf("terminal records=%d", terminal)
			}
		})
	}
}

func TestStreamIntegrityRetainsReceiptsAfterNextTurnDisconnect(t *testing.T) {
	var requests atomic.Int64
	var a, b atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := requests.Add(1)
		if n == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, integrityToolTurn(false))
			return
		}
		if n != 2 {
			t.Errorf("model stream was replayed after receipts were saved: %d", n)
		}
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack local fixture: %v", err)
			return
		}
		defer conn.Close()
		partial := integrityFrame(map[string]any{"content": "下一轮在回执保存后中断。"}, nil)
		// A declared body longer than the closed connection produces a genuine
		// HTTP unexpected-EOF, rather than depending on short sleeps or deadlines.
		_, _ = fmt.Fprintf(buffer, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(partial)+64, partial)
		_ = buffer.Flush()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session := agentcore.NewSession(agentcore.Options{Provider: integrityProvider(t, server),
		Tools:          []actool.CoreTool{integrityCounter("count_a", "a", &a), integrityCounter("count_b", "b", &b)},
		PermissionMode: permission.ModeBypass, MaxTurns: 3, WorkingDir: t.TempDir()})
	defer session.Close()
	var rows []db.Activity
	_, reason, err := captureRunSession(ctx, session, "保存回执后模拟断流", func(row db.Activity) { rows = append(rows, row) })
	if err == nil || reason != harness.ReasonModelError || ctx.Err() != nil || requests.Load() != 2 {
		t.Fatalf("disconnect not surfaced without replay: reason=%s err=%v requests=%d", reason, err, requests.Load())
	}
	integrityReceipts(t, rows, &a, &b)
	retained := map[string]int{}
	for _, message := range session.Messages() {
		for _, block := range message.Content {
			if block.Type == llm.BlockToolResult && !block.IsError {
				retained[block.ToolUseID]++
			}
		}
	}
	if retained["fixture_a"] != 1 || retained["fixture_b"] != 1 {
		t.Fatalf("session lost paired tool receipts after model_error: %v", retained)
	}
	terminal := 0
	for _, row := range rows {
		if row.Kind != "result" {
			continue
		}
		terminal++
		if !row.IsError || row.InputTokens == nil || *row.InputTokens != 11 || row.OutputTokens == nil || *row.OutputTokens != 7 {
			t.Errorf("model_error lost completed-turn usage or failed to mark error: %+v", row)
		}
	}
	if terminal != 1 {
		t.Errorf("model_error terminal records=%d", terminal)
	}
}
