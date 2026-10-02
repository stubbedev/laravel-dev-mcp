package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	toon "github.com/toon-format/toon-go"
)

// ── Tool result types ───────────────────────────────────────────────────────

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"` //nolint:tagliatelle // MCP wire-format name fixed by the spec
}

func textResult(text string) toolResult {
	return toolResult{Content: []contentBlock{{Type: contentText, Text: text}}, IsError: false}
}

// ── Output format (per-call, carried on context) ────────────────────────────

type formatKey struct{}

func ctxWithFormat(ctx context.Context, format string) context.Context {
	return context.WithValue(ctx, formatKey{}, format)
}

func formatFromCtx(ctx context.Context) string {
	if format, ok := ctx.Value(formatKey{}).(string); ok && format == formatJSON {
		return formatJSON
	}

	return formatTOON
}

// renderString serializes value in the call's output format. TOON is the
// default; on any encoding error it falls back to pretty JSON.
func renderString(ctx context.Context, value any) string {
	if formatFromCtx(ctx) == formatJSON {
		return marshalIndent(value)
	}

	tree, err := toonTree(value)
	if err != nil {
		return marshalIndent(value)
	}

	out, err := toon.MarshalString(tree)
	if err != nil {
		return marshalIndent(value)
	}

	return out
}

// errNonStringKey would mean encoding/json emitted an object key that is not a
// string; renderString then falls back to JSON.
var errNonStringKey = errors.New("toon: non-string object key")

// toonTree rebuilds value from its JSON encoding for the TOON encoder, which
// only reads `toon` struct tags. Going through encoding/json gives both formats
// the same field names and omitempty rules, and objects become toon.Object so
// struct field order survives.
func toonTree(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("toon: encode json: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	return readJSONValue(dec)
}

func readJSONValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("toon: read json: %w", err)
	}

	switch typed := tok.(type) {
	case json.Delim:
		if typed == '[' {
			return readJSONArray(dec)
		}

		return readJSONObject(dec)
	case json.Number:
		return readJSONNumber(typed)
	default: // string, bool, nil
		return typed, nil
	}
}

// readJSONArray reads the elements after an opening '[' through its ']'.
func readJSONArray(dec *json.Decoder) (any, error) {
	arr := []any{}

	for dec.More() {
		elem, err := readJSONValue(dec)
		if err != nil {
			return nil, err
		}

		arr = append(arr, elem)
	}

	_, err := dec.Token() // ]
	if err != nil {
		return nil, fmt.Errorf("toon: read json: %w", err)
	}

	return arr, nil
}

// readJSONObject reads the members after an opening '{' through its '}',
// keeping their order.
func readJSONObject(dec *json.Decoder) (any, error) {
	var obj toon.Object

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("toon: read json: %w", err)
		}

		key, ok := keyTok.(string) // object keys are always strings
		if !ok {
			return nil, errNonStringKey
		}

		elem, err := readJSONValue(dec)
		if err != nil {
			return nil, err
		}

		obj.Fields = append(obj.Fields, toon.Field{Key: key, Value: elem})
	}

	_, err := dec.Token() // }
	if err != nil {
		return nil, fmt.Errorf("toon: read json: %w", err)
	}

	return obj, nil
}

// readJSONNumber keeps integers integral; anything else becomes a float.
func readJSONNumber(num json.Number) (any, error) {
	whole, err := num.Int64()
	if err == nil {
		return whole, nil
	}

	frac, err := num.Float64()
	if err != nil {
		return nil, fmt.Errorf("toon: read json number: %w", err)
	}

	return frac, nil
}

func jsonResult(ctx context.Context, value any) toolResult {
	return textResult(renderString(ctx, value))
}

func marshalIndent(value any) string {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")

	err := enc.Encode(value)
	if err != nil {
		return ""
	}

	return strings.TrimRight(buf.String(), "\n")
}

// ── Argument helpers ─────────────────────────────────────────────────────────

func has(m map[string]any, k string) bool {
	_, ok := m[k]

	return ok
}

func argString(m map[string]any, k string) string {
	s, ok := m[k].(string)
	if !ok {
		return ""
	}

	return s
}

func argBool(m map[string]any, k string) bool {
	b, ok := m[k].(bool)
	if !ok {
		return false
	}

	return b
}

func argInt(m map[string]any, k string) int {
	switch value := m[k].(type) {
	case float64:
		return int(value)
	case json.Number:
		i, _ := value.Int64()

		return int(i)
	case int:
		return value
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(value))

		return i
	}

	return 0
}

func argStrSlice(m map[string]any, k string) []string {
	arr, ok := m[k].([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(arr))
	for _, elem := range arr {
		if s, ok := elem.(string); ok {
			out = append(out, s)
		}
	}

	return out
}

func argClampInt(m map[string]any, k string, def, maximum int) int {
	if !has(m, k) {
		return def
	}

	val := argInt(m, k)
	if val <= 0 {
		return def
	}

	if maximum > 0 && val > maximum {
		return maximum
	}

	return val
}
