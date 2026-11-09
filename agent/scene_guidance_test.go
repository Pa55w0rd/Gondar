package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func sceneTestTools(names ...string) []actool.CoreTool {
	out := make([]actool.CoreTool, 0, len(names))
	for _, name := range names {
		out = append(out, readTool(name, "fixture", nil, func(context.Context, json.RawMessage) (actool.Result, error) {
			return actool.Text("fixture"), nil
		}))
	}
	return out
}

func TestSceneGuidanceUsesOnlyFinalVisibleTools(t *testing.T) {
	full := sceneToolGuidance("worker", sceneTestTools("SearchExtraTools", "ExecuteExtraTool", "Read", "Grep", "traffic_search", "traffic_get", "report_finding"))
	for _, want := range []string{"SearchExtraTools", "ExecuteExtraTool", "Grep", "Read", "traffic_search", "traffic_get", "report_finding", "逐目标"} {
		if !strings.Contains(full, want) {
			t.Fatalf("missing %s", want)
		}
	}
	limited := sceneToolGuidance("worker", sceneTestTools("Read"))
	for _, absent := range []string{"SearchExtraTools", "ExecuteExtraTool", "Grep", "traffic_search", "traffic_get", "report_finding"} {
		if strings.Contains(limited, absent) {
			t.Fatalf("disabled tool %s advertised", absent)
		}
	}
	if sceneToolGuidance("worker", nil) != "" || sceneToolGuidance("reporter", sceneTestTools("Read")) != "" {
		t.Fatal("empty/non-task tools gained scenario guidance")
	}
}

func TestSceneGuidanceIsAppliedAfterToolResolve(t *testing.T) {
	oldResolve, oldAugment := ToolResolve, ToolAugment
	t.Cleanup(func() { ToolResolve, ToolAugment = oldResolve, oldAugment })
	ToolAugment = nil
	ToolResolve = func(context.Context, string, []actool.CoreTool) []actool.CoreTool { return sceneTestTools("Read") }
	tools, def, cleanup := AugmentTools(t.Context(), "planner", sceneTestTools("SearchExtraTools", "ExecuteExtraTool"))
	defer cleanup()
	if len(tools) != 1 || tools[0].Name() != "Read" || strings.Contains(def.FindingGuidance, "SearchExtraTools") {
		t.Fatal("scene guidance restored a filtered tool")
	}
	if !strings.Contains(def.FindingGuidance, "前置依赖") {
		t.Fatal("planner card guidance not connected")
	}
}
