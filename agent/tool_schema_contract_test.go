package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

func contractSchema(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestToolSchemaContractPreservesNewStructureAndSupportedAnnotations(t *testing.T) {
	code := contractSchema(t, `{"type":"object","properties":{"severity":{"type":"string","enum":["low","high"],"description":"severity"},"limit":{"type":"integer","description":"limit"},"description":{"type":"string"},"default":{"type":"boolean"},"evidence_refs":{"type":"array","items":{"type":"object","properties":{"role":{"type":"string","enum":["test","impact"]}},"required":["role"]}}},"required":["severity","evidence_refs"],"additionalProperties":false,"$defs":{"proof":{"type":"string","enum":["real"]}},"allOf":[{"if":{"required":["severity"]},"then":{"required":["evidence_refs"]}}]}`)
	old := contractSchema(t, `{"type":"object","properties":{"severity":{"type":"string","enum":["low","high"],"default":"low","description":"my severity"},"limit":{"type":"string","default":"old-type"},"description":{"type":"string","description":"a real parameter"},"default":{"type":"boolean","default":true},"removed":{"type":"string","default":"must not inject"}},"required":[],"additionalProperties":true,"$defs":{"proof":{"type":"string","enum":["inferred"]}}}`)
	beforeCode, _ := json.Marshal(code)
	beforeOld, _ := json.Marshal(old)
	got := ReconcileToolSchema(code, old)
	props := got["properties"].(map[string]any)
	severity := props["severity"].(map[string]any)
	if severity["default"] != "low" || severity["description"] != "my severity" {
		t.Fatalf("supported annotations lost: %v", severity)
	}
	if _, ok := props["limit"].(map[string]any)["default"]; ok {
		t.Fatal("old type default retained")
	}
	if _, ok := props["removed"]; ok {
		t.Fatal("removed parameter reintroduced")
	}
	if props["description"].(map[string]any)["description"] != "a real parameter" || props["default"].(map[string]any)["default"] != true {
		t.Fatal("parameter names mistaken for annotations")
	}
	if !sameToolSchemaStructure(got, code) {
		t.Fatalf("required, enum, definitions or conditions changed: %+v", got)
	}
	if got["additionalProperties"] != false || props["evidence_refs"] == nil {
		t.Fatal("new source contract missing")
	}
	afterCode, _ := json.Marshal(code)
	afterOld, _ := json.Marshal(old)
	if string(beforeCode) != string(afterCode) || string(beforeOld) != string(afterOld) {
		t.Fatal("shared schema mutated")
	}
	if !reflect.DeepEqual(got, ReconcileToolSchema(code, got)) {
		t.Fatal("schema reconciliation is not idempotent")
	}
}

func TestToolSchemaContractDiscardsInvalidAndChangedEnumDefaults(t *testing.T) {
	for _, tc := range []struct{ name, code, old string }{
		{"changed enum", `{"type":"string","enum":["low"]}`, `{"type":"string","enum":["high"],"default":"high"}`},
		{"invalid enum", `{"type":"string","enum":["low"]}`, `{"type":"string","enum":["low"],"default":"high"}`},
		{"invalid integer", `{"type":"integer"}`, `{"type":"integer","default":1.5}`},
		{"changed condition", `{"type":"string","allOf":[{"const":"new"}]}`, `{"type":"string","allOf":[{"const":"old"}],"default":"old"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ReconcileToolSchema(contractSchema(t, tc.code), contractSchema(t, tc.old))
			if _, ok := got["default"]; ok {
				t.Fatalf("incompatible default retained: %v", got)
			}
		})
	}
}
