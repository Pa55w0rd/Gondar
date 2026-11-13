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
	return t.writeExpTool("record_planner_wait", "保存当前分支的具体阻塞并请求最终等待核对，不立即暂停任务。宿主会先自动回复推进建议，刷新态势和未测候选；AI应自行核对现有条件、隔离受阻分支，把仍可执行的步骤补成目标和意图。自动复核后确实无其他方向才可交接等待。先用record_verification保存真实阻塞，引用本任务验证格稳定id；有待执行或运行中意图不可等待，旧目标met不代表持续任务结束。",
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
			return jsonResult(map[string]any{"accepted": true, "task_paused": false, "stage": "host_review", "wait": wait, "scope": "已收到分支阻塞说明；宿主仍需复核其他方向，不代表任务已暂停或测试完成"})
		})
}

const plannerDecisionGuidance = `

【本轮从查阅转入决策】态势已预取，依据具体未验证格、未完成步骤和真实证据自行推导下一步，按需查阅不同证据并提交有依据的目标与可执行意图。本轮采用运行配置中的轮数和时限，仍会制止反复读取相同结果及无实际进展的结束。done仅代表一轮结束，旧目标met但proof_status为unverified/unknown仍待核对。parent_ids仅填事实/发现ID数组，continues仅填意图ID数组，例如[123]；有新证据可持续接续。局部缺前提先保存受阻格并转向其他可执行分支，不把问题直接交给用户。record_planner_wait会先触发宿主自动推进复核，不立即暂停；复核后确实无法推进才附验证格ID和具体步骤交接。不得伪造条件、重复同向耗尽尝试或将等待当作覆盖完成。
`
