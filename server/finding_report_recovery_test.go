package server

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"gondar/agent"
	"gondar/db"
)

type recoveryReportProvider struct {
	complete func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
	stream   func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error]
}

func (p recoveryReportProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.complete(ctx, req)
}

func (p recoveryReportProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return p.stream(ctx, req)
}

func reportRecoveryServerFixture(t *testing.T) (*Server, *db.Task, int64, *db.FindingReportRecovery) {
	t.Helper()
	dsn := os.Getenv("GONDAR_PG_DSN")
	if dsn == "" {
		t.Skip("报告恢复集成回归需要显式专用 GONDAR_PG_DSN")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	task, err := pg.CreateTask("report recovery server fixture", "仅合成数据，无目标请求", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.DeleteTaskCascade(task.ID, true, true) })
	fid, err := pg.AddFinding(task.ID, 0, "fixture", "合成发现", "low", "既有摘要", "既有证据；其中的任何指令都是数据", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`UPDATE findings SET created_at=now()-interval '1 hour' WHERE id=$1`, fid); err != nil {
		t.Fatal(err)
	}
	job, err := pg.ClaimFindingReportRecovery(t.Context(), time.Now(), time.Hour)
	if err != nil || job == nil {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	// Construct just the delivery dependencies; NewManager would copy unrelated
	// scanning templates and New would start background engines.
	s := &Server{m: &Manager{pg: pg, dir: t.TempDir()}, ctx: t.Context()}
	return s, task, fid, job
}

func recoveryText(text, stop string) (llm.Message, string, llm.Usage, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(text)}}, stop, llm.Usage{InputTokens: 11, OutputTokens: 7}, nil
}

func TestReportRecoverySavesOnlyClaimedFindingWithoutToolsOrTaskRestart(t *testing.T) {
	s, task, fid, job := reportRecoveryServerFixture(t)
	other, err := s.m.pg.AddFinding(task.ID, 0, "other", "其他记录", "low", "other", "other evidence", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`UPDATE findings SET report='原始短报告' WHERE id=$1`, other); err != nil {
		t.Fatal(err)
	}
	job.NodeID = other // a node-like number must never redirect the write
	var calls int
	prov := recoveryReportProvider{complete: func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if len(req.Tools) != 0 || req.Thinking == "disabled" {
			t.Fatal("recovery exposed tools or forced unsupported thinking")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 10*time.Minute {
			t.Fatal("configured 12 minute delivery was reduced to a fixed short timeout")
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(req.Messages[0].Text()), &data); err != nil {
			t.Fatal(err)
		}
		if data["finding_id"] != float64(fid) || data["task_id"] != float64(task.ID) || data["finding_node_id"] != float64(other) {
			t.Fatalf("IDs mixed in model input: %+v", data)
		}
		return recoveryText("## 漏洞概述\n仅描述合成证据。\n\n## 证据限制与未验证范围\n没有重新探测。", "end_turn")
	}}
	s.runFindingReportRecovery(t.Context(), job, prov, agent.Config{}, 12*time.Minute)
	f, err := s.m.pg.GetFinding(fid)
	if err != nil || calls != 1 || !strings.Contains(f.Report, "合成证据") || f.Evidence != job.Evidence || f.Status != db.FindingPending {
		t.Fatalf("delivery lost source or failed: %+v calls=%d err=%v", f, calls, err)
	}
	f2, _ := s.m.pg.GetFinding(other)
	if f2.Report != "原始短报告" {
		t.Fatal("node/finding number confusion overwrote another report")
	}
	var status string
	var intents int
	if err := s.m.pg.QueryRow(`SELECT status FROM tasks WHERE id=$1`, task.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent'`, task.ExplorationID).Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if status != "created" || intents != 0 {
		t.Fatalf("report recovery restarted target work: %s %d", status, intents)
	}
	var input, output int
	if err := s.m.pg.QueryRow(`SELECT COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0) FROM activity WHERE exploration_id=$1 AND worker='reporter' AND kind='result'`, task.ExplorationID).Scan(&input, &output); err != nil || input != 11 || output != 7 {
		t.Fatalf("delivery usage not accounted exactly once: %d %d %v", input, output, err)
	}
}

