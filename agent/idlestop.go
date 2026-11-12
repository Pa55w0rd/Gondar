package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

// 无增量保护观察真实工具回显与已保存成果，而不是把少量未落库调用
// 当作方向已被证伪。持续有不同成功回显的验证可继续；长时间未保存仍要
// 做有限检查点，避免时间戳等无关输出让一次运行永远不结束。
const (
	idleStopRounds        = 8
	idleStopMinElapsed    = 3 * time.Minute
	idleStopSaveInterval  = 15 * time.Minute
	idleStopOutputHistory = 64
)

const WorkerReasonIdleStop harness.TerminalReason = "idle_stop"

type idleStopper struct {
	mu sync.Mutex

	writes     func() WriteCounts
	lastWrites int
	start      time.Time
	lastWrite  time.Time
	lastChange time.Time
	now        func() time.Time
	rounds     int
	nudged     bool
	reason     string
	seen       map[[32]byte]bool
	order      [][32]byte
	waiting    map[string]bool
}

func newIdleStopper(writes func() WriteCounts) *idleStopper {
	now := time.Now()
	return &idleStopper{
		writes: writes, start: now, lastWrite: now, lastChange: now,
		now: time.Now, seen: make(map[[32]byte]bool), waiting: make(map[string]bool),
	}
}

func (s *idleStopper) refreshLocked() bool {
	total := s.totalLocked()
	if total <= s.lastWrites {
		return false
	}
	s.lastWrites = total
	s.rounds, s.nudged, s.reason = 0, false, ""
	s.lastWrite, s.lastChange = s.now(), s.now()
	return true
}

func (s *idleStopper) totalLocked() int {
	if s.writes == nil {
		return 0
	}
	return s.writes().Total()
}

func (s *idleStopper) neededLocked() (bool, string) {
	now := s.now()
	if now.Sub(s.start) < idleStopMinElapsed {
		return false, ""
	}
	if now.Sub(s.lastWrite) >= idleStopSaveInterval {
		return true, "持续验证尚未保存检查点；本轮已有成果与未验证步骤需要先写回"
	}
	if s.rounds >= idleStopRounds && now.Sub(s.lastChange) >= idleStopMinElapsed {
		return true, fmt.Sprintf("连续%d次调用没有新的成功回显或已保存成果；需要保存检查点并核对阻塞", s.rounds)
	}
	return false, ""
}

func (s *idleStopper) peek() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return s.neededLocked()
}

// check 只用于没有工具执行的 Stop 路径。
func (s *idleStopper) check() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.refreshLocked() {
		s.rounds++
	}
	return s.neededLocked()
}

func (s *idleStopper) advance() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.refreshLocked() {
		s.rounds++
	}
}

// observe 只把实际成功且不同的回显作为短期探索进展。保存检查点仍有
// 独立的有界间隔；不把模型的草稿、工具参数改写或工具错误当成成果。
func (s *idleStopper) observe(name string, input, result []byte, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshLocked() {
		return
	}
	var text string
	if json.Unmarshal(result, &text) != nil {
		text = string(result)
	}
	text = strings.TrimSpace(text)
	if !failed {
		if name == "Bash" {
			if match := idleBackgroundTask.FindStringSubmatch(text); len(match) == 2 {
				s.waiting[match[1]] = true
			}
		}
		if name == "TaskOutput" {
			var in struct {
				TaskID string `json:"task_id"`
			}
			if json.Unmarshal(input, &in) == nil && in.TaskID != "" {
				s.waiting[in.TaskID] = strings.HasPrefix(text, "[task "+in.TaskID+" | running]")
			}
		}
	}
	if failed || text == "" || name == "TodoWrite" || name == "Sleep" {
		s.rounds++
		return
	}
	key := sha256.Sum256([]byte(name + "\x00" + text))
	if s.seen[key] {
		s.rounds++
		return
	}
	s.seen[key] = true
	s.order = append(s.order, key)
	if len(s.order) > idleStopOutputHistory {
		delete(s.seen, s.order[0])
		s.order = s.order[1:]
	}
	s.rounds = 0
	s.lastChange = s.now()
}

var idleBackgroundTask = regexp.MustCompile(`(?:Running in background as |moved to background as )(task_[A-Za-z0-9_-]+)`)

// A running task is real work, not a request to fabricate a fact. Poll it with
// a bounded blocking read; the configured run/task deadline remains effective.
func (s *idleStopper) waitInput(name string, input []byte) ([]byte, bool) {
	if name != "TaskOutput" {
		return nil, false
	}
	var args map[string]any
	if json.Unmarshal(input, &args) != nil {
		return nil, false
	}
	id, _ := args["task_id"].(string)
	s.mu.Lock()
	waiting := s.waiting[id]
	s.mu.Unlock()
	if !waiting {
		return nil, false
	}
	args["block"] = true
	args["timeout_ms"] = 30000
	updated, err := json.Marshal(args)
	return updated, err == nil
}

