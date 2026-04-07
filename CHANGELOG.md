# Changelog

## Unreleased

- Check resource URLs: `<img>`, `<script>`, `<link>`, `<source>`, `<video>`, `<audio>`, `<object>`, `<embed>` (including `srcset` parsing)

## v0.1.2 — 2026-04-07

- Add `--version` flag

## v0.1.1 — 2026-04-07

- Update all dependencies (Colly v2.1.0 -> v2.3.0, toml, x/net, x/text, protobuf, xpath)
- Filter out Colly v2.3.0 "already visited" and "Forbidden domain" noise from error output
- Add documentation for RE2 regex limitations, `check_extern` behavior, and `AllowedDomains` safety net

## v0.1.0 — 2026-04-07

Initial release.

- Concurrent link checker using Colly, configurable via TOML
- `ignore` and `nofollow` regex-based URL filtering
- `ignore_unless` compound rules for conditional filtering (RE2-compatible alternative to lookaheads)
- `check_extern` option with `AllowedDomains` safety net
- Cookie support via config file and `--cookie` CLI flag
- JSONL error output designed for agent consumption
- 5-second progress heartbeat to stderr
- Parent URL tracking via `sync.Map` (workaround for Colly shared `Ctx` bug)
