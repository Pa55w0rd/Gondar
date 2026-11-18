package server

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"gondar/agent"
	"gondar/db"
)

func TestToolContractUpgradeAndRuntimeAssemblyKeepFindingSources(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no isolated test database: %v", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	const key = "report_finding"
	var original json.RawMessage
	if err := pg.QueryRow(`SELECT coalesce((SELECT to_jsonb(t) FROM tools t WHERE key=$1),'null'::jsonb)`, key).Scan(&original); err != nil {
		t.Fatal(err)
	}
	oldFlag, hadFlag, err := pg.GetSetting("execution_tool_contracts_v033")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM tools WHERE key=$1`, key)
		if string(original) != "null" {
			_, _ = pg.Exec(`INSERT INTO tools SELECT (jsonb_populate_record(NULL::tools,$1)).*`, original)
		}
		_, _ = pg.Exec(`DELETE FROM settings WHERE key='execution_tool_contracts_v033'`)
		if hadFlag {
			_ = pg.SetSetting("execution_tool_contracts_v033", oldFlag)
		}
	})
	var seed agent.ToolSeed
	for _, candidate := range agent.BuiltinToolSeeds() {
		if candidate.Key == key {
			seed = candidate
		}
	}
	raw, _ := json.Marshal(seed.Schema)
	var stale map[string]any
	_ = json.Unmarshal(raw, &stale)
	props := stale["properties"].(map[string]any)
	for _, field := range []string{"evidence_mode", "verification_ref", "evidence_refs"} {
		delete(props, field)
	}
	props["severity"].(map[string]any)["default"] = "low"
	props["summary"].(map[string]any)["description"] = "用户保留的摘要说明"
	staleRaw, _ := json.Marshal(stale)
	_, err = pg.Exec(`INSERT INTO tools(key,system,description,schema,agents,enabled,deferred)
VALUES($1,true,'用户保留的漏洞说明',$2,'["worker"]',false,true)
ON CONFLICT(key) DO UPDATE SET system=true,kind='builtin',description=excluded.description,schema=excluded.schema,agents=excluded.agents,enabled=false,deferred=true`, key, staleRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.SetSetting("execution_tool_contracts_v033", "true"); err != nil {
		t.Fatal(err)
	}
	if err := refreshExecutionToolContracts(pg); err != nil {
		t.Fatal(err)
	}
	upgraded, err := pg.GetTool(key)
	if err != nil || upgraded == nil {
		t.Fatal(err)
	}
	if upgraded.Enabled || !upgraded.Deferred || upgraded.Description != "用户保留的漏洞说明" || !reflect.DeepEqual(upgraded.Agents, []string{"worker"}) {
		t.Fatalf("user preferences changed: %+v", upgraded)
	}
	assertFindingContract := func(schema map[string]any) {
		t.Helper()
		fields := schema["properties"].(map[string]any)
		for _, field := range []string{"evidence_mode", "verification_ref", "evidence_refs"} {
			if fields[field] == nil {
				t.Fatalf("current finding source parameter missing: %s", field)
			}
		}
		if fields["severity"].(map[string]any)["default"] != "low" || fields["summary"].(map[string]any)["description"] != "用户保留的摘要说明" {
			t.Fatal("supported parameter preferences lost")
		}
	}
	var upgradedSchema map[string]any
	_ = json.Unmarshal(upgraded.Schema, &upgradedSchema)
	assertFindingContract(upgradedSchema)
	var before, after time.Time
	if err := pg.QueryRow(`SELECT updated_at FROM tools WHERE key=$1`, key).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := refreshExecutionToolContracts(pg); err != nil {
		t.Fatal(err)
	}
	if err := pg.QueryRow(`SELECT updated_at FROM tools WHERE key=$1`, key).Scan(&after); err != nil || !before.Equal(after) {
		t.Fatalf("repeated startup rewrote unchanged contract: %v %v", before, after)
	}
	oldResolve, oldAugment, oldBinding := agent.ToolResolve, agent.ToolAugment, agent.FindingTrafficBindingEnabled
	t.Cleanup(func() {
		agent.ToolResolve, agent.ToolAugment, agent.FindingTrafficBindingEnabled = oldResolve, oldAugment, oldBinding
	})
	agent.ToolAugment = nil
	agent.FindingTrafficBindingEnabled = func() bool { return false }
	wireTools(pg, nil)
	ts := agent.NewToolSet(nil, "fixture")
	if _, present := names(agent.ToolResolve(t.Context(), "worker", ts.WorkerTools()))[key]; present {
		t.Fatal("disabled finding tool was re-enabled")
	}
	// An old UI/session can persist stale JSON after startup. The actual registry
	// supplied to a newly assembled or resumed worker must still expose code fields.
	if err := pg.UpdateTool(key, upgraded.Description, staleRaw, mustJSON([]string{"worker"}), true); err != nil {
		t.Fatal(err)
	}
	task, err := pg.CreateTask(t.Name(), "纯合成回归，不访问外部目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.DeleteTaskCascade(task.ID, true, true) })
	store := pg.Exploration(task.ExplorationID)
	intent, err := store.AddIntent(map[string]any{"summary": "synthetic material"}, 1, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	activity, err := store.AppendActivity(db.Activity{NodeID: &intent, Worker: "fixture", Kind: "tool_result", Tool: "Bash", ToolUseID: "legacy", Detail: "synthetic actual raw material"})
	if err != nil {
		t.Fatal(err)
	}
	ts = agent.NewToolSet(store, "fixture")
	ts.SetTaskID(task.ID)
	ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{Kind: agent.KindWorker, TaskID: task.ID, ExplorationID: task.ExplorationID, IntentID: intent})
	for _, run := range []string{"new", "resumed"} {
		t.Run(run, func(t *testing.T) {
			tools, info, cleanup := agent.AugmentTools(ctx, "worker", ts.WorkerTools())
			defer cleanup()
			registry := actool.NewRegistry(tools...)
			finding, ok := registry.Get(key)
			if !ok {
				t.Fatal("enabled finding tool missing")
			}
			assertFindingContract(finding.InputSchema())
			if !strings.Contains(info.FindingGuidance, "verification_ref") || !strings.Contains(info.FindingGuidance, "evidence_refs") {
				t.Fatal("actual worker guidance lacks source instructions")
			}
			modelJSON, err := json.Marshal(registry.Schemas())
			if err != nil || !strings.Contains(string(modelJSON), `"evidence_refs"`) {
				t.Fatal("provider-facing registry lost the source contract")
			}
			modelCalls := 0
			for _, err := range harness.Query(ctx, harness.QueryInput{
				Tools: registry, NonStreaming: true, MaxTurns: 1,
				Messages: []llm.Message{llm.UserText("只核对工具契约，不执行任何目标操作")},
			}, harness.QueryDeps{CallModelSync: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				modelCalls++
				found := false
				for _, tool := range req.Tools {
					if tool.Name == key {
						assertFindingContract(tool.InputSchema)
						found = true
					}
				}
				if !found {
					t.Error("actual model request lost finding tool")
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("合成合同核对完成")}}, "end_turn", llm.Usage{}, nil
			}}) {
				if err != nil {
					t.Fatal(err)
				}
			}
			if modelCalls != 1 {
				t.Fatalf("model request capture count=%d", modelCalls)
			}
			input := mustJSON(map[string]any{"vulnclass": "其他", "name": "synthetic contract", "severity": "low", "summary": "合成旧材料登记", "evidence": "合成原件引用", "evidence_mode": "material", "evidence_refs": []db.VerificationEvidence{{Role: "impact", ActivityID: activity, Observation: "纯合成原始回执"}}})
			if err := actool.ValidateInput(finding.InputSchema(), input); err != nil {
				t.Fatal(err)
			}
			result, err := finding.Call(ctx, input, nil)
			if err != nil || result.IsError {
				t.Fatalf("valid material parameters could not reach handler: %+v %v", result, err)
			}
		})
	}
}

func TestLegacyHostToolRefreshPreservesUserAnnotations(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no isolated test database: %v", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	const key = "list_facts"
	var original json.RawMessage
	if err := pg.QueryRow(`SELECT coalesce((SELECT to_jsonb(t) FROM tools t WHERE key=$1),'null'::jsonb)`, key).Scan(&original); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM tools WHERE key=$1`, key)
		if string(original) != "null" {
			_, _ = pg.Exec(`INSERT INTO tools SELECT (jsonb_populate_record(NULL::tools,$1)).*`, original)
		}
	})
	var schema map[string]any
	for _, seed := range agent.BuiltinToolSeeds() {
		if seed.Key == key {
			raw, _ := json.Marshal(seed.Schema)
			_ = json.Unmarshal(raw, &schema)
		}
	}
	fields := schema["properties"].(map[string]any)
	delete(fields, "q")
	fields["limit"].(map[string]any)["default"] = float64(17)
	fields["limit"].(map[string]any)["description"] = "用户的分页说明"
	raw, _ := json.Marshal(schema)
	_, err = pg.Exec(`INSERT INTO tools(key,system,description,schema,agents,enabled,deferred)
VALUES($1,true,'用户的事实查询说明',$2,'["worker"]',false,true)
ON CONFLICT(key) DO UPDATE SET system=true,kind='builtin',description=excluded.description,schema=excluded.schema,agents=excluded.agents,enabled=false,deferred=true`, key, raw)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{pg: pg}}
	s.refreshBuiltinToolSchemas()
	s.refreshBuiltinToolSchemas()
	row, err := pg.GetTool(key)
	if err != nil || row == nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(row.Schema, &schema)
	fields = schema["properties"].(map[string]any)
	limit := fields["limit"].(map[string]any)
	if row.Enabled || !row.Deferred || !reflect.DeepEqual(row.Agents, []string{"worker"}) || row.Description != "用户的事实查询说明" || limit["default"] != float64(17) || limit["description"] != "用户的分页说明" || fields["q"] == nil {
		t.Fatalf("legacy migration overwrote preferences or hid new field: row=%+v schema=%+v", row, schema)
	}
}
