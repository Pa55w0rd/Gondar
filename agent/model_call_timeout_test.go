package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

func deadlineFixtureProvider(t *testing.T, srv *httptest.Server, format llm.Format, limit time.Duration) (*modelCallTimeoutProvider, Config) {
	t.Helper()
	cfg := Config{Format: format, BaseURL: srv.URL, APIKey: "synthetic-deadline-only", Model: "fixture", MaxConcurrent: 1, Retry: RetryConfig{ConnectAttempts: -1, EmptyAttempts: -1}}
	p, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	bounded, ok := p.(*modelCallTimeoutProvider)
	if !ok {
		t.Fatal("NewProvider did not install logical-call bound")
	}
	bounded.timeout = limit
	return bounded, cfg
}

func TestModelCallTimeoutBoundsHeadersAndCompleteBody(t *testing.T) {
	for _, headers := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_headers", true: "body_stalled"}[headers], func(t *testing.T) {
			released := make(chan struct{})
			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				calls.Add(1)
				if headers {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					_, _ = io.WriteString(w, `{"choices":[`)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
				close(released)
			}))
			defer srv.Close()
			p, cfg := deadlineFixtureProvider(t, srv, llm.FormatOpenAI, time.Second)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			_, _, _, err := p.Complete(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("synthetic")}})
			if !errors.Is(err, ErrModelCallTimeout) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err=%v", err)
			}
			if ctx.Err() != nil || calls.Load() != 1 {
				t.Fatalf("parent canceled/replayed: calls=%d cause=%v", calls.Load(), context.Cause(ctx))
			}
			select {
			case <-released:
			case <-ctx.Done():
				t.Fatal("HTTP request body/connection not canceled")
			}
			awaitRequestGate(t, requestIdentity(cfg), 0, 0)
		})
	}
}

