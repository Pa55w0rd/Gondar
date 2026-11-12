package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"gondar/llmrec"
)

type integrityOneByteReader struct{ raw []byte }

func (r *integrityOneByteReader) Read(p []byte) (int, error) {
	if len(r.raw) == 0 {
		return 0, io.EOF
	}
	p[0], r.raw = r.raw[0], r.raw[1:]
	return 1, nil
}

func TestOpenAIStreamTransportUTF8AndRawCapture(t *testing.T) {
	raw := integrityFrame(map[string]any{"content": "观察🧭：完整多字节文本"}, nil) +
		integrityFrame(map[string]any{}, "stop") + integrityUsage(11, 7, 3) + "data: [DONE]\n\n"
	var normalized bytes.Buffer
	if err := normalizeOpenAIStream(t.Context(), &integrityOneByteReader{raw: []byte(raw)}, &normalized); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(normalized.String(), "观察🧭：完整多字节文本") {
		t.Fatal("UTF-8 was corrupted across byte boundaries")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, raw)
	}))
	defer server.Close()
	provider := integrityProvider(t, server)
	ctx, capture := llmrec.NewCapture(t.Context())
	var text strings.Builder
	var usage llm.Usage
	for event, err := range provider.Stream(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("fixture")}}) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == llm.SETextDelta {
			text.WriteString(event.Text)
		}
		if event.Type == llm.SEMessageDelta {
			usage = event.Usage
		}
	}
	if text.String() != "观察🧭：完整多字节文本" || usage.InputTokens != 11 || usage.OutputTokens != 7 || usage.CacheReadTokens != 3 {
		t.Fatalf("provider projection lost text/usage: %q %+v", text.String(), usage)
	}
	if capture.RawResponse() != raw {
		t.Fatal("normalization replaced the original recorded wire bytes")
	}
}

func TestOpenAIStreamTransportRejectsUnfinishedOrAmbiguousTools(t *testing.T) {
	finish := integrityFrame(map[string]any{}, "tool_calls")
	done := "data: [DONE]\n\n"
	for name, raw := range map[string]string{
		"missing finish":   integrityToolFrame(0, "a", "count_a", `{"label":"a"}`) + done,
		"unfinished JSON":  integrityToolFrame(0, "a", "count_a", `{"label":`) + finish + done,
		"empty JSON":       integrityToolFrame(0, "a", "count_a", ``) + finish + done,
		"duplicate ID":     integrityToolFrame(0, "a", "count_a", `{}`) + integrityToolFrame(1, "a", "count_b", `{}`) + finish + done,
		"truncated frame":  strings.TrimSuffix(integrityToolFrame(0, "a", "count_a", `{}`), "\n"),
		"truncated reason": integrityToolFrame(0, "a", "count_a", `{}`) + integrityFrame(map[string]any{}, "length") + done,
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := normalizeOpenAIStream(t.Context(), strings.NewReader(raw), &output); err == nil {
				t.Fatal("unfinished/ambiguous tool was accepted")
			}
			if strings.Contains(output.String(), `"tool_calls"`) {
				t.Fatal("a partial tool escaped before full validation")
			}
		})
	}
	var valid bytes.Buffer
	if err := normalizeOpenAIStream(t.Context(), strings.NewReader(integrityToolFrame(0, "a", "count_a", `{}`)+finish), &valid); err != nil {
		t.Fatalf("complete finish followed by clean EOF must remain compatible: %v", err)
	}
	if !strings.Contains(valid.String(), `"tool_calls"`) {
		t.Fatal("a complete empty-object call was lost")
	}
}

