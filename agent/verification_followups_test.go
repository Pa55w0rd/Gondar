package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"gondar/db"
)

func TestVerificationFollowupsKeepExactRevisionAndDoNotCloseUnknowns(t *testing.T) {
	page := &db.VerificationPage{Cases: []*db.VerificationRecord{
		{ID: "negative", State: db.VerificationNegative},
		{ID: "confirmed", State: db.VerificationConfirmed},
		{ID: "missing", Revision: 3, NodeID: 72, State: db.VerificationMissingPrerequisite, Target: db.VerificationTarget{Endpoint: "https://example.test/a", Method: "POST", Identity: "controlled-user"}, MissingPrerequisites: []string{"account"}, Evidence: []db.VerificationEvidence{{Role: "test", ActivityID: 9}}},
	}}
	cards := verificationFollowups(page)
	if len(cards) != 1 || cards[0]["verification_id"] != "missing" || cards[0]["revision"] != int64(3) || cards[0]["node_id"] != int64(72) {
		t.Fatalf("unbound cards: %+v", cards)
	}
	if page.Cases[2].State != db.VerificationMissingPrerequisite || page.Cases[2].Revision != 3 {
		t.Fatal("read view rewrote evidence")
	}
	missing := cards[0]["missing_evidence_roles"].([]string)
	if strings.Join(missing, ",") != "baseline,control,impact" {
		t.Fatalf("roles=%v", missing)
	}
	for i := 0; i < 20; i++ {
		page.Cases = append(page.Cases, &db.VerificationRecord{ID: fmt.Sprint(i), State: db.VerificationInconclusive})
	}
	if len(verificationFollowups(page)) != 6 {
		t.Fatal("unbounded context")
	}
}

func TestResumedIntentKeepsHostUnknownReceiptWarning(t *testing.T) {
	n := &db.Node{ID: 12, Kind: db.KindIntent, Payload: json.RawMessage(`{"summary":"synthetic continuation","_execution":{"id":"new","prior_uncertain_run_id":"old"}}`)}
	if prompt := renderIntentTask(n); !strings.Contains(prompt, "execution_result_unknown") || !strings.Contains(prompt, "晚到回执") {
		t.Fatal("resumed worker lacks unknown execution warning")
	}
}

func TestPlannerConsumerFollowupDispatchesKnownMethodWithLocalAccountBranchBlocked(t *testing.T) {
	_, task, store, _, _ := autonomyFixture(t)
	goal, _ := store.AddGoal(map[string]any{"text": "existing finished goal"}, "human")
	if err := store.SetNodeState(goal, "met"); err != nil {
		t.Fatal(err)
	}
	basis, err := store.AddNode(db.KindFact, map[string]any{"summary": "已存契约包含匿名POST消费者；GET404只覆盖GET", "evidence": "synthetic offline contract: POST https://example.test/consumer JSON object_id; GET 404", "confidence": "observed"}, 0, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	provider := plannerStateProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		raw, _ := json.Marshal(req)
		if calls == 1 && (!strings.Contains(string(raw), "真实消费者") || !strings.Contains(string(raw), "missing_prerequisite")) {
			t.Error("consumer method or local blocker absent from actual SDK request")
		}
		name, input := "", ""
		switch calls {
		case 1:
			name, input = "add_exploration_goal", fmt.Sprintf(`{"text":"核对已有匿名POST消费者的独立对象边界","parent_ids":[%d]}`, basis)
		case 2:
			name, input = "add_intent", fmt.Sprintf(`{"summary":"依据已存契约独立验证匿名POST JSON object_id；保留GET404结论，不触及缺账号分支","parent_ids":[%d],"hypothesis":"已存POST消费者与GET路径行为不同，尚未核对对象边界","success_criteria":"保存正常对象基线、受控对照及实际消费结果；缺前提停止该边"}`, basis)
		}
		if name != "" {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint(calls), Name: name, Input: json.RawMessage(input)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("synthetic handoff complete")}}, "end_turn", llm.Usage{}, nil
	}}
	p := NewPlanner(provider, "fixture", t.TempDir(), nil, 200000, 8)
	p.SetNonStreaming(func() bool { return true })
	met, _, err := p.Plan(t.Context(), task.ID, nil, store, task.Goal, nil, nil)
	if err != nil || met {
		t.Fatalf("dispatch met=%v err=%v", met, err)
	}
	frontier, err := store.Frontier(10)
	if err != nil || len(frontier) != 1 || !strings.Contains(string(frontier[0].Payload), "POST") {
		t.Fatalf("no real intent: %+v %v", frontier, err)
	}
	old, _ := store.GetNode(goal)
	if old.State != "met" {
		t.Fatal("old goal was rewritten")
	}
	page, err := store.ListVerifications(t.Context(), db.VerificationFilter{Limit: 10})
	if err != nil || page.LatestStates[db.VerificationMissingPrerequisite] != 1 || page.RecordedCases != 1 {
		t.Fatalf("blocker lost or unexecuted candidate became evidence: %+v %v", page, err)
	}
}
