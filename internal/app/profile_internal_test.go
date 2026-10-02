package app

import (
	"net/http"
	"testing"
)

func TestNormalizeSQL(t *testing.T) {
	t.Parallel()

	got := normalizeSQL("select id,  name from users where id = 5 and name = 'bob'")

	want := "select id, name from users where id = ? and name = ?"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSummarizeQueriesNPlusOne(t *testing.T) {
	t.Parallel()

	queries := summarizeQueries([]rawQuery{
		{"select id from posts", 5},
		{"select id from users where id = 1", 2},
		{"select id from users where id = 2", 3},
		{"select id from users where id = 3", 1},
	})
	if queries.Count != 4 {
		t.Fatalf("count %d", queries.Count)
	}

	if queries.TotalMs != 11 {
		t.Fatalf("total %v", queries.TotalMs)
	}
	// The three users-by-id queries normalize to one group ⇒ N+1 of 3.
	if len(queries.NPlusOne) != 1 || queries.NPlusOne[0].Count != 3 {
		t.Fatalf("n+1: %+v", queries.NPlusOne)
	}

	if len(queries.Slowest) == 0 || queries.Slowest[0].Ms != 5 {
		t.Fatalf("slowest: %+v", queries.Slowest)
	}
}

func TestParseClockwork(t *testing.T) {
	t.Parallel()

	req, parsed := parseClockwork([]byte(`{
		"id":"abc","method":"GET","uri":"/users","responseStatus":200,"responseDuration":42.5,
		"databaseQueries":[{"query":"select id from users","duration":3.0},{"query":"select id from users","duration":1.0}]
	}`))
	if !parsed || req.Method != http.MethodGet || req.URI != "/users" || req.DurationMs != 42.5 {
		t.Fatalf("clockwork parse: %+v parsed=%v", req, parsed)
	}

	if req.Queries.Count != 2 || len(req.Queries.NPlusOne) != 1 {
		t.Fatalf("clockwork queries: %+v", req.Queries)
	}
}

func TestParseDebugbar(t *testing.T) {
	t.Parallel()

	// Debugbar durations are in seconds; expect ms in the output.
	req, parsed := parseDebugbar([]byte(`{
		"__meta":{"id":"x","method":"POST","uri":"/save"},
		"time":{"duration":0.25},
		"queries":{"statements":[{"sql":"insert into t values (1)","duration":0.01}]}
	}`))
	if !parsed || req.Method != http.MethodPost || req.DurationMs != 250 {
		t.Fatalf("debugbar parse: %+v parsed=%v", req, parsed)
	}

	if req.Queries.Count != 1 || req.Queries.TotalMs != 10 {
		t.Fatalf("debugbar queries: %+v", req.Queries)
	}
}

func TestDecodeMaybe(t *testing.T) {
	t.Parallel()

	if m, ok := decodeMaybe(`{"a":1}`).(map[string]any); !ok || m["a"] != float64(1) {
		t.Fatalf("json object not decoded: %v", decodeMaybe(`{"a":1}`))
	}

	if decodeMaybe("plain text") != "plain text" {
		t.Fatal("plain string should pass through")
	}
}
