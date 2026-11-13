package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Autumn-27/norma/toolcall"
)

// ErrToolFailureLoop distinguishes a stopped, non-progressing run from a user
// cancellation or a transient provider error. The engine must not retry it as EOF.
var ErrToolFailureLoop = errors.New("tool_failure_loop")

var ErrToolInputLoop = errors.New("tool_input_loop")
var ErrToolPolicyLoop = errors.New("tool_policy_loop")
var ErrToolPrerequisiteLoop = errors.New("tool_prerequisite_loop")
var ErrToolOutcomeUnknownLoop = errors.New("tool_outcome_unknown_loop")

// ToolLoopError preserves the compatibility sentinel while exposing a trusted
// source class. Neither target response text nor imported activity chooses it.
type ToolLoopError struct {
	Class string
	Cause error
}

func (e *ToolLoopError) Error() string {
	return e.Class + "（tool_failure_loop）：" + e.Cause.Error()
}
func (e *ToolLoopError) Unwrap() error { return e.Cause }
func (e *ToolLoopError) Is(target error) bool {
	if target == ErrToolFailureLoop {
		return true
	}
	return target == ErrToolInputLoop && e.Class == "tool_input_loop" || target == ErrToolPolicyLoop && e.Class == "tool_policy_loop" || target == ErrToolPrerequisiteLoop && e.Class == "tool_prerequisite_loop" || target == ErrToolOutcomeUnknownLoop && e.Class == "tool_outcome_unknown_loop"
}

const maxIdenticalToolFailures = 5

type failedToolCall struct {
	name string
	key  [32]byte
}

// Observe SDK result events, rather than only hooks: schema validation and
// permission failures can return before PreToolUse/PostToolUse are invoked.
type toolFailureLoop struct {
	pending            map[string]failedToolCall
	last               [32]byte
	streak             int
	schema             int
	recent             [][32]byte
	planner            bool
	errors             int
	byTool             map[string]map[[32]byte]int
	unchangedWrites    [][32]byte
	inputErrors        map[string]int
	policyErrors       map[string]int
	prerequisiteErrors map[[32]byte]int
}

func (g *toolFailureLoop) resultWithOutcome(id string, failed bool, output string, outcome *toolcall.Outcome) error {
	if outcome == nil || !outcome.Identity.Valid() {
		if err := g.result(id, failed, output); err != nil {
			return &ToolLoopError{Class: "tool_outcome_unknown_loop", Cause: err}
		}
		return nil
	}
	call, known := g.pending[id]
	switch outcome.Kind {
	case toolcall.SchemaInvalid, toolcall.UnknownTool:
		delete(g.pending, id)
		if g.inputErrors == nil {
			g.inputErrors = map[string]int{}
		}
		name := call.name
		if !known {
			name = outcome.ToolName
		}
		g.inputErrors[name]++
		unresolved := 0
		for _, count := range g.inputErrors {
			unresolved += count
		}
		if unresolved >= 12 {
			return &ToolLoopError{Class: "tool_input_loop", Cause: fmt.Errorf("本轮仍有%d次未纠正的工具/参数错误；换工具名不能代替有效调用，请核对可见工具后保留交接", unresolved)}
		}
		if g.inputErrors[name] >= maxIdenticalToolFailures {
			return &ToolLoopError{Class: "tool_input_loop", Cause: fmt.Errorf("工具%s的参数或选择已纠正%d次仍未成功；请核对真实schema或换用可见工具", name, g.inputErrors[name])}
		}
		return nil
	case toolcall.PermissionDenied, toolcall.PolicyDenied, toolcall.ApprovalRejected, toolcall.ApprovalTimeout:
		delete(g.pending, id)
		if g.policyErrors == nil {
			g.policyErrors = map[string]int{}
		}
		// The rule and operation class are host facts. Rephrasing arguments or
		// inserting unrelated successful reads cannot launder this denial budget.
		key := fmt.Sprintf("%s:%d:%s:%s", outcome.Source, outcome.RuleID, call.name, outcome.ReasonCode)
		g.policyErrors[key]++
		if g.policyErrors[key] >= 3 {
			return &ToolLoopError{Class: "tool_policy_loop", Cause: fmt.Errorf("同一管控分支重复受阻%d次，需保留该分支缺口并选择其他合法方向", g.policyErrors[key])}
		}
		return nil
	case toolcall.PreconditionMissing, toolcall.EvidenceIncomplete:
		delete(g.pending, id)
		if g.prerequisiteErrors == nil {
			g.prerequisiteErrors = map[[32]byte]int{}
		}
		if known {
			key := sha256.Sum256(append(call.key[:], []byte(outcome.ReasonCode)...))
			g.prerequisiteErrors[key]++
			if g.prerequisiteErrors[key] >= maxIdenticalToolFailures {
				return &ToolLoopError{Class: "tool_prerequisite_loop", Cause: fmt.Errorf("同一调用缺少前提或证据仍重复%d次，请保留材料并补齐具体缺口或换路", g.prerequisiteErrors[key])}
			}
		}
		return nil
	case toolcall.SchedulerDeferred, toolcall.Cancelled, toolcall.ConfiguredDeadline:
		delete(g.pending, id)
		return nil
	case toolcall.HookPrevented:
		// A hook may be an idle stop, policy, or another host. Without an
		// explicit reason do not invent policy provenance from its message.
		if err := g.resultInternal(id, failed, output, false); err != nil {
			return &ToolLoopError{Class: "tool_outcome_unknown_loop", Cause: err}
		}
		return nil
	default:
		if outcome.Kind == toolcall.ExecutedSuccess && !failed && known {
			delete(g.inputErrors, call.name)
		}
		return g.resultInternal(id, failed, output, false)
	}
}

