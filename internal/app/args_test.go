package app

import "testing"

func TestParseCLI(t *testing.T) {
	// Keep env from leaking into the flag-precedence cases.
	t.Setenv("LARAVEL_MCP_HTTP", "")
	t.Setenv("LARAVEL_MCP_HTTP_ADDR", "")
	t.Setenv("LARAVEL_MCP_HTTP_PATH", "")

	check := func(name string, args []string, want cliOptions) {
		t.Helper()
		got, err := parseCLI(args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Fatalf("%s: got %+v want %+v", name, got, want)
		}
	}

	check("none", nil, cliOptions{httpPath: defaultHTTPPath})
	check("version flag", []string{"--version"}, cliOptions{version: true, httpPath: defaultHTTPPath})
	check("version short", []string{"-v"}, cliOptions{version: true, httpPath: defaultHTTPPath})
	check("version word", []string{"version"}, cliOptions{version: true, httpPath: defaultHTTPPath})
	check("http bare", []string{"--http"}, cliOptions{httpOn: true, httpAddr: defaultHTTPAddr, httpPath: defaultHTTPPath})
	check("http addr", []string{"--http=1.2.3.4:9"}, cliOptions{httpOn: true, httpAddr: "1.2.3.4:9", httpPath: defaultHTTPPath})
	check("http path leading slash", []string{"--http", "--http-path=mcp2"},
		cliOptions{httpOn: true, httpAddr: defaultHTTPAddr, httpPath: "/mcp2"})

	if _, err := parseCLI([]string{"--nope"}); err == nil {
		t.Fatal("expected error on unknown flag")
	}
}

func TestParseCLIEnvFallback(t *testing.T) {
	t.Setenv("LARAVEL_MCP_HTTP", "1")
	t.Setenv("LARAVEL_MCP_HTTP_ADDR", "")
	t.Setenv("LARAVEL_MCP_HTTP_PATH", "/custom")

	got, err := parseCLI(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.httpOn || got.httpAddr != defaultHTTPAddr || got.httpPath != "/custom" {
		t.Fatalf("env fallback: got %+v", got)
	}
}
