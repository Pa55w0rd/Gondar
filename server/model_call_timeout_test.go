package server

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"gondar/agent"
)

func TestModelCallDeadlineDoesNotReplayAndRetainsUsage(t *testing.T) {
	deadline := &agent.ModelCallTimeoutError{Limit: time.Second, Elapsed: time.Second}
	for _, text := range []bool{false, true} {
		p := &scriptedLLMProvider{events: []llm.StreamEvent{{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 17}}}, err: deadline}
		if text {
			p.events = append(p.events, llm.StreamEvent{Type: llm.SETextDelta, Text: "already delivered"})
		}
		selectProvider := func() (taskLLMSelection, error) {
			return taskLLMSelection{provider: p, profileID: 5, retry: agent.RetryConfig{StreamAttempts: 5, StreamInterval: time.Nanosecond}}, nil
		}
		var returned error
		usage := 0
		for event, err := range streamTaskLLM(t.Context(), "synthetic", llm.CompletionRequest{}, taskLLMStreamHooks{current: selectProvider}) {
			if err != nil {
				returned = err
			} else {
				usage += event.Usage.InputTokens
			}
		}
		if !errors.Is(returned, agent.ErrModelCallTimeout) || p.calls != 1 || usage != 17 {
			t.Fatalf("stream text=%v calls=%d usage=%d err=%v", text, p.calls, usage, returned)
		}
	}
	calls := 0
	want := llm.Usage{InputTokens: 19, OutputTokens: 3}
	p := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("partial")}}, "partial", want, deadline
	}}
	msg, stop, usage, err := completeTaskLLM(t.Context(), "synthetic", llm.CompletionRequest{}, taskLLMStreamHooks{current: func() (taskLLMSelection, error) {
		return taskLLMSelection{provider: p, retry: agent.RetryConfig{StreamAttempts: 5, StreamInterval: time.Nanosecond}}, nil
	}})
	if calls != 1 || !errors.Is(err, agent.ErrModelCallTimeout) || usage != want || stop != "partial" || len(msg.Content) != 1 {
		t.Fatalf("complete replay/values changed: calls=%d stop=%s usage=%+v err=%v", calls, stop, usage, err)
	}
	if retryableWorkerModelError(harness.ReasonModelError, deadline) || isRetryableStreamError(deadline) {
		t.Fatal("call deadline enables automatic replay")
	}
	if !retryableWorkerModelError(harness.ReasonModelError, io.ErrUnexpectedEOF) || !isRetryableStreamError(io.ErrUnexpectedEOF) {
		t.Fatal("unrelated existing EOF retry policy changed")
	}
}
