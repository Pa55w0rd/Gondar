package harness

import (
	"context"
	"encoding/json"
)

// InputNormalizer is a host-only, single-pass compatibility hook. It is called
// only after complete JSON fails the original schema validation. Returning nil
// keeps the original error. Returned input is schema-checked again before any
// permission/hook/tool sees it; it never rewrites the provider's stored block.
type InputNormalizer func(context.Context, string, map[string]any, json.RawMessage) (json.RawMessage, []string)
type inputNormalizerKey struct{}

func WithInputNormalizer(ctx context.Context, normalizer InputNormalizer) context.Context {
	return context.WithValue(ctx, inputNormalizerKey{}, normalizer)
}
func inputNormalizer(ctx context.Context) InputNormalizer {
	normalizer, _ := ctx.Value(inputNormalizerKey{}).(InputNormalizer)
	return normalizer
}
