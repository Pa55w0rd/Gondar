package agent

// 工具输出台账与证据逐字回验（v0.4.29，同行对比分析 P0b #2）。
//
// 来源：PentestGPT 新版 agent 的 evidence_excerpt 机制（MIT，机制自研重写）——
// executor 返回的证据引用必须是本 episode 真实命令输出的逐字连续子串，在
// append-only trace 里找不到出处即判 validation 失败。伪造证据是 SRC 场景误报/
// 失信的最大来源，比"请复核"类 prompt 都硬。
//
// 我们的落地是「先观察后拦截」的三档模式（防负优化）：
//   off   —— 不校验（默认之外显式关闭用）
//   warn  —— 只核对并在 report_finding 返回里附提示，不阻断（默认）
//   block —— critical/high 且证据里没有任何一行能在本次工具回显中找到出处 → 打回
//
// 匹配规则按 PentestGPT 的两级放宽重写：空白归一化后逐行子串匹配；证据里
// 短行（<20 字符）不计入分母，避免"GET /api"这类通用行干扰判定。

import (
	"context"
	"regexp"
	"strings"
	"sync"
)

// EvidenceVerifyMode 是证据回验档位。
type EvidenceVerifyMode int

const (
	EvidenceVerifyOff EvidenceVerifyMode = iota
	EvidenceVerifyWarn
	EvidenceVerifyBlock
)

// EvidenceVerifyModeFromString 解析设置值（未知值回落到 warn——宁可提示不阻断）。
func EvidenceVerifyModeFromString(v string) EvidenceVerifyMode {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "false", "0":
		return EvidenceVerifyOff
	case "block", "strict":
		return EvidenceVerifyBlock
	default:
		return EvidenceVerifyWarn
	}
}

type toolOutputCtxKey struct{}

type toolOutputsCtx struct {
	ledger *ToolOutputLedger
	mode   EvidenceVerifyMode
}

// WithToolOutputs 把本次 run 的工具输出台账与回验档位挂到 ctx 上，report_finding
// 从 ctx 读取（agent 包不依赖数据库设置层）。
func WithToolOutputs(ctx context.Context, ledger *ToolOutputLedger, mode EvidenceVerifyMode) context.Context {
	if ledger == nil {
		return ctx
	}
	return context.WithValue(ctx, toolOutputCtxKey{}, toolOutputsCtx{ledger: ledger, mode: mode})
}

// ToolOutputsFromContext 取出台账与档位（未接线时 ledger=nil）。
func ToolOutputsFromContext(ctx context.Context) (*ToolOutputLedger, EvidenceVerifyMode) {
	v, ok := ctx.Value(toolOutputCtxKey{}).(toolOutputsCtx)
	if !ok || v.ledger == nil {
		return nil, EvidenceVerifyOff
	}
	return v.ledger, v.mode
}

const (
	toolOutputLedgerMaxEntries = 200
	toolOutputLedgerMaxBytes   = 4 << 20 // 4MB 归一化文本上限
	toolOutputLedgerEntryCap   = 16 << 10
)

// ToolOutputLedger 是本次 run 模型实际看到过的工具回显文本（截断后的内容——
// 恰好也是模型能引用到的全部文字）。addFinding 用它核对证据出处。
type ToolOutputLedger struct {
	mu      sync.Mutex
	entries []string // 已归一化的输出
	bytes   int
}

// NewToolOutputLedger 建一个空台账。
func NewToolOutputLedger() *ToolOutputLedger { return &ToolOutputLedger{} }

var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// normalizeForMatch 去掉 ANSI 控制序列并把行内连续空白压成单空格。
func normalizeForMatch(s string) string {
	// 对照文本与漏洞入库使用一致的UTF-8/NUL表示；原始字节仍以
	// 流量或附件为准，避免同一段真实回显清理后反而找不到出处。
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	s = strings.ToValidUTF8(s, "�")
	s = ansiEscapeRe.ReplaceAllString(s, "")
	if !strings.ContainsAny(s, " \t\r\f\v") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\r', '\f', '\v':
			space = true
			continue
		case '\n':
			space = false
			b.WriteRune('\n')
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Add 追加一条工具回显（截断后的文本）。
func (l *ToolOutputLedger) Add(text string) {
	if l == nil || strings.TrimSpace(text) == "" {
		return
	}
	if len(text) > toolOutputLedgerEntryCap {
		text = text[:toolOutputLedgerEntryCap]
	}
	norm := normalizeForMatch(text)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, norm)
	l.bytes += len(norm)
	for len(l.entries) > toolOutputLedgerMaxEntries || l.bytes > toolOutputLedgerMaxBytes {
		l.bytes -= len(l.entries[0])
		l.entries = l.entries[1:]
	}
}

// VerifyEvidence 统计证据中有多少「有意义的行」（去除空白后 ≥20 字节）能在工具
// 回显里逐字找到。返回 (命中行数, 计入行数)。total 为 0 表示证据太短/无有效行，
// 调用方应跳过判定。
func (l *ToolOutputLedger) VerifyEvidence(evidence string) (matched, total int) {
	if l == nil {
		return 0, 0
	}
	l.mu.Lock()
	joined := strings.Join(l.entries, "\n")
	l.mu.Unlock()
	for _, line := range strings.Split(evidence, "\n") {
		// 和回显使用相同的归一化，再判断有效长度，避免 ANSI 控制序列
		// 或连续空白把短行凑到阈值，甚至让空串匹配任意工具输出。
		line = strings.TrimSpace(normalizeForMatch(line))
		if len(line) < 20 {
			continue
		}
		total++
		if strings.Contains(joined, line) {
			matched++
		}
	}
	return matched, total
}

// evidenceVerdict 把命中情况归为三档；没有有效行时无法判定，不打扰。
// 单行引用也必须有出处；混有未核实内容时不能标为全部通过。
func evidenceVerdict(matched, total int) string {
	if total == 0 {
		return "good"
	}
	if matched == 0 {
		return "none"
	}
	if matched == total {
		return "good"
	}
	return "partial"
}

// String 返回设置值形式（off|warn|block），用于设置回显。
func (m EvidenceVerifyMode) String() string {
	switch m {
	case EvidenceVerifyOff:
		return "off"
	case EvidenceVerifyBlock:
		return "block"
	default:
		return "warn"
	}
}
