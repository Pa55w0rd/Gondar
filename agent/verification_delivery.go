package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

var ErrVerificationDelivery = errors.New("verification_delivery_gap")

// Track only attempted verification writes. A worker doing legitimate reads or
// idempotent work is not required to manufacture a new fact to finish.
type verificationDeliveryGuard struct {
	inner      harness.HookRunner
	mu         sync.Mutex
	pending    map[string]string
	receipts   map[string]verificationReceipt
	correcting bool
}

type verificationReceipt struct {
	Saved    bool   `json:"saved"`
	ID       string `json:"id"`
	State    string `json:"state"`
	NodeID   int64  `json:"node_id"`
	Revision int64  `json:"revision"`
}

func verificationDeliveryKey(input []byte) string {
	var v struct {
		Target json.RawMessage `json:"target"`
	}
	if json.Unmarshal(input, &v) != nil || len(v.Target) == 0 {
		return "invalid"
	}
	var target map[string]any
	if json.Unmarshal(v.Target, &target) != nil {
		return "invalid"
	}
	if target["variant"] == nil || target["variant"] == "" {
		target["variant"] = "default"
	}
	b, _ := json.Marshal(target)
	return string(b)
}

func (g *verificationDeliveryGuard) PreToolUse(ctx context.Context, name string, in []byte) (bool, string, []byte) {
	g.mu.Lock()
	correcting := g.correcting
	g.mu.Unlock()
	if correcting && !plannerCheckpointTools[name] {
		return true, "正在核对保存回执：只读取已有材料和修正记账，不重新访问目标或重放已执行操作。", nil
	}
	if g.inner != nil {
		return g.inner.PreToolUse(ctx, name, in)
	}
	return false, "", nil
}

func (g *verificationDeliveryGuard) PostToolUse(ctx context.Context, name string, in, out []byte, failed bool) {
	if g.inner != nil {
		g.inner.PostToolUse(ctx, name, in, out, failed)
	}
	if name != "record_verification" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending == nil {
		g.pending = map[string]string{}
		g.receipts = map[string]verificationReceipt{}
	}
	key := verificationDeliveryKey(in)
	// Norma's hook contract JSON-encodes Flatten(), so the payload is a JSON
	// string containing the tool's JSON receipt, not the receipt object itself.
	var flat string
	if json.Unmarshal(out, &flat) == nil {
		out = []byte(flat)
	}
	var receipt verificationReceipt
	_ = json.Unmarshal(out, &receipt)
	if !failed && receipt.Saved && receipt.ID != "" {
		delete(g.pending, key)
		delete(g.pending, "invalid")
		g.receipts[receipt.ID] = receipt
		return
	}
	message := strings.TrimSpace(string(out))
	if len(message) > 700 {
		message = message[:700]
	}
	g.pending[key] = message
}

func (g *verificationDeliveryGuard) pendingCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pending)
}

func (g *verificationDeliveryGuard) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if g.inner != nil {
		if stop, blocking, text := g.inner.Stop(ctx, messages); stop || len(blocking) > 0 {
			return stop, blocking, text
		}
	}
	if ctx.Err() != nil {
		return false, nil, ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.pending) == 0 {
		return false, nil, ""
	}
	if g.correcting {
		return true, nil, ErrVerificationDelivery.Error() + "：验证写入仍未成功；已有成果保留，不能声称账本交付已完成"
	}
	g.correcting = true
	return false, []string{"【保存回执核对，仅修正一次】record_verification仍有失败，不能称已登记或阴性闭环。请依据实际错误，引用已保存的node_id/artifact_id修正；缺基线/对照时记录inconclusive或缺前提，不编造材料。只处理已有证据，不重放目标操作。修正不了就如实交接未完成项。"}, ""
}

func (g *verificationDeliveryGuard) receiptSummary() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.pending) == 0 && len(g.receipts) == 0 {
		return ""
	}
	ids := make([]string, 0, len(g.receipts))
	for id := range g.receipts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	receipts := make([]verificationReceipt, 0, len(ids))
	for _, id := range ids {
		receipts = append(receipts, g.receipts[id])
	}
	b, _ := json.Marshal(map[string]any{"saved_verifications": receipts, "unresolved_verification_writes": len(g.pending)})
	return "\n\n宿主保存回执核对（以此及实际工具结果为准）：" + string(b)
}