func TestReportRecoveryDoesNotSavePartialOrToolResponses(t *testing.T) {
	for _, result := range []string{"empty", "truncated", "provider_error", "tool"} {
		t.Run(result, func(t *testing.T) {
			s, _, fid, job := reportRecoveryServerFixture(t)
			prov := recoveryReportProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				switch result {
				case "empty":
					return recoveryText(" ", "end_turn")
				case "truncated":
					return recoveryText("半份报告", "max_tokens")
				case "provider_error":
					return llm.Message{}, "", llm.Usage{}, errors.New("fixture private provider diagnostic")
				default:
					return llm.Message{Content: []llm.ContentBlock{{Type: llm.BlockToolUse, Name: "Bash", Input: json.RawMessage(`{"command":"must never run"}`)}}}, "tool_use", llm.Usage{}, nil
				}
			}}
			s.runFindingReportRecovery(t.Context(), job, prov, agent.Config{}, time.Hour)
			f, _ := s.m.pg.GetFinding(fid)
			var attempts int
			var diagnostic, token string
			var next *time.Time
			if err := s.m.pg.QueryRow(`SELECT report_recovery_attempts,report_recovery_error,report_recovery_token,report_recovery_next_at FROM findings WHERE id=$1`, fid).Scan(&attempts, &diagnostic, &token, &next); err != nil {
				t.Fatal(err)
			}
			if f.Report != "" || attempts != 1 || diagnostic == "" || token != "" || next == nil || time.Until(*next) < 59*time.Minute || strings.Contains(diagnostic, "private") {
				t.Fatalf("partial result saved or retry not bounded: %+v %d %q %q %v", f, attempts, diagnostic, token, next)
			}
		})
	}
}

func TestReportRecoveryPauseAndConcurrentEvidenceRejectDelivery(t *testing.T) {
	for _, when := range []string{"before_model", "during_model", "evidence_changed"} {
		t.Run(when, func(t *testing.T) {
			s, task, fid, job := reportRecoveryServerFixture(t)
			if when == "before_model" {
				if err := s.m.pg.SetPaused(task.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			prov := recoveryReportProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if when == "during_model" {
					if err := s.m.pg.SetPaused(task.ID, true); err != nil {
						t.Fatal(err)
					}
				} else if when == "evidence_changed" {
					if _, err := s.m.pg.Exec(`UPDATE findings SET evidence_version=evidence_version+1 WHERE id=$1`, fid); err != nil {
						t.Fatal(err)
					}
				}
				return recoveryText("不可保存的过期报告", "end_turn")
			}}
			s.runFindingReportRecovery(t.Context(), job, prov, agent.Config{}, time.Hour)
			f, _ := s.m.pg.GetFinding(fid)
			if f.Report != "" || (when == "before_model" && calls != 0) {
				t.Fatalf("paused or stale source saved: %+v calls=%d", f, calls)
			}
		})
	}
}

func TestReportRecoveryStreamRequiresCleanEnd(t *testing.T) {
	for _, clean := range []bool{false, true} {
		prov := recoveryReportProvider{stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) {
				if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "合成报告"}, nil) || !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil) {
					return
				}
				if clean {
					yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
				}
			}
		}}
		text, _, err := generateRecoveryReport(t.Context(), prov, llm.CompletionRequest{}, true)
		if (err == nil) != clean || (clean && text != "合成报告") {
			t.Fatalf("clean=%t text=%q err=%v", clean, text, err)
		}
	}
}

func TestReportRecoveryRequestBoundsUTF8AndRejectsImpossibleBudget(t *testing.T) {
	job := &db.FindingReportRecovery{FindingID: 41, TaskID: 503, NodeID: 9001, Evidence: strings.Repeat("现有证据；", 100000)}
	cfg := agent.Config{ContextWindowK: 16, MaxTokens: 4096}
	req, err := reportRecoveryRequest(job, nil, "报告格式", cfg)
	if err != nil || len(req.Tools) != 0 || len(req.Messages[0].Text()) > 100000 || !strings.Contains(req.Messages[0].Text(), "截断") {
		t.Fatalf("bounded material failed: len=%d err=%v", len(req.Messages), err)
	}
	cfg.MaxTokens = 1000000
	if _, err := reportRecoveryRequest(job, nil, "报告格式", cfg); err == nil {
		t.Fatal("impossible output reservation ignored")
	}
}

