package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/toolcall"
)

type fakeWrites struct {
	mu sync.Mutex
	w  WriteCounts
}

func TestParallelCallsInOneModelTurnCannotSkipSaveOpportunity(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	h := wrapIdleStop(nil, (&fakeWrites{}).get, nil, cancel)
	ageStopper(h.stop, time.Hour)
	id := toolcall.Identity{Version: toolcall.Version, RunID: "run", PromptID: "prompt", InvocationID: "one", ToolUseID: "provider-one", Turn: 3}
	batch := toolcall.WithInvocation(ctx, toolcall.New(id))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			if block, _, _ := h.PreToolUse(batch, "Probe", nil); !block {
				t.Error("无进展探测应被提醒拦截")
			}
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		t.Fatal("同一模型回合并发调用直接跳到终止，模型没有机会保存")
	}
	id.Turn = 4
	id.InvocationID = "next"
	id.ToolUseID = "provider-next"
	h.PreToolUse(toolcall.WithInvocation(ctx, toolcall.New(id)), "Probe", nil)
	if !errors.Is(context.Cause(ctx), AbortIdleStop) {
		t.Fatal("下一回合仍无进展时保护失效")
	}
}

func (f *fakeWrites) get() WriteCounts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.w
}
func (f *fakeWrites) addFacts(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.w.Facts += n
}

func ageStopper(s *idleStopper, elapsed time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	earlier := s.now().Add(-elapsed)
	s.start, s.lastWrite, s.lastChange = earlier, earlier, earlier
	s.rounds = s.policy.IdleRounds
}

type idleClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *idleClock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *idleClock) add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.at = c.at.Add(d) }

func useIdleClock(s *idleStopper) *idleClock {
	c := &idleClock{at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s.now = c.now
	s.start, s.lastWrite, s.lastChange = c.now(), c.now(), c.now()
	return c
}

func TestIdleStopDifferentSuccessOutputKeepsLongVerification(t *testing.T) {
	s := newIdleStopper((&fakeWrites{}).get)
	c := useIdleClock(s)
	for i := 0; i < 24; i++ {
		c.add(30 * time.Second)
		s.observe("Probe", nil, []byte(fmt.Sprintf("confirmed observation %d", i)), false)
		if stop, reason := s.peek(); stop {
			t.Fatalf("新的成功回显被当空转：%s", reason)
		}
	}
	if s.totalLocked() != 0 {
		t.Fatal("工具回显不能伪造已保存成果")
	}
	c.add(15 * time.Minute)
	if stop, reason := s.peek(); stop {
		t.Fatalf("仅未保存不能终止有效验证：reason=%s", reason)
	}
	if reminder := s.saveReminder(); reminder == "" || !strings.Contains(reminder, "不暂停") {
		t.Fatal("应给出不阻断的保存提醒")
	}
	if s.saveReminder() != "" {
		t.Fatal("同一间隔不能反复提醒")
	}
}

func TestIdleStopRepeatedAndFailedOutputAreNotProgress(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			s := newIdleStopper((&fakeWrites{}).get)
			c := useIdleClock(s)
			s.observe("Probe", nil, []byte("same"), false)
			c.add(idleStopMinElapsed)
			for i := 0; i < idleStopRounds; i++ {
				output := "same"
				if failed {
					output = fmt.Sprint("different error ", i)
				}
				s.observe("Probe", nil, []byte(output), failed)
			}
			if stop, _ := s.peek(); !stop {
				t.Fatal("重复输出或变化的错误不应延长探索")
			}
		})
	}
}

func TestIdleStopGraceAndSavedProgressReset(t *testing.T) {
	f := &fakeWrites{}
	h := wrapIdleStop(nil, f.get, nil)
	c := useIdleClock(h.stop)
	for i := 0; i < idleStopRounds+2; i++ {
		h.stop.advance()
	}
	if stop, _ := h.stop.peek(); stop {
		t.Fatal("启动宽限期内不应打断")
	}
	c.add(idleStopMinElapsed)
	if block, _, _ := h.PreToolUse(t.Context(), "Probe", nil); !block {
		t.Fatal("应先提醒保存")
	}
	f.addFacts(1)
	h.PostToolUse(t.Context(), "record_fact", nil, nil, false)
	if pending, _ := h.stop.pending(); pending {
		t.Fatal("真实写回应清除提醒")
	}
	if block, _, _ := h.PreToolUse(t.Context(), "Probe", nil); block {
		t.Fatal("已保存后不应立即再拦")
	}
}

func TestIdleStopBlockedPreToolCancelsWithoutPostTool(t *testing.T) {
	f := &fakeWrites{}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	calls := 0
	h := wrapIdleStop(nil, f.get, func(string) { calls++ }, cancel)
	ageStopper(h.stop, time.Hour)
	if block, _, _ := h.PreToolUse(ctx, "Probe", nil); !block || ctx.Err() != nil {
		t.Fatal("首次只能提醒")
	}
	if block, _, _ := h.PreToolUse(ctx, "Probe", nil); !block {
		t.Fatal("再次无进展须拦截")
	}
	if !errors.Is(context.Cause(ctx), AbortIdleStop) || calls != 1 {
		t.Fatalf("没有真实取消：cause=%v calls=%d", context.Cause(ctx), calls)
	}
	// 这里故意不调用PostToolUse，符合真实执行框架的被拦路径。
}

