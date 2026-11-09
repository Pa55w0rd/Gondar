package db

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJsonbClean(t *testing.T) {
	// A marshaled payload carrying a NUL byte (e.g. captured HTTP/tool output).
	b, err := json.Marshal(map[string]string{"body": "ab\x00cd"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `\u0000`) {
		t.Fatalf("precondition: marshaled JSON should contain the NUL escape, got %s", b)
	}

	cleaned := jsonbClean(b)
	if strings.Contains(string(cleaned), `\u0000`) {
		t.Fatalf("jsonbClean left a NUL escape jsonb rejects: %s", cleaned)
	}

	// Result must stay valid JSON with the NUL simply dropped.
	var out map[string]string
	if err := json.Unmarshal(cleaned, &out); err != nil {
		t.Fatalf("cleaned bytes are not valid JSON: %v (%s)", err, cleaned)
	}
	if out["body"] != "abcd" {
		t.Fatalf("expected NUL stripped to \"abcd\", got %q", out["body"])
	}

	// A NUL escape typed literally in source text (doubled backslash) is preserved.
	lit := []byte(`{"body":"\\u0000"}`)
	if got := jsonbClean(lit); string(got) != string(lit) {
		t.Fatalf("literal \\\\u0000 must be untouched, got %s", got)
	}

	// No NUL escape → returned unchanged.
	plain := []byte(`{"body":"hello"}`)
	if got := jsonbClean(plain); string(got) != string(plain) {
		t.Fatalf("plain JSON must be untouched, got %s", got)
	}
}

func TestJsonbCleanUntrustedRawBytes(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"raw nul", "{\"body\":\"a\x00b\"}", "ab"},
		{"invalid utf8", "{\"body\":\"a\xffb\"}", "a�b"},
		{"raw and escaped", "{\"body\":\"a\x00\\u0000\xffb\"}", "a�b"},
		{"literal escape", `{"body":"\\u0000"}`, `\u0000`},
		{"raw NUL splits unicode escape", `{"body":"` + string([]byte{92, 0}) + `u0000"}`, ""},
		{"raw NUL splits literal escape", `{"body":"` + string([]byte{92, 0, 92}) + `u0000"}`, `\u0000`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var decoded map[string]string
			cleaned := jsonbClean([]byte(tc.input))
			if err := json.Unmarshal(cleaned, &decoded); err != nil {
				t.Fatalf("cleaned output is invalid JSON: %v", err)
			}
			if got := decoded["body"]; got != tc.want {
				t.Fatalf("body=%q, want %q", got, tc.want)
			}
			if strings.ContainsRune(string(cleaned), 0) {
				t.Fatal("raw NUL remains in jsonb payload")
			}
		})
	}
}