func TestModelCallTimeoutStreamPreservesUsageAndTerminal(t *testing.T) {
	var calls atomic.Int64
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"fixture","type":"message","role":"assistant","content":[],"usage":{"input_tokens":11,"output_tokens":0}}}` + "\n\n",
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial fixture"}}` + "\n\n",
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{},"usage":{"output_tokens":3}}` + "\n\n",
		} {
			_, _ = io.WriteString(w, frame)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(released)
	}))
	defer srv.Close()
	p, cfg := deadlineFixtureProvider(t, srv, llm.FormatAnthropic, time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var rows []db.Activity
	_, reason, err := captureRun(ctx, agentcore.Options{Provider: p, MaxTurns: 3, WorkingDir: t.TempDir()}, "synthetic", func(a db.Activity) { rows = append(rows, a) })
	if !errors.Is(err, ErrModelCallTimeout) || reason != harness.ReasonModelError || ctx.Err() != nil || calls.Load() != 1 {
		t.Fatalf("reason=%s err=%v calls=%d", reason, err, calls.Load())
	}
	terminal := 0
	for _, row := range rows {
		if row.Kind == "result" {
			terminal++
			if !row.IsError || row.InputTokens == nil || *row.InputTokens != 11 || row.OutputTokens == nil || *row.OutputTokens != 3 {
				t.Fatalf("terminal lost usage/error: %+v", row)
			}
		}
	}
	if terminal != 1 {
		t.Fatalf("terminal count=%d", terminal)
	}
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("stalled SSE did not cancel")
	}
	awaitRequestGate(t, requestIdentity(cfg), 0, 0)
}

func TestModelCallTimeoutCancelsSDKRetryBackoff(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "temporary", 503) }))
	defer srv.Close()
	cfg := Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, APIKey: "synthetic-retry-only", Model: "fixture", MaxConcurrent: 1, Retry: RetryConfig{ConnectAttempts: 5, ConnectInterval: 3 * time.Second}}
	prov, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	p := prov.(*modelCallTimeoutProvider)
	p.timeout = time.Second
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, _, err := p.Complete(ctx, llm.CompletionRequest{}); !errors.Is(err, ErrModelCallTimeout) {
		t.Fatalf("err=%v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("deadline permitted SDK replay after bound: %d", calls.Load())
	}
	awaitRequestGate(t, requestIdentity(cfg), 0, 0)
}

func TestModelCallTimeoutPreservesCompleteValues(t *testing.T) {
	want := llm.Usage{InputTokens: 13, OutputTokens: 2}
	p := &modelCallTimeoutProvider{timeout: time.Second, inner: plannerStateProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		<-ctx.Done()
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("returned before error")}}, "partial", want, ctx.Err()
	}}}
	msg, stop, usage, err := p.Complete(t.Context(), llm.CompletionRequest{})
	if !errors.Is(err, ErrModelCallTimeout) || stop != "partial" || usage != want || len(msg.Content) != 1 {
		t.Fatalf("values erased: stop=%s usage=%+v msg=%+v err=%v", stop, usage, msg, err)
	}
}

func TestModelCallDeadlineUsesParentTaskAndRun(t *testing.T) {
	parentDeadline := time.Now().Add(time.Minute)
	parent, stop := context.WithDeadline(t.Context(), parentDeadline)
	defer stop()
	call, cancel, _ := modelCallContext(parent, 2*time.Minute)
	defer cancel()
	got, _ := call.Deadline()
	if !got.Equal(parentDeadline) {
		t.Fatalf("parent deadline extended: %v", got)
	}
	taskEnd := time.Now().Add(30 * time.Second).Unix()
	call, cancel, _ = modelCallContext(WithTaskClock(parent, TaskClock{DeadlineUnix: taskEnd}), 2*time.Minute)
	defer cancel()
	got, _ = call.Deadline()
	if !got.Equal(time.Unix(taskEnd, 0)) {
		t.Fatalf("task deadline ignored: %v", got)
	}
	call, cancel, limit := modelCallContext(withModelCallLimit(t.Context(), 20*time.Second), time.Minute)
	defer cancel()
	if limit > 20*time.Second || limit < 19*time.Second {
		t.Fatalf("run cap ignored: %v", limit)
	}
	call, cancel, _ = modelCallContext(WithTaskClock(t.Context(), TaskClock{DeadlineUnix: 1, Final: true}), time.Minute)
	defer cancel()
	if call.Err() != nil {
		t.Fatal("expired task clock disabled explicit final round")
	}
	parent, causeCancel := context.WithCancelCause(t.Context())
	call, cancel, limit = modelCallContext(parent, time.Minute)
	defer cancel()
	causeCancel(AbortPausedByUser)
	err := modelCallError(parent, call, parent.Err(), limit, time.Now())
	if errors.Is(err, ErrModelCallTimeout) || context.Cause(call) != AbortPausedByUser {
		t.Fatalf("parent cancel relabeled: %v", err)
	}
}

func TestModelCallConfiguredBudgetReplacesFallback(t *testing.T) {
	for _, budget := range []time.Duration{30 * time.Second, 20 * time.Minute, time.Hour} {
		ctx := withModelCallLimit(t.Context(), budget)
		call, cancel, limit := modelCallContext(ctx, defaultModelCallTimeout)
		deadline, ok := call.Deadline()
		cancel()
		if !ok || limit > budget || limit < budget-time.Second || time.Until(deadline) < budget-time.Second {
			t.Fatalf("configured budget %v replaced by %v", budget, limit)
		}
	}
	// Unlimited runs still have a bounded individual network call.
	call, cancel, limit := modelCallContext(withModelCallLimit(t.Context(), 0), defaultModelCallTimeout)
	defer cancel()
	if _, ok := call.Deadline(); !ok || limit > defaultModelCallTimeout || limit < defaultModelCallTimeout-time.Second {
		t.Fatalf("unlimited run lost network-call protection: %v", limit)
	}
}

func TestModelCallRunCapPreservesSDKSettlement(t *testing.T) {
	calls := 0
	p := &modelCallTimeoutProvider{timeout: time.Minute, inner: plannerStateProvider{complete: func(_ context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "wait", Name: "wait_budget", Input: []byte(`{}`)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("checkpoint")}}, "end_turn", llm.Usage{}, nil
	}}}
	wait := actool.Build(actool.Spec{Name: "wait_budget", Schema: obj(nil), Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
		return permission.Allowed()
	}, Run: func(ctx context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
		<-ctx.Done()
		return actool.Result{}, ctx.Err()
	}})
	_, reason, err := captureRun(t.Context(), agentcore.Options{Provider: p, NonStreaming: true, Tools: []actool.CoreTool{wait}, PermissionMode: permission.ModeBypass, WorkingDir: t.TempDir(), MaxDuration: 100 * time.Millisecond, Settlement: &harness.Settlement{Prompt: "checkpoint", MaxTurns: 2}}, "fixture", nil)
	if err != nil || reason != harness.ReasonTimeout || calls != 2 {
		t.Fatalf("normal SDK settlement changed: reason=%s calls=%d err=%v", reason, calls, err)
	}
	if t.Context().Err() != nil {
		t.Fatal("run cap canceled parent")
	}
}

func TestModelCallConsumerExitAndParentCancelRelease(t *testing.T) {
	for _, early := range []bool{true, false} {
		t.Run(map[bool]string{true: "consumer_exit", false: "parent_cancel"}[early], func(t *testing.T) {
			released := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fixture\"},\"finish_reason\":null}]}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(released)
			}))
			defer srv.Close()
			p, cfg := deadlineFixtureProvider(t, srv, llm.FormatOpenAI, 5*time.Second)
			ctx, stop := context.WithCancelCause(t.Context())
			defer stop(AbortPausedByUser)
			seen := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				var end error
				for ev, err := range p.Stream(ctx, llm.CompletionRequest{}) {
					if err != nil {
						end = err
						break
					}
					if ev.Type == llm.SETextDelta {
						close(seen)
						if early {
							break
						}
					}
				}
				done <- end
			}()
			select {
			case <-seen:
			case <-time.After(5 * time.Second):
				t.Fatal("fixture produced no stream prefix")
			}
			if !early {
				stop(AbortPausedByUser)
			}
			select {
			case err := <-done:
				if errors.Is(err, ErrModelCallTimeout) {
					t.Fatalf("normal exit/cancel relabeled: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stream did not finish")
			}
			select {
			case <-released:
			case <-time.After(5 * time.Second):
				t.Fatal("stream kept HTTP connection open")
			}
			awaitRequestGate(t, requestIdentity(cfg), 0, 0)
		})
	}
}
