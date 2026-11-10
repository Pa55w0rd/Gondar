package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrToolFailureLoop distinguishes a stopped, non-progressing run from a user
// cancellation or a transient provider error. The engine must not retry it as EOF.
var ErrToolFailureLoop = errors.New("tool_failure_loop")

const maxIdenticalToolFailures = 5

type failedToolCall struct {
	name string
	key  [32]byte
}

// Observe SDK result events, rather than only hooks: schema validation and
// permission failures can return before PreToolUse/PostToolUse are invoked.
type toolFailureLoop struct {
	pending map[string]failedToolCall
	last    [32]byte
	streak  int
	schema  int
	recent  [][32]byte
	planner bool
	errors  int
	byTool  map[string]map[[32]byte]int
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
	call, known := g.pending[id]
	delete(g.pending, id)
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
	if strings.HasPrefix(strings.TrimSpace(output), "Error: invalid tool input:") {
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