func TestOpenAIStreamTransportBoundsFramesAndArguments(t *testing.T) {
	for name, raw := range map[string]string{
		"frame":        "data: " + strings.Repeat("x", maxOpenAIStreamFrameBytes) + "\n\n",
		"arguments":    integrityToolFrame(0, "a", "count_a", strings.Repeat("x", 900000)) + strings.Repeat(integrityToolFrame(0, "", "", strings.Repeat("x", 900000)), 10),
		"choice index": "data: {\"choices\":[{\"index\":16,\"delta\":{}}]}\n\n",
		"tool index":   integrityToolFrame(256, "a", "count_a", `{}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := normalizeOpenAIStream(t.Context(), strings.NewReader(raw), io.Discard); err == nil {
				t.Fatal("unbounded stream accepted")
			}
		})
	}
}

type integrityBlockingBody struct {
	started, closed      chan struct{}
	startOnce, closeOnce sync.Once
	closes               atomic.Int64
}

func (b *integrityBlockingBody) Read([]byte) (int, error) {
	b.startOnce.Do(func() { close(b.started) })
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *integrityBlockingBody) Close() error {
	b.closeOnce.Do(func() { b.closes.Add(1); close(b.closed) })
	return nil
}

func TestOpenAIStreamTransportCancellationAndCloseRelease(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "early_close", true: "context_cancel"}[cancelFirst], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			upstream := &integrityBlockingBody{started: make(chan struct{}), closed: make(chan struct{})}
			body := newOpenAIStreamBody(ctx, upstream)
			select {
			case <-upstream.started:
			case <-time.After(5 * time.Second):
				t.Fatal("fixture did not start")
			}
			if cancelFirst {
				cancel()
			}
			closed := make(chan struct{})
			go func() { _ = body.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("Close left upstream/pipe blocked")
			}
			if upstream.closes.Load() != 1 {
				t.Fatalf("upstream closed %d times", upstream.closes.Load())
			}
			select {
			case <-body.done:
			default:
				t.Fatal("pump still running after Close")
			}
			select {
			case <-body.watchDone:
			default:
				t.Fatal("cancellation watcher still running after Close")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	upstream := &integrityBlockingBody{started: make(chan struct{}), closed: make(chan struct{})}
	body := newOpenAIStreamBody(ctx, upstream)
	cancel()
	_, err := io.ReadAll(body)
	if !errors.Is(err, context.Canceled) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("cancellation disappeared: %v", err)
	}
	_ = body.Close()
	// A consumer can stop before reading any projected frame. Closing only the
	// network body would leave the pump blocked on its pipe writer.
	backpressured := newOpenAIStreamBody(t.Context(), io.NopCloser(strings.NewReader(
		integrityFrame(map[string]any{"content": "消费者提前离开"}, nil))))
	closed := make(chan struct{})
	go func() { _ = backpressured.Close(); _ = backpressured.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("unread projected body was not released")
	}
}

type integrityTransportFunc func(*http.Request) (*http.Response, error)

func (f integrityTransportFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOpenAIStreamTransportLeavesOtherResponsesUntouched(t *testing.T) {
	for _, fixture := range []struct{ method, path, contentType string }{
		{http.MethodPost, "/chat/completions", "application/json"},
		{http.MethodGet, "/chat/completions", "text/event-stream"},
		{http.MethodPost, "/messages", "text/event-stream"},
	} {
		req, err := http.NewRequest(fixture.method, "http://127.0.0.1"+fixture.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		original := io.NopCloser(strings.NewReader(`{}`))
		transport := openAIStreamTransport{base: integrityTransportFunc(func(got *http.Request) (*http.Response, error) {
			if got != req {
				t.Error("request was replaced")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{fixture.contentType}}, Body: original, ContentLength: 2}, nil
		})}
		response, err := transport.RoundTrip(req)
		if err != nil || response.Body != original || response.ContentLength != 2 {
			t.Fatalf("non-applicable response changed: %+v %v", response, err)
		}
		_ = response.Body.Close()
	}
}

func TestOpenAIStreamTransportKeepsChoiceArgumentsSeparate(t *testing.T) {
	raw := `data: {"choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"id":"alternative","function":{"name":"count_b","arguments":"{}"}}]}},{"index":0,"delta":{"tool_calls":[{"index":0,"id":"primary","function":{"name":"count_a","arguments":"{}"}}]}}]}` + "\n\n" +
		integrityFrame(map[string]any{}, "tool_calls") + "data: [DONE]\n\n"
	var output bytes.Buffer
	if err := normalizeOpenAIStream(t.Context(), strings.NewReader(raw), &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "alternative") || !strings.Contains(output.String(), "primary") {
		t.Fatalf("alternative candidate was mixed into primary execution: %s", output.String())
	}
}