func (g *toolFailureLoop) start(id, name string, input []byte) {
	if g.pending == nil {
		g.pending = make(map[string]failedToolCall)
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	if dec.Decode(&value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			input = canonical // stable object-key order without rounding numbers
		}
	}
	g.pending[id] = failedToolCall{name: name, key: sha256.Sum256(append([]byte(name+"\x00"), input...))}
}

func (g *toolFailureLoop) result(id string, failed bool, output string) error {
	return g.resultInternal(id, failed, output, true)
}

func (g *toolFailureLoop) resultInternal(id string, failed bool, output string, legacy bool) error {
	call, known := g.pending[id]
	delete(g.pending, id)
	if known && !failed && (call.name == "record_fact" || call.name == "save_evidence" || call.name == "record_verification") {
		key := sha256.Sum256(append(call.key[:], []byte(strings.TrimSpace(output))...))
		g.unchangedWrites = append(g.unchangedWrites, key)
		if len(g.unchangedWrites) > 6 {
			g.unchangedWrites = g.unchangedWrites[len(g.unchangedWrites)-6:]
		}
		identical := len(g.unchangedWrites) == 6
		for _, prior := range g.unchangedWrites {
			if prior != key {
				identical = false
				break
			}
		}
		if identical {
			return fmt.Errorf("%w：工具%s连续返回相同写回回执，没有新内容；已保留已有证据并停止重复提交", ErrToolFailureLoop, call.name)
		}
	} else if !failed {
		g.unchangedWrites = nil
	}
	if g.planner && known {
		if !failed {
			delete(g.byTool, call.name)
		} else {
			g.errors++
			if g.byTool == nil {
				g.byTool = map[string]map[[32]byte]int{}
			}
			if g.byTool[call.name] == nil {
				g.byTool[call.name] = map[[32]byte]int{}
			}
			key := sha256.Sum256([]byte(strings.TrimSpace(output)))
			g.byTool[call.name][key]++
			if g.byTool[call.name][key] >= 3 || g.errors >= 8 {
				return fmt.Errorf("%w：规划工具%s同类错误重复%d次，本轮累计%d次工具错误；修改参数或插入无关成功调用不能代替修复错误，请核对真实工具契约后继续", ErrToolFailureLoop, call.name, g.byTool[call.name][key], g.errors)
			}
		}
	}
	if !failed || !known {
		g.streak = 0
		g.schema = 0
		g.recent = nil
		return nil
	}
	key := sha256.Sum256(append(call.key[:], []byte(strings.TrimSpace(output))...))
	g.recent = append(g.recent, key)
	if len(g.recent) > 6 {
		g.recent = g.recent[len(g.recent)-6:]
	}
	if legacy && strings.HasPrefix(strings.TrimSpace(output), "Error: invalid tool input:") {
		g.schema++
	}
	if g.streak == 0 || key != g.last {
		g.last, g.streak = key, 1
	} else {
		g.streak++
	}
	cycle := len(g.recent) == 6 && g.recent[0] == g.recent[2] && g.recent[2] == g.recent[4] &&
		g.recent[1] == g.recent[3] && g.recent[3] == g.recent[5]
	if g.streak < maxIdenticalToolFailures && g.schema < maxIdenticalToolFailures && !cycle {
		return nil
	}
	// Do not copy arguments or tool output into the terminal error: either may
	// contain credentials or untrusted target content. The real trace has them.
	reason := fmt.Sprintf("工具 %s 以相同输入连续失败 %d 次", call.name, g.streak)
	if g.schema >= maxIdenticalToolFailures {
		reason = fmt.Sprintf("工具参数校验累计失败 %d 次", g.schema)
	} else if cycle {
		reason = "相同失败调用以 A/B 模式交替重复 3 次"
	}
	return fmt.Errorf("%w：%s，期间没有成功工具结果；本轮已停止以避免继续空转。请查看真实工具错误，修正参数或前置条件后再继续", ErrToolFailureLoop, reason)
}
