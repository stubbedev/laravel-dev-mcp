package app

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// sensitiveKeyRe matches config keys whose values must not be surfaced verbatim
// (APP_KEY, mail/DB/service passwords, API secrets, tokens, …).
var sensitiveKeyRe = regexp.MustCompile(
	`(?i)(password|passwd|secret|token|api[_-]?key|private[_-]?key|credential|^key$|_key$|salt|cipher|signing)`,
)

const redacted = "********"

// redactValue deep-copies a config value, masking the values of sensitive keys.
func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if sensitiveKeyRe.MatchString(k) && isNonEmptyScalar(val) {
				out[k] = redacted
			} else {
				out[k] = redactValue(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactValue(val)
		}
		return out
	default:
		return v
	}
}

func isNonEmptyScalar(v any) bool {
	switch v.(type) {
	case map[string]any, []any, nil:
		return false
	default:
		return fmt.Sprint(v) != ""
	}
}

// maxToolText bounds any single tool result so one fat payload (a Telescope
// request dump, a huge tinker echo, a giant package list) can't blow the
// model's context window.
const maxToolText = 200_000

// capResult truncates oversized text blocks in a tool result.
func capResult(r toolResult) toolResult {
	for i := range r.Content {
		if len(r.Content[i].Text) > maxToolText {
			r.Content[i].Text = r.Content[i].Text[:maxToolText] +
				"\n…[truncated: output exceeded " + strconv.Itoa(maxToolText) + " chars]"
		}
	}
	return r
}

// lastSegment returns the final dotted segment of a config key.
func lastSegment(key string) string {
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		return key[i+1:]
	}
	return key
}