type blockingHook struct {
	prevent   bool
	msg       string
	stopCalls int
}

func (b *blockingHook) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (b *blockingHook) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (b *blockingHook) Stop(context.Context, []llm.Message) (bool, []string, string) {
	b.stopCalls++
	return b.prevent, nil, b.msg
}

func TestIdleStopKeepsInnerStopCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	inner := &blockingHook{prevent: true, msg: "范围拦截"}
	h := wrapIdleStop(inner, (&fakeWrites{}).get, nil, cancel)
	ageStopper(h.stop, time.Hour)
	prevent, _, msg := h.Stop(ctx, nil)
	if !prevent || msg != "范围拦截" || ctx.Err() != nil {
		t.Fatal("内层拦截不能被归为idle_stop")
	}
}

func TestIdleStopCheckpointDoesNotRelabelAnotherHookStop(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	h := wrapIdleStop(&blockingHook{prevent: true, msg: "范围拦截"}, (&fakeWrites{}).get, nil, cancel)
	ageStopper(h.stop, time.Hour)
	h.PreToolUse(ctx, "Probe", nil) // 曾有检查点提醒，不代表后续所有停止都归它。
	prevent, _, _ := h.Stop(ctx, nil)
	if !prevent {
		t.Fatal("内层钩子应该停止")
	}
	reason, err := workerIdleOutcome(t.Context(), ctx, h, harness.ReasonStopHookPrevented, nil)
	if err != nil || reason != harness.ReasonStopHookPrevented || ctx.Err() != nil {
		t.Fatalf("其他钩子停止被误归idle：reason=%s err=%v cause=%v", reason, err, context.Cause(ctx))
	}
}

func TestIdleStopParentCancellationWins(t *testing.T) {
	parent, stopParent := context.WithCancelCause(t.Context())
	run, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	h := wrapIdleStop(nil, (&fakeWrites{}).get, nil, cancel)
	ageStopper(h.stop, time.Hour)
	h.PreToolUse(run, "Probe", nil)
	stopParent(AbortPausedByUser)
	h.PreToolUse(run, "Probe", nil)
	reason, err := workerIdleOutcome(parent, run, h, harness.ReasonAbortedTools, context.Canceled)
	if errors.Is(err, AbortIdleStop) || reason == WorkerReasonIdleStop || !errors.Is(context.Cause(run), AbortPausedByUser) {
		t.Fatalf("父取消被误归类：reason=%s err=%v cause=%v", reason, err, context.Cause(run))
	}
}

func TestIdleStopRunningTaskUsesBoundedWaitWithoutInventingFacts(t *testing.T) {
	h := wrapIdleStop(nil, (&fakeWrites{}).get, nil)
	ageStopper(h.stop, time.Hour)
	h.PostToolUse(t.Context(), "TaskOutput", []byte(`{"task_id":"task_1"}`), []byte(`"[task task_1 | running]\n"`), false)
	block, _, updated := h.PreToolUse(t.Context(), "TaskOutput", []byte(`{"task_id":"task_1","block":false}`))
	if block || !strings.Contains(string(updated), `"block":true`) || !strings.Contains(string(updated), "30000") {
		t.Fatalf("后台真实等待被打断或未退避：block=%v input=%s", block, updated)
	}
	if h.stop.totalLocked() != 0 {
		t.Fatal("等待不能冒充事实")
	}
	h.PostToolUse(t.Context(), "TaskOutput", []byte(`{"task_id":"task_1"}`), []byte(`"[task task_1 | completed | exit 0]\n"`), false)
	if block, _, updated := h.PreToolUse(t.Context(), "TaskOutput", []byte(`{"task_id":"task_1"}`)); block || len(updated) > 0 {
		t.Fatal("结束回执应恢复正常工具调用，不能因旧保存间隔阻断")
	}
}

func TestIdleStopTimestampChangesDoNotConcealRepeatedOutput(t *testing.T) {
	s := newIdleStopper((&fakeWrites{}).get)
	c := useIdleClock(s)
	for i := 0; i <= idleStopRounds; i++ {
		c.add(time.Minute)
		s.observe("Bash", nil, []byte(fmt.Sprintf("2026-11-17T10:%02d:00Z\nunchanged", i)), false)
	}
	if stop, _ := s.peek(); !stop {
		t.Fatal("只有时钟变化不应重置空转计数")
	}
}

func TestIdleStopCheckpointAllowsBoundedMaterialReads(t *testing.T) {
	h := wrapIdleStop(nil, (&fakeWrites{}).get, nil)
	ageStopper(h.stop, time.Hour)
	for i := 0; i < h.stop.policy.IdleRounds; i++ {
		if block, _, _ := h.PreToolUse(t.Context(), "Read", []byte(`{"file_path":"existing.txt"}`)); block {
			t.Fatal("整理已存材料被阻断")
		}
	}
	if block, _, _ := h.PreToolUse(t.Context(), "Read", []byte(`{"file_path":"existing.txt"}`)); !block {
		t.Fatal("材料读取不能无限绕过真正空转")
	}
}
