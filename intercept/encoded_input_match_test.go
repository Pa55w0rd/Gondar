package intercept

import (
	"testing"

	"gondar/db"
)

func encodedInputMatcher(target, matchType, pattern string) *Interceptor {
	i := New(nil)
	i.SetRulesInMemory([]db.InterceptRule{{ID: 1, Name: "synthetic input rule", Enabled: true,
		MatchTarget: target, MatchType: matchType, Pattern: pattern, Action: "deny"}})
	return i
}

func TestRuleMatchDecodedInputValues(t *testing.T) {
	for _, input := range []string{
		`{"command":"first\nblocked_action"}`,
		`{"command":"first\tblocked_action"}`,
		`{"command":"\u0062locked_action"}`,
		`{"changes":[{"text":"first\nblocked_action"}]}`,
		`"first\nblocked_action"`,
	} {
		if decision, ok := encodedInputMatcher("tool_input", "regex", `\bblocked_action\b`).Match("Bash", []byte(input)); !ok || decision.Action != "deny" {
			t.Errorf("decoded input was not matched: %s", input)
		}
	}
	for _, tc := range []struct{ input, pattern string }{
		{`{"command":"say \"blocked_action\""}`, `say "blocked_action"`},
		{`{"command":"x \u003e /fixture"}`, `x > /fixture`},
	} {
		if _, ok := encodedInputMatcher("tool_input", "string", tc.pattern).Match("Bash", []byte(tc.input)); !ok {
			t.Errorf("decoded string literal did not match: %s", tc.input)
		}
	}
}

func TestRuleMatchDecodedInputPreservesRawAndFieldBoundaries(t *testing.T) {
	for _, tc := range []struct {
		input, pattern, typ string
		want                bool
	}{
		{`{"command":"allowed"}`, `"command":"allowed"`, "string", true},
		{`not JSON: blocked_action`, `blocked_action`, "string", true},
		{`{"blocked_action":"allowed"}`, `^blocked_action$`, "regex", false},
		{`{"a":"blocked_","b":"action"}`, `blocked_action`, "string", false},
		{`{"command":"first\\nblocked_action"}`, `^first\nblocked_action$`, "regex", false},
		{`{"command":"ordinary_action"}`, `\bblocked_action\b`, "regex", false},
	} {
		if _, ok := encodedInputMatcher("tool_input", tc.typ, tc.pattern).Match("Bash", []byte(tc.input)); ok != tc.want {
			t.Errorf("input=%s pattern=%q matched=%v want=%v", tc.input, tc.pattern, ok, tc.want)
		}
	}
	i := encodedInputMatcher("tool_name", "string", "Read")
	if _, ok := i.Match("Read", []byte(`{"command":"allowed"}`)); !ok {
		t.Fatal("tool-name matching changed")
	}
	if _, ok := i.Match("Bash", []byte(`{"command":"Read"}`)); ok {
		t.Fatal("tool-name rule matched an argument")
	}
}

func TestRuleMatchDecodedInputKeepsPriorityAndDisabledState(t *testing.T) {
	i := New(nil)
	i.SetRulesInMemory([]db.InterceptRule{
		{ID: 1, Name: "disabled", Enabled: false, Priority: 30, MatchTarget: "tool_input", MatchType: "string", Pattern: "blocked_action", Action: "deny"},
		{ID: 2, Name: "first", Enabled: true, Priority: 20, MatchTarget: "tool_name", MatchType: "string", Pattern: "Bash", Action: "allow"},
		{ID: 3, Name: "later", Enabled: true, Priority: 10, MatchTarget: "tool_input", MatchType: "string", Pattern: "blocked_action", Action: "deny"},
	})
	decision, ok := i.Match("Bash", []byte(`{"command":"\u0062locked_action"}`))
	if !ok || decision.RuleID != 2 || decision.Action != "allow" {
		t.Fatalf("rule priority changed: %+v matched=%v", decision, ok)
	}
}
