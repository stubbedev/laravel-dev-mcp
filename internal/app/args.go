package app

import (
	"flag"
	"fmt"
	"io"
	"os"
)

// cliOptions is the resolved command line. Flags win; the HTTP settings fall
// back to env vars (LARAVEL_MCP_HTTP / _HTTP_ADDR / _HTTP_PATH).
type cliOptions struct {
	version  bool
	httpOn   bool
	httpAddr string
	httpPath string
}

// optHTTP is a flag with an optional value: `--http` enables HTTP on the
// default address, `--http=host:port` overrides it. IsBoolFlag lets the bare
// form work without swallowing the next argument (so `--http=addr` is the way
// to pass an address, not `--http addr`).
type optHTTP struct {
	on   bool
	addr string
}

func (o *optHTTP) String() string { return o.addr }
func (*optHTTP) IsBoolFlag() bool { return true }
func (o *optHTTP) Set(s string) error {
	o.on = true
	if s != literalTrue { // the bare flag passes the literal "true"
		o.addr = s
	}

	return nil
}

// parseCLI parses args (os.Args[1:]) and layers env-var defaults underneath.
// Errors are ready to print as-is; flag.ErrHelp stays matchable with errors.Is.
func parseCLI(args []string) (cliOptions, error) {
	flags := flag.NewFlagSet("laravel-dev-mcp", flag.ContinueOnError)
	flags.SetOutput(io.Discard) // we format our own errors via the caller

	var (
		ver      bool
		http     optHTTP
		httpPath string
	)
	flags.BoolVar(&ver, "version", false, "print version and exit")
	flags.BoolVar(&ver, "v", false, "print version and exit")
	flags.Var(&http, "http", "serve over HTTP; bare to use the default address, or --http=host:port")
	flags.StringVar(&httpPath, "http-path", "", "HTTP endpoint path (default /mcp)")

	err := flags.Parse(args)
	if err != nil {
		return cliOptions{}, fmt.Errorf("invalid arguments: %w", err)
	}

	opt := cliOptions{version: ver, httpOn: http.on, httpAddr: http.addr, httpPath: httpPath}

	// `version` as a bare word (no dash), for parity with the old behavior.
	for _, a := range flags.Args() {
		if a == "version" {
			opt.version = true
		}
	}

	// HTTP can also be switched on by env when no --http flag was given.
	if !opt.httpOn {
		if v := os.Getenv("LARAVEL_MCP_HTTP_ADDR"); v != "" {
			opt.httpOn, opt.httpAddr = true, v
		} else if truthy(os.Getenv("LARAVEL_MCP_HTTP")) {
			opt.httpOn = true
		}
	}

	if opt.httpOn && opt.httpAddr == "" {
		opt.httpAddr = defaultHTTPAddr
	}

	if opt.httpPath == "" {
		opt.httpPath = envOr("LARAVEL_MCP_HTTP_PATH", defaultHTTPPath)
	}

	opt.httpPath = ensureLeadingSlash(opt.httpPath)

	return opt, nil
}
