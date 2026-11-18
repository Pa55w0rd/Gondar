package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"gondar/db"
)

var ErrVerificationDelivery = errors.New("verification_delivery_gap")

// Track only attempted verification writes. A worker doing legitimate reads or
// idempotent work is not required to manufacture a new fact to finish.
type verificationDeliveryGuard struct {
	inner          harness.HookRunner
	mu             sync.Mutex
	pending        map[string]string
	receipts       map[string]verificationReceipt
	correcting     bool
	maxCorrections int
	corrections    int
	lastCorrection string
	inputs         map[string]json.RawMessage
	lookup         func(context.Context, string) (*db.VerificationRecord, error)
	ownerIntentID  int64
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
		Target db.VerificationTarget `json:"target"`
	}
	if json.Unmarshal(input, &v) == nil {
		if _, id, err := db.NormalizeVerificationTarget(v.Target); err == nil {
			return id
		}
	}
	// An unrelated successful write cannot erase a malformed, unidentified one.
	return fmt.Sprintf("invalid:%x", sha256.Sum256(input))
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
		g.inputs = map[string]json.RawMessage{}
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
	if !failed && receipt.Saved && receipt.ID != "" && receipt.ID == key && receipt.NodeID > 0 && receipt.Revision > 0 {
		delete(g.pending, key)
		delete(g.inputs, key)
		g.resolveRepairLocked(in)
		g.receipts[receipt.ID] = receipt
		if len(g.pending) == 0 {
			g.correcting = false
			g.corrections, g.lastCorrection = 0, ""
		}
		return
	}
	message := strings.TrimSpace(string(out))
	if len(message) > 700 {
		message = message[:700]
	}
	g.pending[key] = message
	g.inputs[key] = append(json.RawMessage(nil), in...)
}

func (g *verificationDeliveryGuard) reconcile(ctx context.Context) {
	if g.lookup == nil || ctx.Err() != nil {
		return
	}
	g.mu.Lock()
	inputs := make(map[string]json.RawMessage, len(g.inputs))
	for key, input := range g.inputs {
		inputs[key] = append(json.RawMessage(nil), input...)
	}
	g.mu.Unlock()
	for key, raw := range inputs {
		var update db.VerificationUpdate
		if json.Unmarshal(raw, &update) != nil {
			continue
		}
		id := verificationDeliveryKey(raw)
		if update.IntentID == 0 {
			update.IntentID = g.ownerIntentID
		}
		_, _, err := db.VerificationUpdateIdentity(update)
		if err != nil || id != key {
			continue
		}
		record, err := g.lookup(ctx, id)
		if err != nil || record == nil || record.ID != id || !db.VerificationRecordMatchesUpdate(record, update) {
			continue
		}
		g.mu.Lock()
		// Do not settle a newer concurrent attempt with an older snapshot.
		if string(g.inputs[key]) == string(raw) {
			delete(g.pending, key)
			delete(g.inputs, key)
			g.resolveRepairLocked(raw)
			g.receipts[id] = verificationReceipt{Saved: true, ID: id, State: record.State, NodeID: record.NodeID, Revision: record.Revision}
		}
		if len(g.pending) == 0 {
			g.correcting = false
			g.corrections, g.lastCorrection = 0, ""
		}
		g.mu.Unlock()
	}
}

func (g *verificationDeliveryGuard) resolveRepairLocked(in []byte) {
	var repaired struct {
		RepairOf string `json:"repair_of"`
	}
	_ = json.Unmarshal(in, &repaired)
	if previous, ok := g.inputs[repaired.RepairOf]; ok && strings.HasPrefix(repaired.RepairOf, "invalid:") && compatibleVerificationRepair(previous, in) {
		delete(g.pending, repaired.RepairOf)
		delete(g.inputs, repaired.RepairOf)
	}
}

func (g *verificationDeliveryGuard) pendingCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pending)
}

func (g *verificationDeliveryGuard) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	g.reconcile(ctx)
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
	limit := g.maxCorrections
	if limit <= 0 {
		limit = resolveWrapupTurns("worker")
	}
	fingerprint, _ := json.Marshal(map[string]any{"pending": g.pending, "receipts": g.receipts})
	if g.corrections >= limit || (g.correcting && g.lastCorrection == string(fingerprint)) {
		return true, nil, ErrVerificationDelivery.Error() + "：验证写入仍未成功；已有成果保留，不能声称账本交付已完成"
	}
	g.correcting = true
	g.corrections++
	g.lastCorrection = string(fingerprint)
	var unidentified []string
	for key := range g.pending {
		if strings.HasPrefix(key, "invalid:") {
			unidentified = append(unidentified, key)
		}
	}
	sort.Strings(unidentified)
	note := fmt.Sprintf("【保存回执核对 %d/%d】record_verification仍有失败，已成功部分保留。依据具体错误读取现有材料、最新revision和真实node_id/artifact_id后增量修正；缺基线/对照时记录inconclusive或缺前提，不编造材料。不得重放目标操作；没有实质修正进展时交接未完成项。", g.corrections, limit)
	if len(unidentified) > 0 {
		raw, _ := json.Marshal(unidentified)
		note += " 原target无效的失败编号：" + string(raw) + "；明确修正同一条记录时可用repair_of关联，不能核销无关失败。"
	}
	return false, []string{note}, ""
}

