package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrPlannerReadLoop = errors.New("planner_read_loop")

// Only stable domain reads are candidates. Worker polling, shell output and
// changing graph results must not be mistaken for a non-progressing plan.
var plannerStateReads = map[string]bool{
	"graph_overview": true, "list_intents": true, "list_facts": true,
	"list_findings": true, "list_assets": true, "get_node": true,
	"list_goals": true, "get_worker_output": true,
}

type plannerReadLoop struct {
	pending map[string][32]byte
	recent  [][32]byte
}

func (g *plannerReadLoop) start(id, name string, input []byte) {
	if !plannerStateReads[name] {
		return
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	if decoder.Decode(&value) == nil {
		if raw, err := json.Marshal(value); err == nil {
			input = raw
		}
	}
	if g.pending == nil {
		g.pending = map[string][32]byte{}
	}
	g.pending[id] = sha256.Sum256(append([]byte(name+"\x00"), input...))
}

func (g *plannerReadLoop) result(id string, failed bool, output string) error {
	call, known := g.pending[id]
	delete(g.pending, id)
	if failed {
		return nil
	}
	if !known {
		g.recent = nil
		return nil
	} // successful non-read tool may have progressed
	key := sha256.Sum256(append(call[:], []byte(strings.TrimSpace(output))...))
	g.recent = append(g.recent, key)
	if len(g.recent) > 8 {
		g.recent = g.recent[len(g.recent)-8:]
	}
	identical := len(g.recent) >= 6
	if identical {
		for _, other := range g.recent[len(g.recent)-6:] {
			if other != key {
				identical = false
				break
			}
		}
	}
	cycle := len(g.recent) == 8
	if cycle {
		for i := 2; i < 8; i++ {
			if g.recent[i] != g.recent[i%2] {
				cycle = false
				break
			}
		}
	}
	if !identical && !cycle {
		return nil
	}
	return fmt.Errorf("%w：本轮反复读取相同图状态（含交替读取），未观察到新的结果或写入，已停止当前规划轮；等待任务图变化或下一次唤醒后再规划", ErrPlannerReadLoop)
}
