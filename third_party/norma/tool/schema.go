package tool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// ValidateInput checks a raw JSON input against a minimal subset of JSON Schema
// sufficient for tool inputs: object type, required properties, and per-property
// primitive type checks (string/number/integer/boolean/array/object). It never
// panics; a non-nil error means the input is invalid (FR-04.5, NFR-07).
func ValidateInput(schema map[string]any, raw json.RawMessage) error {
	if schema == nil {
		return nil
	}
	var v any
	if len(raw) == 0 {
		v = map[string]any{}
	} else {
		if !json.Valid(raw) {
			return fmt.Errorf("input is not valid JSON")
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("input is not valid JSON: %w", err)
		}
	}
	return validateValue(schema, v, "")
}

func validateValue(schema map[string]any, v any, path string) error {
	if branches, ok := schema["anyOf"].([]any); ok {
		matched := false
		for _, raw := range branches {
			branch, ok := raw.(map[string]any)
			if ok && validateValue(branch, v, path) == nil {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: no anyOf branch matches", at(path))
		}
	}
	typ, _ := schema["type"].(string)
	switch typ {
	case "object", "":
		m, ok := v.(map[string]any)
		if !ok {
			if typ == "" {
				return nil
			}
			return fmt.Errorf("%s: expected object", at(path))
		}
		for _, req := range toStrings(schema["required"]) {
			if _, present := m[req]; !present {
				return fmt.Errorf("%s: missing required property %q", at(path), req)
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for key, val := range m {
			ps, ok := props[key].(map[string]any)
			if !ok {
				continue // unknown property — tolerate
			}
			if err := validateValue(ps, val, join(path, key)); err != nil {
				return err
			}
		}
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s: expected string", at(path))
		}
	case "integer":
		n, ok := v.(json.Number)
		if !ok || !exactSchemaInteger(string(n)) {
			return fmt.Errorf("%s: expected integer", at(path))
		}
	case "number":
		n, ok := v.(json.Number)
		number, err := strconv.ParseFloat(string(n), 64)
		if !ok || err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
			return fmt.Errorf("%s: expected number", at(path))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: expected boolean", at(path))
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: expected array", at(path))
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, el := range arr {
				if err := validateValue(items, el, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Integer validation preserves ordinary large integer lexemes without a float
// round-trip. Fraction/exponent forms are bounded before rational conversion so
// a tiny untrusted JSON exponent cannot request an enormous power allocation.
func exactSchemaInteger(text string) bool {
	if !strings.ContainsAny(text, ".eE") {
		return text != ""
	}
	if len(text) > 4096 {
		return false
	}
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		power, err := strconv.ParseInt(text[index+1:], 10, 32)
		if err != nil || power > 1024 || power < -1024 {
			return false
		}
	}
	r, ok := new(big.Rat).SetString(text)
	return ok && r.IsInt()
}

func toStrings(v any) []string {
	if values, ok := v.([]string); ok {
		return values
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func at(path string) string {
	if path == "" {
		return "input"
	}
	return strings.TrimPrefix(path, ".")
}
