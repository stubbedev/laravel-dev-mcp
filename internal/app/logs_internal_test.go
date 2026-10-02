package app

import (
	"reflect"
	"testing"
)

func TestChannelLogPaths(t *testing.T) {
	t.Parallel()

	channels := map[string]any{
		driverStack:  map[string]any{keyDriver: driverStack, "channels": []any{driverSingle, driverDaily}},
		driverSingle: map[string]any{keyDriver: driverSingle, keyPath: "/app/storage/logs/laravel.log"},
		driverDaily:  map[string]any{keyDriver: driverDaily, keyPath: "/app/storage/logs/app.log"},
		"syslog":     map[string]any{keyDriver: "syslog"},
	}

	got := channelLogPaths(channels, driverStack, map[string]bool{})

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
	t.Parallel()

	// A stack that references itself must terminate.
	channels := map[string]any{
		driverStack:  map[string]any{keyDriver: driverStack, "channels": []any{driverStack, driverSingle}},
		driverSingle: map[string]any{keyDriver: driverSingle, keyPath: "/x/laravel.log"},
	}

	got := channelLogPaths(channels, driverStack, map[string]bool{})
	if !reflect.DeepEqual(got, []string{"/x/laravel.log"}) {
		t.Fatalf("cycle: got %v", got)
	}
}

func TestDailyGlob(t *testing.T) {
	t.Parallel()

	if g := dailyGlob("/logs/laravel.log"); g != "/logs/laravel-*.log" {
		t.Fatalf("got %q", g)
	}
}
