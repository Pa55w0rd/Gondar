package agent

import (
	"context"
	"fmt"
	"iter"
	"time"

	"github.com/Autumn-27/norma/llm"
)

const defaultModelCallTimeout = 10 * time.Minute

// Unlike an EOF, a completed call deadline is not permission to replay a whole
// worker after a partial response may already have dispatched tools.
var ErrModelCallTimeout = fmt.Errorf("model_call_timeout: %w", context.DeadlineExceeded)

// A task budget expiring is an expected transition to settlement, not evidence
// that the model backend failed. Keep it distinct from the logical call limit.
var ErrTaskBudgetExpired = fmt.Errorf("task_deadline_reached：任务总时限已到，进入收尾: %w", context.DeadlineExceeded)

type ModelCallTimeoutError struct{ Limit, Elapsed time.Duration }

func (e *ModelCallTimeoutError) Error() string {
	return fmt.Sprintf("model_call_timeout：本次模型调用整体期限已到（上限%s，已耗时%s）", e.Limit, e.Elapsed.Round(time.Millisecond))
}
func (e *ModelCallTimeoutError) Unwrap() error { return ErrModelCallTimeout }

type modelCallLimitKey struct{}

// A run's configured budget supplies the single model call limit as well. This is not a
// second absolute run timer: the SDK still owns its run clock and settlement.
func withModelCallLimit(ctx context.Context, limit time.Duration) context.Context {
	if limit <= 0 {
		return ctx
	}
	if previous, ok := ctx.Value(modelCallLimitKey{}).(time.Duration); ok && previous > 0 && previous < limit {
		limit = previous
	}
	return context.WithValue(ctx, modelCallLimitKey{}, limit)
}

type modelCallTimeoutProvider struct {
	inner   llm.Provider
	timeout time.Duration
}

func modelCallContext(parent context.Context, limit time.Duration) (context.Context, context.CancelFunc, time.Duration) {
	if limit <= 0 {
		limit = defaultModelCallTimeout
	}
	// Explicit agent configuration replaces the fallback, including values above
	// ten minutes. The live parent/task deadline still bounds the whole run.
	if run, ok := parent.Value(modelCallLimitKey{}).(time.Duration); ok && run > 0 {
		limit = run
	}
	deadline := time.Now().Add(limit)
	cause := ErrModelCallTimeout
	clock := taskClockFrom(parent)
	// Final settlement has its own live parent deadline. Reusing the already
	// expired task-clock value would prevent the engine's explicit final round.
	if clock.DeadlineUnix > 0 && !clock.Final {
		if taskEnd := time.Unix(clock.DeadlineUnix, 0); taskEnd.Before(deadline) {
			deadline = taskEnd
			cause = ErrTaskBudgetExpired
		}
	}
	if parentEnd, ok := parent.Deadline(); ok && !parentEnd.After(deadline) {
		// Let the parent's own timer/cause win; scheduling an equal local timer
		// could otherwise mislabel a user/run deadline as our call deadline.
		ctx, cancel := context.WithCancel(parent)
		return ctx, cancel, time.Until(parentEnd)
	}
	actual := time.Until(deadline)
	if actual < 0 {
		actual = 0
	}
	ctx, cancel := context.WithDeadlineCause(parent, deadline, cause)
	return ctx, cancel, actual
}

func modelCallError(parent, call context.Context, err error, limit time.Duration, start time.Time) error {
	if parent.Err() != nil {
		if err != nil {
			return err
		}
		return parent.Err()
	}
	if call.Err() != nil && context.Cause(call) == ErrModelCallTimeout {
		return &ModelCallTimeoutError{Limit: limit, Elapsed: time.Since(start)}
	}
	if call.Err() != nil && context.Cause(call) == ErrTaskBudgetExpired {
		return ErrTaskBudgetExpired
	}
	return err
}

func (p *modelCallTimeoutProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	call, cancel, limit := modelCallContext(ctx, p.timeout)
	defer cancel()
	start := time.Now()
	msg, stop, usage, err := p.inner.Complete(call, req)
	// Preserve any values the provider did return on error. Do not manufacture
	// usage from unfinished JSON, or erase valid usage as the source wrapper did.
	return msg, stop, usage, modelCallError(ctx, call, err, limit, start)
}

func (p *modelCallTimeoutProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		call, cancel, limit := modelCallContext(ctx, p.timeout)
		defer cancel() // early consumer exit also closes the HTTP stream
		start := time.Now()
		for event, err := range p.inner.Stream(call, req) {
			if err != nil {
				yield(event, modelCallError(ctx, call, err, limit, start))
				return
			}
			if !yield(event, nil) {
				return
			}
		}
		if err := modelCallError(ctx, call, nil, limit, start); err != nil {
			yield(llm.StreamEvent{}, err)
		}
	}
}
