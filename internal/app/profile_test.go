package app

import (
	"net/http"
	"testing"
)

func TestNormalizeSQL(t *testing.T) {
	got := normalizeSQL("select *  from users where id = 5 and name = 'bob'")
	want := "select * from users where id = ? and name = ?"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSummarizeQueriesNPlusOne(t *testing.T) {
	q := summarizeQueries([]rawQuery{
		{"select * from posts", 5},
		{"select * from users where id = 1", 2},
		{"select * from users where id = 2", 3},
		{"select * from users where id = 3", 1},
	})
	if q.Count != 4 {
		t.Fatalf("count %d", q.Count)
	}
	if q.TotalMs != 11 {
		t.Fatalf("total %v", q.TotalMs)
	}
	// The three users-by-id queries normalize to one group ⇒ N+1 of 3.
	if len(q.NPlusOne) != 1 || q.NPlusOne[0].Count != 3 {
		t.Fatalf("n+1: %+v", q.NPlusOne)
	}
	if len(q.Slowest) == 0 || q.Slowest[0].Ms != 5 {
		t.Fatalf("slowest: %+v", q.Slowest)
	}
}

func TestParseClockwork(t *testing.T) {
	pr, ok := parseClockwork([]byte(`{
		"id":"abc","method":"GET","uri":"/users","responseStatus":200,"responseDuration":42.5,
		"databaseQueries":[{"query":"select * from users","duration":3.0},{"query":"select * from users","duration":1.0}]
	}`))
	if !ok || pr.Method != http.MethodGet || pr.URI != "/users" || pr.DurationMs != 42.5 {
		t.Fatalf("clockwork parse: %+v ok=%v", pr, ok)
	}
	if pr.Queries.Count != 2 || len(pr.Queries.NPlusOne) != 1 {
		t.Fatalf("clockwork queries: %+v", pr.Queries)
	}
}

func TestParseDebugbar(t *testing.T) {
	// Debugbar durations are in seconds; expect ms in the output.
	pr, ok := parseDebugbar([]byte(`{
		"__meta":{"id":"x","method":"POST","uri":"/save"},
		"time":{"duration":0.25},
		"queries":{"statements":[{"sql":"insert into t values (1)","duration":0.01}]}
	}`))
	if !ok || pr.Method != http.MethodPost || pr.DurationMs != 250 {
		t.Fatalf("debugbar parse: %+v ok=%v", pr, ok)
	}
	if pr.Queries.Count != 1 || pr.Queries.TotalMs != 10 {
		t.Fatalf("debugbar queries: %+v", pr.Queries)
	}
}

func TestDecodeMaybe(t *testing.T) {
	if m, ok := decodeMaybe(`{"a":1}`).(map[string]any); !ok || m["a"] != float64(1) {
		t.Fatalf("json object not decoded: %v", decodeMaybe(`{"a":1}`))
	}
	if decodeMaybe("plain text") != "plain text" {
		t.Fatal("plain string should pass through")
	}
}
