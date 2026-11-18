package agent

import (
	"encoding/json"
	"reflect"

	actool "github.com/Autumn-27/norma/tool"
)

// ReconcileToolSchema keeps a built-in handler's complete code contract while
// retaining supported user annotations. Database/UI schemas cannot remove new
// fields, required/enum constraints, definitions or conditional branches.
// Both inputs remain untouched: tools may share their code schema across runs.
func ReconcileToolSchema(code, edited map[string]any) map[string]any {
	out := cloneToolSchema(code)
	walkToolSchemas(out, edited, func(current, old map[string]any) {
		if desc, ok := old["description"].(string); ok {
			current["description"] = desc
		}
		if value, ok := old["default"]; ok && sameToolSchemaStructure(current, old) && validToolScalarDefault(current, value) {
			current["default"] = value
		}
	})
	return out
}

func cloneToolSchema(schema map[string]any) map[string]any {
	raw, _ := json.Marshal(schema)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func sameToolSchemaStructure(a, b map[string]any) bool {
	strip := func(schema map[string]any) map[string]any {
		out := cloneToolSchema(schema)
		walkToolSchemas(out, nil, func(node, _ map[string]any) {
			delete(node, "description")
			delete(node, "default")
		})
		return out
	}
	return reflect.DeepEqual(strip(a), strip(b))
}

// Only scalar defaults are injected by DecorateTool. If a parameter's structural
// contract changed, the old default is discarded rather than reintroducing a
// removed enum value, old type, or old conditional meaning.
func validToolScalarDefault(schema map[string]any, value any) bool {
	switch schema["type"] {
	case "string", "integer", "number", "boolean":
	default:
		return false
	}
	raw, err := json.Marshal(value)
	if err != nil || value == nil || actool.ValidateInput(schema, raw) != nil {
		return false
	}
	if values, ok := schema["enum"].([]any); ok {
		matched := false
		for _, allowed := range values {
			encoded, _ := json.Marshal(allowed)
			matched = matched || string(raw) == string(encoded)
		}
		if !matched {
			return false
		}
	}
	if constant, ok := schema["const"]; ok {
		encoded, _ := json.Marshal(constant)
		return string(raw) == string(encoded)
	}
	return true
}

// Visit schema nodes, not arbitrary JSON maps: properties named "description"
// or "default" are real parameters and must never be treated as annotations.
func walkToolSchemas(code, edited map[string]any, visit func(map[string]any, map[string]any)) {
	if code == nil {
		return
	}
	visit(code, edited)
	for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
		children, _ := code[key].(map[string]any)
		previous, _ := edited[key].(map[string]any)
		for name, raw := range children {
			child, _ := raw.(map[string]any)
			old, _ := previous[name].(map[string]any)
			walkToolSchemas(child, old, visit)
		}
	}
	for _, key := range []string{"items", "additionalItems", "additionalProperties", "unevaluatedProperties", "unevaluatedItems", "contains", "propertyNames", "not", "if", "then", "else", "contentSchema", "allOf", "anyOf", "oneOf", "prefixItems"} {
		if child, ok := code[key].(map[string]any); ok {
			old, _ := edited[key].(map[string]any)
			walkToolSchemas(child, old, visit)
		}
		if children, ok := code[key].([]any); ok {
			previous, _ := edited[key].([]any)
			for i, raw := range children {
				child, _ := raw.(map[string]any)
				var old map[string]any
				if i < len(previous) {
					old, _ = previous[i].(map[string]any)
				}
				walkToolSchemas(child, old, visit)
			}
		}
	}
}
