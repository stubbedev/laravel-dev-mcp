package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	IsError bool           `json:"isError,omitempty"`
}

func textResult(t string) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: t}}}
}

// ── Output format (per-call, carried on context) ────────────────────────────

type formatKey struct{}

func ctxWithFormat(ctx context.Context, format string) context.Context {
	return context.WithValue(ctx, formatKey{}, format)
}

func formatFromCtx(ctx context.Context) string {
	if f, _ := ctx.Value(formatKey{}).(string); f == "json" {
		return "json"
	}
	return "toon"
}

// renderString serializes v in the call's output format. TOON is the default;
// on any encoding error it falls back to pretty JSON.
func renderString(ctx context.Context, v any) string {
	if formatFromCtx(ctx) == "json" {
		return marshalIndent(v)
	}
	s, err := toon.MarshalString(v)
	if err != nil {
		return marshalIndent(v)
	}
	return s
}

func jsonResult(ctx context.Context, v any) toolResult {
	return textResult(renderString(ctx, v))
}

func marshalIndent(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimRight(buf.String(), "\n")
}

// ── Argument helpers ─────────────────────────────────────────────────────────

func has(m map[string]any, k string) bool { _, ok := m[k]; return ok }

func argString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func argBool(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

func argInt(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	case int:
		return v
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(v))
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
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func argClampInt(m map[string]any, k string, def, maximum int) int {
	if !has(m, k) {
		return def
	}
	v := argInt(m, k)
	if v <= 0 {
		return def
	}
	if maximum > 0 && v > maximum {
		return maximum
	}
	return v
}
