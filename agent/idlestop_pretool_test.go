package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"gondar/db"
)

func TestIdleStopRealHarnessWaitsForKnownBackgroundTask(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	writes := &fakeWrites{}
	h := wrapIdleStop(nil, writes.get, nil, cancel)
	clock := useIdleClock(h.stop)
	calls, polls := 0, 0
	launch := readTool("Bash", "启动本地合成作业", obj(nil), func(context.Context, json.RawMessage) (actool.Result, error) {
		return actool.Text("Running in background as task_1. Output file: local-fixture"), nil
	})
	output := readTool("TaskOutput", "读取本地合成作业状态", obj(map[string]any{
		"task_id": str("任务标识"), "block": map[string]any{"type": "boolean"}, "timeout_ms": intp("等待毫秒"),
	}), func(_ context.Context, input json.RawMessage) (actool.Result, error) {
		var in struct {
			Block   bool `json:"block"`
			Timeout int  `json:"timeout_ms"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			t.Fatal(err)
		}
		if !in.Block || in.Timeout != 30000 {
			t.Fatalf("真实执行框架没有应用阻塞等待参数：%s", input)
		}
		polls++
		clock.add(time.Hour)
		if polls < 12 {
			return actool.Text("[task task_1 | running]\n"), nil
		}
		return actool.Text("[task task_1 | completed | exit 0]\n真实合成结果"), nil
	})
	save := writeTool("record_fact", "保存已完成作业观察", obj(nil), func(context.Context, json.RawMessage) (actool.Result, error) {
		if polls != 12 {
			t.Fatal("未完成作业时不能虚构事实")
		}
		writes.addFacts(1)
		return actool.Text("已保存作业结果"), nil
	})
	p := plannerStateProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls == 1 {
			return idleToolCall(calls, "Bash", "{}")
		}
		if polls < 12 {
			clock.add(time.Hour)
			return idleToolCall(calls, "TaskOutput", `{"task_id":"task_1","block":false}`)
		}
		if writes.get().Facts == 0 {
			return idleToolCall(calls, "record_fact", "{}")
		}
		return idleAssistantText("已完成并保存"), "end_turn", llm.Usage{}, nil
	}}
	// 保留真实SDK执行与hook路径，仅避免内置同名TaskOutput覆盖合成工具。
	_, reason, err := captureRun(ctx, agentcore.Options{Provider: p, NonStreaming: true, Hooks: h,
		Tools: []actool.CoreTool{launch, output, save}, DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, WorkingDir: t.TempDir(), MaxTurns: 24}, "只运行合成后台状态", nil)
	reason, err = workerIdleOutcome(t.Context(), ctx, h, reason, err)
	if err != nil || reason != harness.ReasonCompleted || polls != 12 || writes.get().Facts != 1 {
		t.Fatalf("合法后台等待被保护误杀：reason=%s err=%v polls=%d facts=%d", reason, err, polls, writes.get().Facts)
	}
}

func idleToolCall(call int, name, input string) (llm.Message, string, llm.Usage, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
		Type: llm.BlockToolUse, ID: fmt.Sprintf("idle-%d", call), Name: name, Input: json.RawMessage(input),
	}}}, "tool_use", llm.Usage{}, nil
}

func idleAssistantText(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(text)}}
}

// These regressions use the real SDK execution path. In particular, blocked
// PreToolUse calls must never be simulated with a manual PostToolUse callback.
func TestIdleStopRealHarnessCancelsOrSavesCheckpoint(t *testing.T) {
	for _, mode := range []string{"cancel", "save", "parent_cancel", "natural_stop"} {
		t.Run(mode, func(t *testing.T) {
			parent, stopParent := context.WithCancelCause(t.Context())
			defer stopParent(nil)
			run, cancel := context.WithCancelCause(parent)
			defer cancel(nil)
			writes := &fakeWrites{}
			writes.addFacts(1) // 已有成果不能因保护被清空。
			stopCalls := 0
			h := wrapIdleStop(nil, writes.get, func(string) { stopCalls++ }, cancel)
			clock := useIdleClock(h.stop)
			clock.add(4 * time.Minute)
			actualProbes, providerCalls, blockedResults := 0, 0, 0
			saved, afterSave := false, false
			probe := readTool("Probe", "本地固定回显", obj(nil), func(context.Context, json.RawMessage) (actool.Result, error) {
				actualProbes++
				clock.add(time.Minute)
				return actool.Text("unchanged observation"), nil
			})
			save := writeTool("record_fact", "保存本地合成检查点", obj(nil), func(context.Context, json.RawMessage) (actool.Result, error) {
				writes.addFacts(1)
				saved = true
				return actool.Text("fact saved"), nil
			})
			provider := plannerStateProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				providerCalls++
				pending, _ := h.stop.pending()
				if pending {
					switch mode {
					case "save":
						return idleToolCall(providerCalls, "record_fact", "{}")
					case "parent_cancel":
						stopParent(AbortPausedByUser)
						return idleAssistantText("人工暂停"), "end_turn", llm.Usage{}, nil
					case "natural_stop":
						return idleAssistantText("已有成果保留，待核验方向未完成"), "end_turn", llm.Usage{}, nil
					}
				}
				if saved {
					if afterSave {
						return idleAssistantText("检查点已保存"), "end_turn", llm.Usage{}, nil
					}
					afterSave = true
				}
				return idleToolCall(providerCalls, "Probe", "{}")
			}}
			_, reason, err := captureRun(run, agentcore.Options{
				Provider: provider, NonStreaming: true, Hooks: h,
				Tools: []actool.CoreTool{probe, save}, PermissionMode: permission.ModeBypass,
				WorkingDir: t.TempDir(), MaxTurns: 40,
			}, "只执行本地合成工具", func(a db.Activity) {
				if a.Kind == "tool_result" && strings.Contains(a.Detail, idleStopMessage) {
					blockedResults++
				}
			})
			reason, err = workerIdleOutcome(parent, run, h, reason, err)
			if writes.get().Facts < 1 {
				t.Fatal("已有成果丢失")
			}
			switch mode {
			case "cancel", "natural_stop":
				if !errors.Is(err, AbortIdleStop) || reason != WorkerReasonIdleStop || stopCalls != 1 {
					t.Fatalf("未返回专用终态：reason=%s err=%v stop=%d", reason, err, stopCalls)
				}
				if actualProbes != idleStopRounds+1 || blockedResults < 1 {
					t.Fatalf("未走真实拦截路径：probes=%d blocked=%d calls=%d", actualProbes, blockedResults, providerCalls)
				}
			case "save":
				if err != nil || reason != harness.ReasonCompleted || writes.get().Facts != 2 || stopCalls != 0 || actualProbes != idleStopRounds+2 {
					t.Fatalf("真实写回后未能继续：reason=%s err=%v writes=%+v probes=%d", reason, err, writes.get(), actualProbes)
				}
			case "parent_cancel":
				if errors.Is(err, AbortIdleStop) || reason == WorkerReasonIdleStop || !errors.Is(context.Cause(run), AbortPausedByUser) || stopCalls != 0 {
					t.Fatalf("人工取消误分类：reason=%s err=%v cause=%v", reason, err, context.Cause(run))
				}
			}
		})
	}
}

func TestIdleStopRealHarnessAllowsChangingOutputWithoutFacts(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	writes := &fakeWrites{}
	h := wrapIdleStop(nil, writes.get, nil, cancel)
	clock := useIdleClock(h.stop)
	actual, calls := 0, 0
	probe := readTool("Probe", "本地分步验证", obj(nil), func(context.Context, json.RawMessage) (actool.Result, error) {
		actual++
		clock.add(30 * time.Second)
		return actool.Text(fmt.Sprintf("new observation %d", actual)), nil
	})
	p := plannerStateProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls <= 20 {
			return idleToolCall(calls, "Probe", "{}")
		}
		return idleAssistantText("合成验证结束"), "end_turn", llm.Usage{}, nil
	}}
	_, reason, err := captureRun(ctx, agentcore.Options{Provider: p, NonStreaming: true, Hooks: h,
		Tools: []actool.CoreTool{probe}, PermissionMode: permission.ModeBypass, WorkingDir: t.TempDir(), MaxTurns: 30}, "本地验证", nil)
	reason, err = workerIdleOutcome(t.Context(), ctx, h, reason, err)
	if err != nil || reason != harness.ReasonCompleted || actual != 20 || writes.get().Total() != 0 {
		t.Fatalf("长验证被写回节奏误杀：reason=%s err=%v calls=%d writes=%+v", reason, err, actual, writes.get())
	}
}
