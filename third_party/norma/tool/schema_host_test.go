package tool

import "testing"

func TestHostSchemaExactNumbersAnyOfAndRequired(t *testing.T) {
	for _, tc := range []struct {
		typ, raw string
		valid    bool
	}{{"integer", `9007199254740993`, true}, {"integer", `1.0000000000000001`, false}, {"integer", `1e2`, true}, {"integer", `1e2000000000`, false}, {"number", `1e2000000000`, false}, {"number", `1.5`, true}} {
		err := ValidateInput(map[string]any{"type": tc.typ}, []byte(tc.raw))
		if (err == nil) != tc.valid {
			t.Fatalf("%s %s %v", tc.typ, tc.raw, err)
		}
	}
	if ValidateInput(map[string]any{"type": "object", "required": []string{"needed"}}, []byte(`{}`)) == nil {
		t.Fatal("[]string必填字段被漏过")
	}
	schema := map[string]any{"anyOf": []any{map[string]any{"type": "boolean"}, map[string]any{"type": "integer"}}}
	if ValidateInput(schema, []byte(`"3"`)) == nil {
		t.Fatal("anyOf不匹配未拒绝")
	}
	if err := ValidateInput(schema, []byte(`3`)); err != nil {
		t.Fatal(err)
	}
}