func compatibleVerificationRepair(previous, corrected []byte) bool {
	var oldInput map[string]json.RawMessage
	var next struct {
		Target db.VerificationTarget `json:"target"`
	}
	if json.Unmarshal(previous, &oldInput) != nil || json.Unmarshal(corrected, &next) != nil {
		return false
	}
	var oldTarget map[string]json.RawMessage
	_ = json.Unmarshal(oldInput["target"], &oldTarget) // malformed target itself is repaired explicitly
	target, _, err := db.NormalizeVerificationTarget(next.Target)
	if err != nil {
		return false
	}
	b, _ := json.Marshal(target)
	var fields map[string]string
	_ = json.Unmarshal(b, &fields)
	for name, raw := range oldTarget {
		value, known := fields[name]
		if !known {
			continue
		}
		var old string
		if json.Unmarshal(raw, &old) != nil {
			continue
		} // invalid type is what was repaired
		old = strings.TrimSpace(strings.ReplaceAll(old, "\r\n", "\n"))
		if name == "method" {
			old = strings.ToUpper(old)
		}
		if old != "" && old != value {
			return false
		}
	}
	return true
}

type verificationDeliverySnapshot struct {
	Summary    json.RawMessage
	Persisted  json.RawMessage
	Unresolved int
}

func (g *verificationDeliveryGuard) snapshot() verificationDeliverySnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.pending) == 0 && len(g.receipts) == 0 {
		return verificationDeliverySnapshot{}
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
	pending := make([]map[string]any, 0, len(g.pending))
	keys := make([]string, 0, len(g.pending))
	for key := range g.pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		var input struct {
			Target db.VerificationTarget `json:"target"`
		}
		_ = json.Unmarshal(g.inputs[key], &input)
		pending = append(pending, map[string]any{"id": key, "target": input.Target, "error": g.pending[key], "next_step": "仅核对已有材料和修订，成功项不重做；证据不足保留待验证"})
	}
	data := map[string]any{"saved_verifications": receipts, "unresolved_verification_writes": len(g.pending), "unresolved_verification_details": pending}
	summary, _ := json.Marshal(data)
	data["pending_inputs"] = g.inputs // persistence only; never repeat full tool inputs in model context
	persisted, _ := json.Marshal(data)
	return verificationDeliverySnapshot{Summary: summary, Persisted: persisted, Unresolved: len(g.pending)}
}

func (g *verificationDeliveryGuard) receiptJSON() []byte { return g.snapshot().Summary }

func (g *verificationDeliveryGuard) restore(raw []byte) {
	var state struct {
		Saved   []verificationReceipt `json:"saved_verifications"`
		Pending []struct {
			ID     string                `json:"id"`
			Target db.VerificationTarget `json:"target"`
			Error  string                `json:"error"`
		} `json:"unresolved_verification_details"`
		Inputs map[string]json.RawMessage `json:"pending_inputs"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &state) != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pending = map[string]string{}
	g.inputs = map[string]json.RawMessage{}
	g.receipts = map[string]verificationReceipt{}
	for _, receipt := range state.Saved {
		if receipt.Saved && receipt.ID != "" && receipt.NodeID > 0 && receipt.Revision > 0 {
			g.receipts[receipt.ID] = receipt
		}
	}
	for _, item := range state.Pending {
		input := state.Inputs[item.ID]
		if len(input) == 0 {
			input, _ = json.Marshal(map[string]any{"target": item.Target})
		}
		if !strings.HasPrefix(item.ID, "invalid:") && verificationDeliveryKey(input) != item.ID {
			continue
		}
		g.pending[item.ID] = item.Error
		g.inputs[item.ID] = append(json.RawMessage(nil), input...)
	}
	g.correcting = len(g.pending) > 0
}

func (g *verificationDeliveryGuard) receiptSummary() string {
	if raw := g.receiptJSON(); len(raw) > 0 {
		return "\n\n宿主保存回执核对（以此及实际工具结果为准）：" + string(raw)
	}
	return ""
}
