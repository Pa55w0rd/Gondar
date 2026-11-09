package agent

import (
	"context"
	"strings"
	"testing"
)

// v0.4.29（P0b #2）：证据逐字回验——证据里的有效行必须在本次工具回显中找到出处。
func TestToolOutputLedgerVerifyEvidence(t *testing.T) {
	l := NewToolOutputLedger()
	l.Add("HTTP/1.1 200 OK\nServer: nginx/1.25.3 (Ubuntu)\n\n{\"total\":63631,\"data\":[...]}\nX-Powered-By: PHP/8.1.2")
	l.Add("$ id\nuid=0(root) gid=0(root)")

	// 两行都能逐字找到（含空白差异归一化）。
	ev := "  Server: nginx/1.25.3 (Ubuntu)   \n  \"total\":63631,\"data\":[...]  \nuid=0(root) gid=0(root)"
	matched, total := l.VerifyEvidence(ev)
	if matched != 3 || total != 3 {
		t.Fatalf("matched=%d total=%d want 3/3 (ev=%q)", matched, total, ev)
	}
	if v := evidenceVerdict(matched, total); v != "good" {
		t.Fatalf("verdict=%s want good", v)
	}

	// 全是转述 → none。
	ev = "该接口未授权返回了 6 万多条患者记录，说明存在未授权访问。"
	matched, total = l.VerifyEvidence(ev)
	if matched != 0 || total != 1 { // 单行长句计入但未命中
		t.Fatalf("matched=%d total=%d want 0/1", matched, total)
	}
	if v := evidenceVerdict(matched, total); v != "none" {
		t.Fatalf("verdict=%s want none for a single ungrounded line", v)
	}

	// 两条以上且全无出处 → none。
	ev = "第一条转述结论写在这里支撑漏洞判断。\n第二条转述结论继续描述影响范围。"
	matched, total = l.VerifyEvidence(ev)
	if matched != 0 || total != 2 {
		t.Fatalf("matched=%d total=%d want 0/2", matched, total)
	}
	if v := evidenceVerdict(matched, total); v != "none" {
		t.Fatalf("verdict=%s want none", v)
	}

	// 部分命中 → partial（命中不足一半）。
	ev = "Server: nginx/1.25.3 (Ubuntu)\n这是一条纯转述、工具输出里不存在的内容。\n另一条同样不在工具输出里的补充说明文字。"
	matched, total = l.VerifyEvidence(ev)
	if matched != 1 || total != 3 {
		t.Fatalf("matched=%d total=%d want 1/3", matched, total)
	}
	if v := evidenceVerdict(matched, total); v != "partial" {
		t.Fatalf("verdict=%s want partial", v)
	}

	// 短行（<20 字节）不计入分母。
	matched, total = l.VerifyEvidence("id\nls\n这是一个足够长的转述句子用来测试分母是否忽略短行。")
	if total != 1 {
		t.Fatalf("total=%d want 1 (short lines must be ignored)", total)
	}
}

// 参考 PentestGPT 对每条证据都校验真实回显出处的机制：单行不能绕过，
// 部分真实引用也不能替同段中的未验证内容背书。
func TestEvidenceVerdictRequiresEveryMeaningfulLine(t *testing.T) {
	for _, tc := range []struct {
		name           string
		matched, total int
		want           string
	}{
		{"no meaningful evidence", 0, 0, "good"},
		{"single grounded line", 1, 1, "good"},
		{"single invented line", 0, 1, "none"},
		{"half grounded", 1, 2, "partial"},
		{"mostly grounded", 2, 3, "partial"},
		{"all grounded", 3, 3, "good"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := evidenceVerdict(tc.matched, tc.total); got != tc.want {
				t.Fatalf("verdict(%d, %d)=%s, want %s", tc.matched, tc.total, got, tc.want)
			}
		})
	}
}

func TestToolOutputLedgerCountsNormalizedEvidence(t *testing.T) {
	l := NewToolOutputLedger()
	l.Add("observed-response-header: sample-value")
	for _, ev := range []string{
		strings.Repeat("\x1b[31m", 8) + "\x1b[0m",
		strings.Repeat("\x1b[31m", 8) + "absent" + "\x1b[0m",
		"absent" + strings.Repeat(" ", 40) + "value",
	} {
		if matched, total := l.VerifyEvidence(ev); matched != 0 || total != 0 {
			t.Fatalf("control sequences or whitespace inflated evidence count: %d/%d", matched, total)
		}
	}
	if matched, total := l.VerifyEvidence("observed-response-header: sample-value\nthis is an invented response header"); matched != 1 || total != 2 {
		t.Fatalf("mixed evidence=%d/%d, want 1/2", matched, total)
	}
}

func TestToolOutputLedgerEmptyDoesNotVerifyClaims(t *testing.T) {
	l := NewToolOutputLedger()
	matched, total := l.VerifyEvidence("a long claim without any actual observed tool output")
	if matched != 0 || total != 1 || evidenceVerdict(matched, total) != "none" {
		t.Fatalf("empty ledger verified an unsupported claim: %d/%d", matched, total)
	}
}

func TestToolOutputLedgerMatchesStorageCleanedText(t *testing.T) {
	l := NewToolOutputLedger()
	l.Add("observed-response-body: a\x00b\xffc with verified context")
	matched, total := l.VerifyEvidence("observed-response-body: ab�c with verified context")
	if matched != 1 || total != 1 {
		t.Fatalf("storage-compatible text lost its real source: %d/%d", matched, total)
	}
}

// ANSI 控制序列不应影响匹配。
func TestToolOutputLedgerANSI(t *testing.T) {
	l := NewToolOutputLedger()
	l.Add("\x1b[31mERROR: access denied for user admin\x1b[0m")
	if matched, total := l.VerifyEvidence("ERROR: access denied for user admin"); matched != 1 || total != 1 {
		t.Fatalf("matched=%d total=%d want 1/1", matched, total)
	}
}

// ctx 挂载/读取。
func TestToolOutputsContext(t *testing.T) {
	l := NewToolOutputLedger()
	ctx := WithToolOutputs(context.Background(), l, EvidenceVerifyBlock)
	got, mode := ToolOutputsFromContext(ctx)
	if got != l || mode != EvidenceVerifyBlock {
		t.Fatalf("got ledger=%p mode=%v", got, mode)
	}
	if led, m := ToolOutputsFromContext(context.Background()); led != nil || m != EvidenceVerifyOff {
		t.Fatalf("empty ctx must yield nil/off, got %v/%v", led, m)
	}
	// 档位解析
	for in, want := range map[string]EvidenceVerifyMode{
		"off": EvidenceVerifyOff, "block": EvidenceVerifyBlock, "warn": EvidenceVerifyWarn,
		"": EvidenceVerifyWarn, "strict": EvidenceVerifyBlock, "0": EvidenceVerifyOff,
	} {
		if got := EvidenceVerifyModeFromString(in); got != want {
			t.Fatalf("mode(%q)=%v want %v", in, got, want)
		}
	}
}

// 台账超出上限时滚动丢弃最旧记录。
func TestToolOutputLedgerRolling(t *testing.T) {
	l := NewToolOutputLedger()
	for i := 0; i < toolOutputLedgerMaxEntries+10; i++ {
		l.Add(strings.Repeat("x", 32))
	}
	l.mu.Lock()
	n := len(l.entries)
	l.mu.Unlock()
	if n > toolOutputLedgerMaxEntries {
		t.Fatalf("entries=%d must be capped at %d", n, toolOutputLedgerMaxEntries)
	}
}
