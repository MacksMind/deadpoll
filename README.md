# deadpoll

A fast, concurrent link checker for web applications. Crawls a target site, follows internal links, and reports broken URLs as JSONL.

Built with [colly](https://github.com/gocolly/colly) for speed — HTTP-level crawling with no browser overhead.

## Background

deadpoll replaces [LinkChecker](https://linkchecker.github.io/linkchecker/) for our use case. LinkChecker threads HTTP retrieval but parses sequentially, which becomes the bottleneck when hitting local targets where network latency is negligible. deadpoll parallelizes both retrieval and parsing via colly's async mode and Go's concurrency model.

Configuration is modeled after LinkChecker's [`linkcheckerrc`](https://linkchecker.github.io/linkchecker/man/linkcheckerrc.html) format. Only a subset of features are implemented — the ones needed for the current use case. The table below shows what maps over and what doesn't.

### Feature coverage vs LinkChecker

| LinkChecker feature | deadpoll | Notes |
|---|---|---|
| **[checking]** `threads` | `[checking] threads` | Same concept |
| **[checking]** `timeout` | `[checking] timeout` | Go duration string instead of integer seconds |
| **[checking]** `recursionlevel` | `[checking] max_depth` | 0 = unlimited in both |
| **[checking]** `sslverify` | `[checking] ssl_verify` | Same concept |
| **[filtering]** `checkextern` | `[filtering] check_extern` | Same concept |
| **[filtering]** `ignore` | `[filtering] ignore` | Regex list, same concept |
| **[filtering]** `nofollow` | `[filtering] nofollow` | Regex list, same concept |
| **[authentication]** `entry` | `[cookies]` / `--cookie` | Simplified to raw cookie values instead of HTTP auth |
| **[filtering]** `ignore_unless` | `[filtering] ignore_unless` | **deadpoll extension** — see [Regex notes](#regex-notes) |
| **[checking]** `useragent` | — | Not implemented |
| **[checking]** `maxrunseconds` | — | Not implemented |
| **[checking]** `robotstxt` | — | Not implemented |
| **[checking]** `cookiefile` | — | Not implemented |
| **[authentication]** `loginurl` | — | Not implemented |
| **[output]** section | — | deadpoll always outputs JSONL |
| **[text/html/csv/xml/...]** | — | Single output format (JSONL) |
| Plugin sections | — | Not implemented |

## Install

Requires Go 1.23+.

```sh
go install github.com/MacksMind/deadpoll@latest
```

Or build from source:

```sh
go build -o deadpoll .
```

## Usage

```sh
deadpoll [flags] <target-url>
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--config` | | Path to TOML config file |
| `--output` | `deadpoll-results.jsonl` | Output file (`-` for stdout) |
| `--cookie` | | Cookie sent with every request (`name=value`) |

### Examples

```sh
# Basic crawl
deadpoll https://example.com

# With config and auth cookie
deadpoll --config mysite.toml --cookie "_session_id=abc123" https://example.com

# Output to stdout
deadpoll --output - https://example.com
```

## Output

Errors are written as JSONL (one JSON object per line):

```json
{"url":"https://example.com/missing","status":404,"error":"Not Found","parent":"https://example.com/page","timestamp":"2026-04-07T00:13:06-04:00"}
```

| Field | Description |
|-------|-------------|
| `url` | The broken URL |
| `status` | HTTP status code (0 if connection failed) |
| `error` | Description of the failure |
| `parent` | The page that linked to the broken URL |
| `timestamp` | When the error was recorded (RFC 3339) |

A progress heartbeat prints to stderr every 5 seconds. Exit code is 0 for clean runs, 1 if any errors were found.

The JSONL format is a deliberate choice for automated workflows. The intended pattern is to run deadpoll, then have an AI agent read the output file and propose fixes. This is why there's a single output format rather than the HTML/CSV/text options that LinkChecker provides.

## Configuration

See [`config-example.toml`](config-example.toml) for a fully annotated config file with patterns for common scenarios (session-mutating endpoints, destructive GET actions, pagination, etc.).

### `check_extern`

When `false` (default), external links are silently skipped — not checked, not reported. When `true`, external links are checked (an HTTP request is made to verify they respond) but not crawled — the crawler will not follow links found on external pages. As a safety measure, `AllowedDomains` is set at the colly level when `check_extern` is false, ensuring the crawler cannot escape the target domain even if the filtering logic has a bug.

### Regex notes

All patterns (`ignore`, `nofollow`, `ignore_unless`) use Go's [regexp](https://pkg.go.dev/regexp/syntax) package, which implements RE2 syntax. This means **no lookaheads or lookbehinds** — patterns like `^(?!.*/catalog/).*page=` will fail with a parse error.

The `ignore_unless` rule exists specifically to solve this. Instead of a negative lookahead, express it as two conditions:

```toml
# "Ignore page= unless the URL also contains /catalog/"
[[filtering.ignore_unless]]
pattern = "page="
unless  = "/catalog/"
```

## How it works

deadpoll is an HTTP-level crawler, not a browser. It fetches pages, parses the HTML for `<a href="...">` links, and follows them. This makes it fast but means it:

- **Does** find all links present in the HTML, regardless of CSS visibility (hidden navs, collapsed menus, etc.)
- **Does not** execute JavaScript — links injected by JS at runtime will be missed
- **Does not** check resources referenced in CSS (`background-image: url(...)`, `@font-face`, etc.)

## Known limitations

- **No JavaScript execution.** Single-page apps or JS-rendered navigation won't be crawled. This is a deliberate trade-off for speed.
- **No CSS resource checking.** URLs inside stylesheets (`url()` references for images, fonts, etc.) are not currently checked. A prior Python-based tool caught these; adding CSS `url()` extraction is planned.
- **No viewport/responsive awareness.** Since deadpoll works at the HTTP/HTML level rather than rendering pages, viewport size is irrelevant — it sees the full DOM regardless of breakpoints.

## License

MIT
