package app

import (
	"reflect"
	"testing"
)

func TestChannelLogPaths(t *testing.T) {
	channels := map[string]any{
		"stack":  map[string]any{"driver": "stack", "channels": []any{"single", "daily"}},
		"single": map[string]any{"driver": "single", "path": "/app/storage/logs/laravel.log"},
		"daily":  map[string]any{"driver": "daily", "path": "/app/storage/logs/app.log"},
		"syslog": map[string]any{"driver": "syslog"},
	}

	got := channelLogPaths(channels, "stack", map[string]bool{})
	want := []string{"/app/storage/logs/laravel.log", "/app/storage/logs/app.log"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stack: got %v want %v", got, want)
	}

	if p := channelLogPaths(channels, "syslog", map[string]bool{}); p != nil {
		t.Fatalf("non-file channel should yield no paths, got %v", p)
	}
	if p := channelLogPaths(channels, "missing", map[string]bool{}); p != nil {
		t.Fatalf("unknown channel should yield no paths, got %v", p)
	}
}

func TestChannelLogPathsCycle(t *testing.T) {
	// A stack that references itself must terminate.
	channels := map[string]any{
		"stack":  map[string]any{"driver": "stack", "channels": []any{"stack", "single"}},
		"single": map[string]any{"driver": "single", "path": "/x/laravel.log"},
	}
	got := channelLogPaths(channels, "stack", map[string]bool{})
	if !reflect.DeepEqual(got, []string{"/x/laravel.log"}) {
		t.Fatalf("cycle: got %v", got)
	}
}

func TestDailyGlob(t *testing.T) {
	if g := dailyGlob("/logs/laravel.log"); g != "/logs/laravel-*.log" {
		t.Fatalf("got %q", g)
	}
}
