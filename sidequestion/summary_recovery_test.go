package sidequestion

import (
	"context"
	"errors"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestSummaryIncompleteRetriesOnceWithoutThinkingOverride(t *testing.T) {
	for _, stop := range []string{"end_turn", "max_tokens", "length"} {
		t.Run(stop, func(t *testing.T) {
			calls := 0
			p := fakeProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if req.Thinking != "" {
					t.Fatal("summary overrode thinking")
				}
				if calls == 1 {
					return assistant(""), stop, llm.Usage{InputTokens: 3}, nil
				}
				return assistant("synthetic verified summary"), "end_turn", llm.Usage{InputTokens: 5}, nil
			}}
			b := contextBuilder{service: SideQuestionService{p}, window: 200000}
			out, err := b.summarize(t.Context(), "", "synthetic source")
			if err != nil || out != "synthetic verified summary" || calls != 2 || b.usage.InputTokens != 8 {
				t.Fatalf("recovery: %q %d %+v %v", out, calls, b.usage, err)
			}
		})
	}
}

func TestSummaryRecoveryKeepsGlobalCallCapAndCancellation(t *testing.T) {
	calls := 0
	p := fakeProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return assistant(""), "max_tokens", llm.Usage{}, nil
	}}
	b := contextBuilder{service: SideQuestionService{p}, window: 200000, calls: 11}
	if _, err := b.summarize(t.Context(), "", "synthetic"); err == nil || calls != 1 || b.calls != 12 {
		t.Fatalf("cap: %d %d %v", calls, b.calls, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := b.summarize(ctx, "", "synthetic"); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("canceled summary retried: %v %d", err, calls)
	}
}