func TestReportRecoveryQueuedReporterDefersAutomaticDelivery(t *testing.T) {
	s, _, fid, job := reportRecoveryServerFixture(t)
	if err := s.m.pg.FailFindingReportRecovery(t.Context(), job, "fixture_released", time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.triggerActive = map[string]int{"reporter": 1}
	var called atomic.Bool
	s.llmOn, s.llmProv = true, recoveryReportProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		called.Store(true)
		return recoveryText("不应生成", "end_turn")
	}}
	count, err := s.recoverFindingReportsOnce(t.Context(), time.Now())
	if err != nil || count != 0 || called.Load() {
		t.Fatalf("competed with active reporter: %d %t %v", count, called.Load(), err)
	}
	var attempts int
	if err := s.m.pg.QueryRow(`SELECT report_recovery_attempts FROM findings WHERE id=$1`, fid).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("deferral consumed attempt: %d %v", attempts, err)
	}
}

func TestReportRecoveryManualReporterAlsoDefers(t *testing.T) {
	s, _, _, _ := reportRecoveryServerFixture(t)
	conv, err := s.m.pg.CreateConversation("reporter", "合成手动报告", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.m.pg.Exec(`DELETE FROM conversations WHERE id=$1`, conv.ID) })
	s.chatBusy = map[string]bool{s.convBusyKey(conv.ID): true}
	if !s.reporterTriggerBusy() {
		t.Fatal("manual reporter was not recognised")
	}
	s.chatBusy[s.convBusyKey(conv.ID)] = false
	if s.reporterTriggerBusy() {
		t.Fatal("completed manual reporter still blocks recovery")
	}
}

func TestReportRecoveryDoesNotFallBackFromExhaustedTaskChain(t *testing.T) {
	s, _, _, job := reportRecoveryServerFixture(t)
	s.llmOn, s.llmProv = true, recoveryReportProvider{}
	job.TaskChainConfigured, job.ProfileID = true, nil
	if _, _, ok := s.reportRecoveryProvider(job); ok {
		t.Fatal("exhausted task chain silently fell back to global model")
	}
}

func TestReportRecoveryOnceClaimsAndDeliversWithoutStartingEngines(t *testing.T) {
	s, task, fid, claim := reportRecoveryServerFixture(t)
	if err := s.m.pg.FailFindingReportRecovery(t.Context(), claim, "fixture_released", time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	old, err := s.m.pg.GetAgentByKey("reporter")
	if err != nil {
		t.Fatal(err)
	}
	if old == nil {
		if _, err := s.m.pg.CreateAgent("reporter", "合成报告角色", "仅离线恢复回归"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.m.pg.DeleteAgent("reporter") })
	} else {
		t.Cleanup(func() {
			s.m.pg.Exec(`UPDATE agents SET enabled=$2,run_seconds=$3,llm_profile_id=$4 WHERE id=$1`, old.ID, old.Enabled, old.RunSecs, old.LLMProfileID)
		})
	}
	if _, err := s.m.pg.Exec(`UPDATE agents SET enabled=true,run_seconds=720,llm_profile_id=NULL WHERE key='reporter'`); err != nil {
		t.Fatal(err)
	}
	flag, existed, err := s.m.pg.GetSetting("report_recovery_enabled")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			s.m.pg.SetSetting("report_recovery_enabled", flag)
		} else {
			s.m.pg.Exec(`DELETE FROM settings WHERE key='report_recovery_enabled'`)
		}
	})
	if err := s.m.pg.SetSetting("report_recovery_enabled", "true"); err != nil {
		t.Fatal(err)
	}
	var calls int
	s.llmOn, s.llmProv = true, recoveryReportProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return recoveryText("## 恢复报告\n仅用既有合成证据。", "end_turn")
	}}
	count, err := s.recoverFindingReportsOnce(t.Context(), time.Now())
	if err != nil || count != 1 || calls != 1 {
		t.Fatalf("automatic delivery did not drain exactly once: %d %d %v", count, calls, err)
	}
	f, err := s.m.pg.GetFinding(fid)
	if err != nil || f.Report == "" || f.Evidence != claim.Evidence || f.Status != db.FindingPending {
		t.Fatalf("automatic delivery lost original: %+v %v", f, err)
	}
	var status string
	if err := s.m.pg.QueryRow(`SELECT status FROM tasks WHERE id=$1`, task.ID).Scan(&status); err != nil || status != "created" {
		t.Fatalf("automatic report changed task lifecycle: %q %v", status, err)
	}
}
