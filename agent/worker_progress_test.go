package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

func TestSaveReminderRealSessionNeverBlocksLongChangingWork(t *testing.T) {
	h := wrapIdleStop(nil, (&fakeWrites{}).get, nil)
	clock := useIdleClock(h.stop)
	probes, reminders := 0, 0
	p := plannerStateProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if len(req.Messages) > 0 && strings.Contains(req.Messages[len(req.Messages)-1].Text(), "【保存提醒】") {
			reminders++
		}
		if probes < 40 {
			return idleToolCall(probes+1, "Bash", "{}")
		}
		return idleAssistantText("本地合成操作完成"), "end_turn", llm.Usage{}, nil
	}}
	probe := readTool("Bash", "本地合成回执", obj(nil), func(context.Context, json.RawMessage) (actool.Result, error) {
		probes++
		clock.add(time.Minute)
		return actool.Text(fmt.Sprintf("distinct local observation %d", probes)), nil
	})
	provider := &progressReminderProvider{inner: p, stop: h.stop}
	_, reason, err := captureRun(t.Context(), agentcore.Options{Provider: provider, NonStreaming: true, Hooks: h, Tools: []actool.CoreTool{probe}, DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, WorkingDir: t.TempDir(), MaxTurns: 50}, "仅本地合成工具", nil)
	if err != nil || reason != harness.ReasonCompleted || probes != 40 || reminders != 2 {
		t.Fatalf("软提醒中断实际执行：reason=%s err=%v probes=%d reminders=%d", reason, err, probes, reminders)
	}
}

func TestProgressPolicyZeroDisablesOnlyReminders(t *testing.T) {
	h := wrapIdleStop(nil, (&fakeWrites{}).get, nil)
	h.stop.policy = WorkerProgressPolicy{IdleRounds: 3, IdleSeconds: 60}
	clock := useIdleClock(h.stop)
	clock.add(time.Hour)
	if h.stop.saveReminder() != "" {
		t.Fatal("关闭提醒仍注入内容")
	}
	for i := 0; i < 4; i++ {
		h.stop.observe("Probe", nil, []byte("same"), false)
	}
	clock.add(time.Minute)
	if stop, _ := h.stop.peek(); !stop {
		t.Fatal("关闭提醒不能关闭防空转")
	}
}

func TestFailedOrIdempotentSavesCannotEvadeIdleProtection(t *testing.T) {
	h := wrapIdleStop(nil, (&fakeWrites{}).get, nil)
	clock := useIdleClock(h.stop)
	for i := 0; i < idleStopRounds; i++ {
		clock.add(time.Minute)
		h.PostToolUse(t.Context(), "record_fact", nil, []byte(`"already exists"`), false)
	}
	if block, _, _ := h.PreToolUse(t.Context(), "record_fact", nil); !block {
		t.Fatal("反复无新增保存不能绕过防空转")
	}
}

func TestSubstantiveProgressResetsOldIdempotentSaveAttempts(t *testing.T) {
	h := wrapIdleStop(nil, (&fakeWrites{}).get, nil)
	clock := useIdleClock(h.stop)
	for i := 0; i < idleStopRounds; i++ {
		h.PostToolUse(t.Context(), "record_fact", nil, []byte(`"already exists"`), false)
	}
	h.PostToolUse(t.Context(), "Probe", nil, []byte(`"new observation"`), false)
	clock.add(time.Hour)
	for i := 0; i < idleStopRounds; i++ {
		h.PostToolUse(t.Context(), "Probe", nil, []byte(`"new observation"`), false)
	}
	if block, _, _ := h.PreToolUse(t.Context(), "save_evidence", nil); block {
		t.Fatal("旧幂等计数阻断了新一轮材料保存")
	}
}

func TestChangingSleepReceiptsNeverCountAsExplorationProgress(t *testing.T) {
	s := newIdleStopper((&fakeWrites{}).get)
	clock := useIdleClock(s)
	for i := 0; i < idleStopRounds+1; i++ {
		clock.add(time.Minute)
		s.observe("sleep", nil, []byte(fmt.Sprintf("slept %ds", i+1)), false)
	}
	if stop, _ := s.peek(); !stop {
		t.Fatal("sleep回显变化不能掩盖空转")
	}
}

func TestSaveReminderDoesNotLeakIntoToollessCompactionRequest(t *testing.T) {
	s := newIdleStopper((&fakeWrites{}).get)
	clock := useIdleClock(s)
	clock.add(20 * time.Minute)
	p := &progressReminderProvider{stop: s}
	if got := p.request(llm.CompletionRequest{Messages: []llm.Message{llm.UserText("summarize existing context")}}); len(got.Messages) != 1 {
		t.Fatal("保存提醒污染无工具的摘要请求")
	}
	if s.saveReminder() == "" {
		t.Fatal("辅助调用提前消耗了主会话提醒")
	}
}
