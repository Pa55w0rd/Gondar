package agent

import (
	"context"
	"fmt"
	"iter"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// WorkerProgressPolicy keeps a periodic save reminder separate from the actual
// no-progress guard. A zero reminder interval disables reminders, not protection.
type WorkerProgressPolicy struct {
	SaveReminderSeconds int `json:"save_reminder_seconds"`
	IdleRounds          int `json:"idle_rounds"`
	IdleSeconds         int `json:"idle_seconds"`
}

func DefaultWorkerProgressPolicy() WorkerProgressPolicy {
	return WorkerProgressPolicy{SaveReminderSeconds: 900, IdleRounds: idleStopRounds, IdleSeconds: int(idleStopMinElapsed / time.Second)}
}

func (p WorkerProgressPolicy) Validate() error {
	if p.SaveReminderSeconds < 0 || p.SaveReminderSeconds > 31536000 || p.IdleRounds < 1 || p.IdleRounds > 10000 || p.IdleSeconds < 1 || p.IdleSeconds > 31536000 {
		return fmt.Errorf("保存提醒须为0至31536000秒；防空转调用次数须为1至10000，观察时间须为1至31536000秒")
	}
	return nil
}

// SetProgressPolicy resolves a fresh snapshot before each run, including cached
// worker bundles. It never changes the policy of an already executing run.
func (w *Worker) SetProgressPolicy(resolve func() WorkerProgressPolicy) { w.progressPolicy = resolve }

func (w *Worker) resolveProgressPolicy() WorkerProgressPolicy {
	if w.progressPolicy != nil {
		if p := w.progressPolicy(); p.Validate() == nil {
			return p
		}
	}
	return DefaultWorkerProgressPolicy()
}

func (s *idleStopper) saveReminder() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	interval := time.Duration(s.policy.SaveReminderSeconds) * time.Second
	now := s.now()
	if interval <= 0 || now.Sub(s.lastWrite) < interval || (!s.lastReminder.IsZero() && now.Sub(s.lastReminder) < interval) {
		return ""
	}
	s.lastReminder = now
	return "【保存提醒】已有一段时间未向平台保存成果。请在合适步骤使用 save_evidence 登记现有文件，或保存真实观察与未验证步骤；相对路径以本意图目录为基准，无须先复制文件。此提醒不暂停、不阻挡工具，也不表示本意图已完成；不要为消除提醒编造发现。"
}

// Reminders are request-local context, never a synthetic failed tool result or
// a Stop-hook retry. This lets the next real action execute with its own result.
type progressReminderProvider struct {
	inner      llm.Provider
	stop       *idleStopper
	onReminder func(string)
}

func (p *progressReminderProvider) request(req llm.CompletionRequest) llm.CompletionRequest {
	// Compaction/auxiliary requests have no tool surface. They must neither
	// consume a reminder nor receive instructions to call unavailable tools.
	if len(req.Tools) == 0 {
		return req
	}
	if text := p.stop.saveReminder(); text != "" {
		req.Messages = append(append([]llm.Message(nil), req.Messages...), llm.UserText(text))
		if p.onReminder != nil {
			p.onReminder(text)
		}
	}
	return req
}

func (p *progressReminderProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.inner.Complete(ctx, p.request(req))
}

func (p *progressReminderProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for event, err := range p.inner.Stream(ctx, p.request(req)) {
			if !yield(event, err) {
				return
			}
		}
	}
}
