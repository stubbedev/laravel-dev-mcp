package app

import (
	"testing"
)

func TestParseToolsLoadsFullSet(t *testing.T) {
	t.Parallel()

	reg := loadTools()

	if len(reg.tools) < 10 {
		t.Fatalf("expected the full toolset loaded, got %d", len(reg.tools))
	}

	for _, def := range reg.tools {
		if reg.validators[def.Name] == nil {
			t.Fatalf("validator missing for %q", def.Name)
		}

		if reg.handlers[def.Name] == nil {
			t.Fatalf("handler missing for %q", def.Name)
		}
	}
}

func TestParseAuditAdvisories(t *testing.T) {
	t.Parallel()

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
	// What composer actually prints for a clean audit: PHP's empty array.
	if names, ok := parseAuditAdvisories([]byte(`{"advisories":[],"abandoned":[]}`)); !ok || len(names) != 0 {
		t.Fatalf("clean audit as []: got %v ok=%v", names, ok)
	}
}
