# AGENTS.md

Guidance for AI agents working on this codebase.

## Project overview

deadpoll is a fast link checker for web applications, written in Go. It uses [colly](https://github.com/gocolly/colly) for HTTP-level crawling — no headless browser. Speed is a core design goal; do not introduce browser-based solutions.

Configuration is modeled after [LinkChecker's `linkcheckerrc`](https://linkchecker.github.io/linkchecker/man/linkcheckerrc.html), but only a subset is implemented. See the feature coverage table in README.md. When adding new config options, check whether LinkChecker has an equivalent and follow its naming/semantics where reasonable.

## Architecture

- `main.go` — CLI flag parsing, config loading, output file setup
- `config.go` — TOML config struct and loader with defaults
- `crawler.go` — Core crawl logic: link extraction, filtering, error reporting
- `config-example.toml` — Annotated config reference

The crawler runs colly in async mode with configurable parallelism. Links are extracted from `<a href>` tags in raw HTML. Errors are written as JSONL.

## Key design decisions

- **No headless browser.** The tool replaces a slower Python-based spider. Performance matters. Do not add browser dependencies (chromedp, rod, playwright, etc.).
- **Parent tracking uses a `sync.Map`, not colly's `Ctx`.** Colly v2.1.0's context is a shared mutable reference passed to child requests, which causes race conditions in async mode. The `parentMap` records the first page that linked to each URL via `LoadOrStore`. If upgrading colly, verify whether `Request.Visit` still passes `Ctx` by reference — if that changes, this workaround could be revisited.
- **The `seen` sync.Map is not redundant with colly's internal dedup.** Colly deduplicates visits internally, but doesn't expose the count. The `seen` map tracks unique discovered URLs so the heartbeat can report a meaningful queue depth (`discovered - checked`). Without it, the counter inflates with every `<a>` tag rather than unique URLs. Do not remove it thinking colly handles this — colly deduplicates requests, but we need the count.
- **AllowedDomains is a safety net.** When `check_extern` is false, `colly.AllowedDomains` is set to the target hostname. This is belt-and-suspenders — the `OnHTML` callback already skips external links, but `AllowedDomains` ensures the crawler cannot escape the target domain even if the filtering logic has a bug (e.g., via redirects or other URL discovery paths). Do not remove this guard.
- **Filtering is regex-based (RE2, not PCRE).** Go's `regexp` package uses RE2 syntax — no lookaheads or lookbehinds. This is why `ignore_unless` exists: it replaces patterns like `^(?!.*/catalog/).*page=` that cannot be expressed in RE2. When helping users write filter patterns, remember this constraint. New filter types should follow the same compile-once-at-startup pattern.

## Conventions

- Output goes to a JSONL file (or stdout). One `Result` struct per error.
- Progress/diagnostics go to stderr.
- Config defaults are set in `loadConfig()`, not scattered across the codebase.
- CLI flags can override or supplement config file values (see `--cookie`).

## Planned work

- **CSS `url()` checking.** Extract and check URLs from stylesheets (`background-image`, `@font-face src`, `@import`, etc.). These should go through the same ignore/check pipeline as HTML links. Resolve URLs relative to the stylesheet location, not the referring page.

## Testing

When adding behavioral changes, write tests first per project convention. The codebase does not yet have tests — adding them is welcome when practical.
