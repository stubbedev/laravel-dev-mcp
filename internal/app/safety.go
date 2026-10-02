package app

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// sensitiveKeyRe matches config keys whose values must not be surfaced verbatim
// (APP_KEY, mail/DB/service passwords, API secrets, tokens, …).
var sensitiveKeyRe = regexp.MustCompile(
	`(?i)(password|passwd|secret|token|api[_-]?key|private[_-]?key|credential|^key$|_key$|salt|cipher|signing)`,
)

const redacted = "********"

// redactValue deep-copies a config value, masking the values of sensitive keys.
func redactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, val := range typed {
			if sensitiveKeyRe.MatchString(k) && isNonEmptyScalar(val) {
				out[k] = redacted
			} else {
				out[k] = redactValue(val)
			}
		}

		return out
	case []any:
		out := make([]any, len(typed))
		for i, val := range typed {
			out[i] = redactValue(val)
		}

		return out
	default:
		return value
	}
}

func isNonEmptyScalar(value any) bool {
	switch value.(type) {
	case map[string]any, []any, nil:
		return false
	default:
		return fmt.Sprint(value) != ""
	}
}

// maxToolText bounds any single tool result so one fat payload (a Telescope
// request dump, a huge tinker echo, a giant package list) can't blow the
// model's context window.
const maxToolText = 200_000

// capResult truncates oversized text blocks in a tool result.
func capResult(result toolResult) toolResult {
	for i := range result.Content {
		if len(result.Content[i].Text) > maxToolText {
			result.Content[i].Text = truncateUTF8(result.Content[i].Text, maxToolText) +
				"\n…[truncated: output exceeded " + strconv.Itoa(maxToolText) + " bytes]"
		}
	}

	return result
}

// truncateUTF8 cuts text to at most maxBytes bytes without splitting a UTF-8
// sequence.
func truncateUTF8(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}

	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}

	return text[:cut]
}

// lastSegment returns the final dotted segment of a config key.
func lastSegment(key string) string {
	if _, last, ok := strings.CutLast(key, "."); ok {
		return last
	}

	return key
}
