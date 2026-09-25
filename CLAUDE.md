# sitewatch

## Goal

A small, self-hosted watcher for web pages and listings. It alerts on Telegram when something you want
appears online, such as a particular used bike listed on DriveX, an item back in stock, or a page
changing. It is a public repo. README.md is the user-facing documentation; this file is for working on
the code.

## Build / run

- Go 1.24. Build with `GOTOOLCHAIN=local go build -o sitewatch .` and keep dependencies compatible with
  Go 1.24.
- `./sitewatch [-dir DIR]` runs the watchers, the web app and the Telegram bot. The default port is 8478.
- `./sitewatch preview <watcher>` runs one extraction for tuning selectors. It sends no alerts and saves
  no state.
- `./sitewatch rebaseline` (service stopped) marks what is listed now as seen.
- `./sitewatch telegram-setup <token>` (service stopped) adds the last chat that messaged the bot to
  `chat_ids`.
- Test with `go test ./...`. For anything bigger, run against a scratch `-dir` that has its own
  config.json (a different port, no secrets.json, and an `auth_token` of 16+ characters).

## Layout

- `config.json`: the example config, which is committed. It is hot-reloaded every 30s, except
  host/port/auth_token.
- `secrets.json` (gitignored, 0600) holds `auth_token` and `telegram` (`{bot_token, chat_ids}`). It is
  overlaid on config.json.
- `main.go`: flags, boot, and the preview, rebaseline and telegram-setup subcommands.
- `config.go`: config structs and `Watcher.expand` (defaults).
- `watcher.go`: `Manager`, with one goroutine per watcher doing fetch → extract → evaluate → alert. It
  also handles new_items state, `absorb` for quiet filter edits, and hot reload.
- `render.go`: headless Chrome rendering and CSS `items` extraction.
- `notify.go`: the events.jsonl, notify_file, ntfy and Telegram sinks, and the Telegram `/health` bot.
- `server.go`: the PWA and JSON API. `web/` is embedded with `go:embed`, so rebuild after editing it.
- `sitewatch.service`: the systemd user unit.
- Runtime files (gitignored): `state.json`, `events.jsonl`, `notifications.txt`.

## Gotchas

- Never log errors from Telegram requests as-is. Go's `url.Error` includes the URL, which contains the
  bot token.
- The `/health` loop consumes `getUpdates`, so `telegram-setup` only works while the service is stopped.
  While it runs, new chats show up as `telegram: unlisted chat <id>` in the log.
- An empty page never wipes a baseline, and zero selector matches gives `selector_missing`. This
  prevents a flood of "new" alerts after a glitch.
- Stop the service before changing line formats or filter semantics in code. Hot reload would otherwise
  feed the new config to the old binary. The order is stop, build, `rebaseline`, start.
- Keep example watchers generic (bikes, stock, page changes). No personal data goes in committed files.
