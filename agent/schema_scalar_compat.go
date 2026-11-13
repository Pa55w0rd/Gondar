package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/tool"
)

type scalarCompatibilityPolicyKey struct{}

// WithScalarCompatibilityPolicy permits a trusted host to disable compatibility
// for individual tools. It does not alter a tool schema or provider messages.
func WithScalarCompatibilityPolicy(ctx context.Context, enabled func(string) bool) context.Context {
	return context.WithValue(ctx, scalarCompatibilityPolicyKey{}, enabled)
}

func withScalarCompatibility(ctx context.Context) context.Context {
	return harness.WithInputNormalizer(ctx, func(ctx context.Context, name string, schema map[string]any, raw json.RawMessage) (json.RawMessage, []string) {
		if enabled, _ := ctx.Value(scalarCompatibilityPolicyKey{}).(func(string) bool); enabled != nil && !enabled(name) {
			return nil, nil
		}
		return normalizeSchemaScalars(schema, raw)
	})
}

var integerScalar = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
var numberScalar = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// The compatibility surface is deliberately smaller than JSON Schema. An
// unsupported constraint prevents normalization, rather than accepting input
// under constraints the local validator cannot prove. Arrays and free strings
// are never repaired heuristically; numeric precision is preserved as Number.
func normalizeSchemaScalars(schema map[string]any, raw json.RawMessage) (json.RawMessage, []string) {
	if !json.Valid(raw) || !scalarSchemaSupported(schema) || tool.ValidateInput(schema, raw) == nil {
		return nil, nil
	}
	value, ok := decodeScalarInput(raw)
	if !ok {
		return nil, nil
	}
	value, paths, changed := repairSchemaScalar(schema, value, "")
	if !changed {
		return nil, nil
	}
	corrected, err := json.Marshal(value)
	if err != nil || tool.ValidateInput(schema, corrected) != nil {
		return nil, nil
	}
	return corrected, paths
}

func decodeScalarInput(raw []byte) (any, bool) {
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	err := dec.Decode(&value)
	return value, err == nil
}

func scalarSchemaSupported(schema map[string]any) bool {
	if schema == nil {
		return false
	}
	for key, value := range schema {
		switch key {
		case "type":
			if _, ok := value.(string); !ok {
				return false
			}
		case "properties":
			props, ok := value.(map[string]any)
			if !ok {
				return false
			}
			for _, raw := range props {
				s, ok := raw.(map[string]any)
				if !ok || !scalarSchemaSupported(s) {
					return false
				}
			}
		case "required":
			switch value.(type) {
			case []any, []string:
			default:
				return false
			}
		case "items":
			s, ok := value.(map[string]any)
			if !ok || !scalarSchemaSupported(s) {
				return false
			}
		case "anyOf":
			branches, ok := value.([]any)
			if !ok || len(branches) == 0 {
				return false
			}
			for _, raw := range branches {
				s, ok := raw.(map[string]any)
				if !ok || !scalarSchemaSupported(s) {
					return false
				}
			}
		case "description", "title", "default", "examples", "$schema":
		default:
			return false
		}
	}
	return true
}

func repairSchemaScalar(schema map[string]any, value any, path string) (any, []string, bool) {
	if branches, ok := schema["anyOf"].([]any); ok {
		var selected any
		var selectedPaths []string
		matches := 0
		for _, raw := range branches {
			branch := raw.(map[string]any)
			snapshot, _ := json.Marshal(value)
			copy, _ := decodeScalarInput(snapshot)
			candidate, paths, changed := repairSchemaScalar(branch, copy, path)
			if !changed {
				continue
			}
			encoded, err := json.Marshal(candidate)
			if err == nil && tool.ValidateInput(branch, encoded) == nil {
				matches++
				selected, selectedPaths = candidate, paths
			}
		}
		if matches != 1 {
			return value, nil, false
		}
		return selected, selectedPaths, true
	}
	typ, _ := schema["type"].(string)
	if s, ok := value.(string); ok {
		switch typ {
		case "boolean":
			if s == "true" {
				return true, []string{path}, true
			}
			if s == "false" {
				return false, []string{path}, true
			}
		case "integer":
			if integerScalar.MatchString(s) {
				return json.Number(s), []string{path}, true
			}
		case "number":
			if numberScalar.MatchString(s) {
				return json.Number(s), []string{path}, true
			}
		}
	}
	if typ != "object" && typ != "" {
		return value, nil, false
	}
	object, ok := value.(map[string]any)
	if !ok {
		return value, nil, false
	}
	props, _ := schema["properties"].(map[string]any)
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var paths []string
	for _, key := range keys {
		property, ok := props[key].(map[string]any)
		if !ok {
			continue
		}
		next := path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
		fixed, changedPaths, changed := repairSchemaScalar(property, object[key], next)
		if changed {
			object[key] = fixed
			paths = append(paths, changedPaths...)
		}
	}
	return object, paths, len(paths) > 0
}
