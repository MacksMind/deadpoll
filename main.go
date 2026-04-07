package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
)

var version = "dev"

func main() {
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `deadpoll - a link checker

Usage: deadpoll [flags] <target-url>

Crawls <target-url>, follows internal links, and reports errors as JSONL.
Progress heartbeat is written to stderr every 5 seconds.
Exit code 0 = no errors, 1 = broken links found or runtime error.

Flags:
`)
		flag.PrintDefaults()
		fmt.Fprint(os.Stderr, `
Output format (one JSON object per line in the output file):
  {"url":"...","status":404,"error":"...","parent":"...","timestamp":"..."}

  url        the broken URL
  status     HTTP status code (0 if connection failed)
  error      description of the failure
  parent     the page that linked to the broken URL
  timestamp  when the error was recorded (RFC 3339)

Config file format (TOML):
  [checking]
  ssl_verify = false        # skip TLS verification (default true)
  threads    = 10           # concurrent requests (default 10)
  max_depth  = 0            # recursion depth, 0 = unlimited (default 0)
  timeout    = "20s"        # per-request timeout (default "20s")

  [cookies]
  _session_id = "value"     # sent with every request

  [filtering]
  check_extern = false      # check external links (default false)
  ignore   = ["pattern"]    # regexes — matching URLs are skipped entirely
  nofollow = ["pattern"]    # regexes — matching URLs are checked but not crawled

  [[filtering.ignore_unless]]
  pattern = "page="         # ignore URLs matching this...
  unless  = "/catalog/"     # ...unless they also match this
`)
	}

	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "", "path to TOML config file")
	outputPath := flag.String("output", "deadpoll-results.jsonl", "path to output JSONL file (- for stdout)")
	cookie := flag.String("cookie", "", "cookie to send with requests (name=value)")
	flag.Parse()

	if *showVersion {
		if version == "dev" {
			if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
				version = info.Main.Version
			}
		}
		fmt.Println("deadpoll", version)
		os.Exit(0)
	}

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: deadpoll [--config config.toml] [--output results.jsonl] [--cookie name=value] <target-url>")
		os.Exit(1)
	}
	targetURL := flag.Arg(0)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: loading config: %v\n", err)
		os.Exit(1)
	}

	// CLI cookie overrides/merges with config.
	if *cookie != "" {
		if cfg.Cookies == nil {
			cfg.Cookies = make(map[string]string)
		}
		name, value, found := strings.Cut(*cookie, "=")
		if !found {
			fmt.Fprintln(os.Stderr, "error: --cookie must be name=value")
			os.Exit(1)
		}
		cfg.Cookies[name] = value
	}

	var output *os.File
	if *outputPath == "-" {
		output = os.Stdout
	} else {
		output, err = os.Create(*outputPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: creating output file: %v\n", err)
			os.Exit(1)
		}
		defer output.Close()
	}

	cr := NewCrawler(cfg, output)
	os.Exit(cr.Run(targetURL))
}
