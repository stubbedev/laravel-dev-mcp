package app

import "testing"

func TestParseToolsSplitsGate(t *testing.T) {
	parseTools()

	if gateDef.Name != gateToolName {
		t.Fatalf("gate not parsed: got %q", gateDef.Name)
	}
	if validators[gateToolName] == nil {
		t.Fatal("gate validator missing")
	}
	if len(gatedTools) < 10 {
		t.Fatalf("expected the full toolset gated, got %d", len(gatedTools))
	}
	for _, d := range gatedTools {
		if d.Name == gateToolName {
			t.Fatal("gate leaked into the gated set")
		}
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