// request 返回是否已提醒过。计数独立于 PostToolUse：被 PreToolUse 拦截
// 的调用不会进入 PostToolUse，因此不能靠后置钩子推进取消阶段。
func (s *idleStopper) request(reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	already := s.nudged
	s.nudged, s.reason = true, reason
	return already
}

func (s *idleStopper) pending() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return s.nudged, s.reason
}

type idleStopHooks struct {
	inner  harness.HookRunner
	stop   *idleStopper
	onStop func(string)
	cancel func(error)
	once   sync.Once
}

func (h *idleStopHooks) finish(ctx context.Context, reason string) {
	// 父取消必须保留用户暂停、任务时限等真实原因。
	if ctx.Err() != nil {
		return
	}
	h.once.Do(func() {
		if h.onStop != nil {
			h.onStop(reason)
		}
		if h.cancel != nil {
			h.cancel(AbortIdleStop)
		}
	})
}

func (h *idleStopHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	var updatedInput []byte
	if h.inner != nil {
		if block, msg, updated := h.inner.PreToolUse(ctx, name, input); block {
			return block, msg, updated
		} else {
			updatedInput = updated
		}
	}
	if ctx.Err() != nil || idleStopWriteTools[name] {
		return false, "", updatedInput
	}
	probeInput := input
	if len(updatedInput) > 0 {
		probeInput = updatedInput
	}
	if updated, waiting := h.stop.waitInput(name, probeInput); waiting {
		return false, "", updated
	}
	needed, reason := h.stop.peek()
	if !needed {
		return false, "", updatedInput
	}
	if h.stop.request(reason) {
		h.finish(ctx, reason)
		return true, idleStopMessage + "（保护已停止本轮，已保存成果保留。）", nil
	}
	return true, idleStopMessage, nil
}

func (h *idleStopHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if h.inner != nil {
		h.inner.PostToolUse(ctx, name, input, result, isErr)
	}
	if idleStopWriteTools[name] {
		// 只有真实成功写入导致 WriteCounts 增长才会清除提醒。
		h.stop.mu.Lock()
		h.stop.refreshLocked()
		h.stop.mu.Unlock()
		return
	}
	h.stop.observe(name, input, result, isErr)
}

func (h *idleStopHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if h.inner != nil {
		if prevent, blocking, msg := h.inner.Stop(ctx, messages); prevent || len(blocking) > 0 {
			return prevent, blocking, msg
		}
	}
	if ctx.Err() != nil {
		return false, nil, ""
	}
	needed, reason := h.stop.check()
	if !needed {
		return false, nil, ""
	}
	if !h.stop.request(reason) {
		return false, []string{idleStopMessage}, ""
	}
	h.finish(ctx, reason)
	return true, nil, reason
}

func wrapIdleStop(inner harness.HookRunner, writes func() WriteCounts, onStop func(string), cancel ...func(error)) *idleStopHooks {
	h := &idleStopHooks{inner: inner, stop: newIdleStopper(writes), onStop: onStop}
	if len(cancel) > 0 {
		h.cancel = cancel[0]
	}
	return h
}

func workerIdleOutcome(parentCtx, runCtx context.Context, hooks *idleStopHooks, reason harness.TerminalReason, err error) (harness.TerminalReason, error) {
	if parentCtx.Err() != nil {
		return reason, parentCtx.Err()
	}
	// The SDK can naturally end after its first nudge without a second Stop call.
	if err == nil && reason == harness.ReasonCompleted {
		if pending, detail := hooks.stop.pending(); pending {
			hooks.finish(runCtx, detail)
		}
	}
	if errors.Is(context.Cause(runCtx), AbortIdleStop) {
		return WorkerReasonIdleStop, AbortIdleStop
	}
	return reason, err
}

var idleStopWriteTools = map[string]bool{
	"record_fact": true, "insert_assets": true, "report_finding": true,
	"save_evidence": true, "register_resource": true, "update_resource": true,
	"record_verification": true,
}

const idleStopMessage = "本轮需要保存检查点：近期没有新的成功回显或已到保存间隔，这不表示该方向已安全或没有价值。请停止新的探测，把已确认观察、负结果及未完成步骤用 record_fact 保存，确认漏洞用 report_finding，已有证据用 save_evidence；随后说明具体阻塞或可续探步骤。不要为通过保护编造事实。保存真实新增后可继续本意图；没有新增则结束本轮，保留未验证范围。"

func logIdleStop(worker string, taskID, intentID int64, reason string) {
	log.Printf("[worker %s] task %d intent #%d 无增量检查点保护: %s", worker, taskID, intentID, reason)
}

func summarizeWriteDelta(cur, prev WriteCounts) string {
	return fmt.Sprintf("事实+%d 资产+%d 漏洞+%d 附件+%d",
		cur.Facts-prev.Facts, cur.Assets-prev.Assets, cur.Findings-prev.Findings, cur.Artifacts-prev.Artifacts)
}

func idleStopStateText(reason string) string { return strings.TrimSpace(reason) }
