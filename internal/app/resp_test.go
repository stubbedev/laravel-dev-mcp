package app

import (
	"bufio"
	"reflect"
	"strings"
	"testing"
)

func decode(t *testing.T, wire string) any {
	t.Helper()
	v, err := readRESP(bufio.NewReader(strings.NewReader(wire)))
	if err != nil {
		t.Fatalf("readRESP(%q): %v", wire, err)
	}
	return v
}

func TestReadRESP(t *testing.T) {
	if got := decode(t, "+OK\r\n"); got != "OK" {
		t.Fatalf("simple string: %v", got)
	}
	if got := decode(t, ":42\r\n"); got != int64(42) {
		t.Fatalf("integer: %v", got)
	}
	if got := decode(t, "$3\r\nfoo\r\n"); got != "foo" {
		t.Fatalf("bulk: %v", got)
	}
	if got := decode(t, "$-1\r\n"); got != nil {
		t.Fatalf("null bulk: %v", got)
	}
	if got := decode(t, "*2\r\n$3\r\nfoo\r\n:7\r\n"); !reflect.DeepEqual(got, []any{"foo", int64(7)}) {
		t.Fatalf("array: %v", got)
	}
	if _, err := readRESP(bufio.NewReader(strings.NewReader("-ERR boom\r\n"))); err == nil {
		t.Fatal("expected error reply to surface as error")
	}
}
