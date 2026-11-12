package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
	"gondar/db"
)

var ErrPlannerWaiting = errors.New("planner_waiting")

type PlannerWait struct {
	Kind            string   `json:"kind"`
	Reason          string   `json:"reason"`
	VerificationIDs []string `json:"verification_ids"`
	RemainingSteps  []string `json:"remaining_steps"`
	progress        *db.PlannerProgress
}

type PlannerWaitError struct{ Wait PlannerWait }

func (e *PlannerWaitError) Error() string {
	return "planner_waiting：" + e.Wait.Reason + "；已有成果保留，未验证范围保持未知"
}
func (e *PlannerWaitError) Unwrap() error { return ErrPlannerWaiting }

func (w PlannerWait) Current(ctx context.Context, store *db.ExplorationStore) bool {
	if w.progress == nil {
		return true
	}
	after, err := store.PlannerProgress(ctx)
	return err == nil && after.ActiveIntents == 0 && after.LatestIntent == w.progress.LatestIntent && after.LatestFinding == w.progress.LatestFinding && !after.EvidenceAdvancedSince(*w.progress)
}

func (w PlannerWait) Validate(ctx context.Context, store *db.ExplorationStore) error {
	if store == nil {
		return fmt.Errorf("缺少本任务上下文")
	}
	if w.Kind != "missing_prerequisite" && w.Kind != "environment_blocked" && w.Kind != "no_executable_candidates" {
		return fmt.Errorf("等待类型无效")
	}
	if strings.TrimSpace(w.Reason) == "" || len(w.Reason) > 1024 || len(w.VerificationIDs) == 0 || len(w.VerificationIDs) > 16 || len(w.RemainingSteps) == 0 || len(w.RemainingSteps) > 16 {
		return fmt.Errorf("等待需简短原因、1至16个真实验证格及未完成步骤")
	}
	for _, step := range w.RemainingSteps {
		if strings.TrimSpace(step) == "" || len(step) > 512 {
			return fmt.Errorf("未完成步骤必须具体且简短")
		}
	}
	active, err := store.HasActiveIntent()
	if err != nil {
		return err
	}
	if active {
		return fmt.Errorf("仍有待执行或运行中的意图，请等待实际产出，不暂停整个任务")
	}
	for _, id := range w.VerificationIDs {
		page, err := store.ListVerifications(ctx, db.VerificationFilter{ID: id, Limit: 1})
		if err != nil {
			return err
		}
		if len(page.Cases) != 1 {
			return fmt.Errorf("等待引用的验证格不存在于本任务：%s", id)
		}
		r := page.Cases[0]
		if r.Observation == "" || len(r.Evidence) == 0 {
			return fmt.Errorf("等待需已保存观察和证据，不能仅凭文字停机")
		}
		switch w.Kind {
		case "missing_prerequisite":
			if r.State != db.VerificationMissingPrerequisite || len(r.MissingPrerequisites) == 0 {
				return fmt.Errorf("该格未记录具体缺失前提")
			}
		case "environment_blocked":
			if r.State != db.VerificationBlocked || r.Blocker == "" {
				return fmt.Errorf("该格未记录环境阻塞及回执")
			}
		default:
			if r.State != db.VerificationMissingPrerequisite && r.State != db.VerificationBlocked && r.State != db.VerificationInconclusive {
				return fmt.Errorf("无可执行候选需引用受阻或不确定格，阴性不能替代剩余面核对")
			}
		}
	}
	page, err := store.ListVerifications(ctx, db.VerificationFilter{Limit: 1})
	if err != nil {
		return err
	}
	if page.LatestStates[db.VerificationNotTested] > 0 {
		return fmt.Errorf("仍有登记为未测的候选；先核对其是否可执行或按真实原因记录受阻")
	}
	return nil
}

func (t *ToolSet) recordPlannerWait() actool.CoreTool {
	return t.writeExpTool("record_planner_wait", "明确交接当前无法执行的规划轮，等待具体前提/环境/新线索。先用record_verification保存真实阻塞并核对范围内其他可执行方向；有在跑或待执行意图时不可等待。引用本任务验证格稳定id，保留未完成步骤；这不是完成证明，也不自动重启任务。",
		obj(map[string]any{
			"kind":             map[string]any{"type": "string", "enum": []string{"missing_prerequisite", "environment_blocked", "no_executable_candidates"}},
			"reason":           str("具体缺少什么，以及为何当前其他方向也无法执行；不填已测完"),
			"verification_ids": map[string]any{"type": "array", "minItems": 1, "maxItems": 16, "items": map[string]any{"type": "string"}},
			"remaining_steps":  map[string]any{"type": "array", "minItems": 1, "maxItems": 16, "items": map[string]any{"type": "string"}},
		}, "kind", "reason", "verification_ids", "remaining_steps"),
		func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var wait PlannerWait
			if err := json.Unmarshal(raw, &wait); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if err := wait.Validate(ctx, t.ts); err != nil {
				return actool.Errorf("尚不能进入等待：" + err.Error()), nil
			}
			progress, err := t.ts.PlannerProgress(ctx)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			wait.progress = &progress
			t.plannerWait = &wait
			return jsonResult(map[string]any{"accepted": true, "wait": wait, "scope": "仅说明当前等待条件，未登记和未验证面保持未知，不能认定任务完成"})
		})
}

const plannerDecisionGuidance = `

【本轮从查阅转入决策】态势已预取，优先用具体未验证格、未完成步骤和真实证据决定下一步；最多12次没有实质进展的查阅，随后应提交少量可执行意图或明确交接等待。done仅代表一轮结束，旧目标met但proof_status为unverified/unknown仍是待核对项，不等同整片安全。parent_ids仅填写事实/发现ID数组，接续意图ID填写continues数组，例如continues:[123]；有新证据可继续，无总三代限制。没有可执行方向时，先保存缺前提/受阻/不确定格；核对其他方向确实不能执行后调用record_planner_wait，附验证格稳定ID和未完成步骤。未登记范围仍未知，不编造方向、不重复证明旧目标，也不把等待账号当模型或规划故障。
`
