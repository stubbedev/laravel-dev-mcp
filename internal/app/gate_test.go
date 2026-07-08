package app

import (
	"testing"
)

func TestParseToolsLoadsFullSet(t *testing.T) {
	parseTools()

	if len(allTools) < 10 {
		t.Fatalf("expected the full toolset loaded, got %d", len(allTools))
	}
	for _, d := range allTools {
		if validators[d.Name] == nil {
			t.Fatalf("validator missing for %q", d.Name)
		}
	}
}

func TestParseAuditAdvisories(t *testing.T) {
	names, ok := parseAuditAdvisories([]byte(`{"advisories":{"foo/bar":[{"cve":"x"}],"baz/qux":[]}}`))
	if !ok {
		t.Fatal("expected valid audit JSON to parse")
	}
	if len(names) != 2 || names[0] != "baz/qux" || names[1] != "foo/bar" {
		t.Fatalf("got %v (want sorted [baz/qux foo/bar])", names)
	}
	if _, ok := parseAuditAdvisories([]byte("not json")); ok {
		t.Fatal("expected parse failure on non-JSON")
	}
	if names, ok := parseAuditAdvisories([]byte(`{"advisories":{}}`)); !ok || len(names) != 0 {
		t.Fatalf("clean audit: got %v ok=%v", names, ok)
	}
}
