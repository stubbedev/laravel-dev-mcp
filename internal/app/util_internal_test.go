package app

import (
	"strings"
	"testing"
)

// TOON output must use the same field names and omitempty rules as JSON, and
// keep struct field order.
func TestRenderTOONUsesJSONTags(t *testing.T) {
	t.Parallel()

	type row struct {
		BatchID string `json:"batch_id"`
		Name    string `json:"name"`
		Note    string `json:"note,omitempty"`
	}

	got := renderString(t.Context(), []row{{BatchID: "b1", Name: "x", Note: ""}, {BatchID: "b2", Name: "y", Note: ""}})
	if want := "[2]{batch_id,name}:"; !strings.HasPrefix(got, want) {
		t.Errorf("TOON header = %q, want prefix %q", got, want)
	}

	if strings.Contains(got, "BatchID") || strings.Contains(got, "note") {
		t.Errorf("TOON output ignores json tags: %q", got)
	}
}
