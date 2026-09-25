# sitewatch

A small self-hosted watcher for web pages and listings. It checks pages on a schedule and pings you on
Telegram when something you care about shows up. For example, a used bike you want gets listed, an item
comes back in stock, or a page changes.

- One Go binary with no database. State is a couple of JSON files next to the config.
- Alerts go to Telegram (any number of chats), [ntfy](https://ntfy.sh), and a local text file.
- Pages that only fill in with JavaScript are loaded in headless Chrome.
- A small phone web app (PWA) shows watcher health and recent alerts. It's meant to be reached over
  Tailscale or a LAN.
- Send `/health` to the Telegram bot for a summary of all watchers.

## Quick start

```sh
go build -o sitewatch .
echo '{"auth_token": "'$(openssl rand -hex 24)'"}' > secrets.json && chmod 600 secrets.json
./sitewatch
```

Open `http://localhost:8478` and paste the `auth_token` from `secrets.json`. The bundled `demo-local`
watcher alerts once the demo page contains its keyword. Open `http://localhost:8478/demo-page?on=1` to
see what that looks like. The watcher itself always fetches the page without `?on=1`, so it never fires
on its own.

## Watchers

Everything lives in `config.json`, which is reloaded every 30 seconds, so there's no need to restart
after editing it. Each watcher has a `name`, a `url` and an `interval` in seconds (default 300).

### Listings: alert on new items

This is the main use. `items` picks the listings out of the page with CSS selectors, and each new
listing that passes the filter triggers an alert. The first check records what is already there
without alerting.

```json
{
  "name": "drivex-assured-blr",
  "url": "https://www.drivex.in/buy-used-assured-bikes-in-bangalore",
  "interval": 1800,
  "items": {
    "title": "p[class*='max-w-[220px]']",
    "detail": "p[class*='text-neutral300'], p[class*='text-[18px]'][class*='text-neutral900']"
  },
  "include": ["duke", "\\br15(\\b|m)", "himalayan", "classic 350"]
}
```

- `items.title` is required. `detail` can match several elements, and their texts are joined with " · ".
  `link` defaults to the nearest link. `item` optionally selects each listing's card.
- `include` and `exclude` are case-insensitive regular expressions (RE2) matched against the title. A
  top-level `filter` supplies defaults for every watcher that doesn't set its own.
- Listings are remembered forever, so one that drops off the first page and comes back doesn't alert
  twice.
- Editing a filter doesn't cause an alert storm. Listings that match only because of the edit are
  marked as seen quietly.
- If nothing matches the selectors at all, the status becomes `selector_missing`. That usually means
  the site changed its layout.

### Single page

Use `selector` (optional CSS) together with a `mode`:

| mode           | alerts when                                           |
|----------------|-------------------------------------------------------|
| `changed`      | the text changes (the default)                        |
| `contains`     | any of `keywords` appears                             |
| `not_contains` | none of `keywords` is present, e.g. "Out of stock"    |
| `regex`        | `pattern` (RE2) matches                               |

For example, an item coming back in stock:

```json
{ "name": "restock", "url": "https://shop.example/item", "selector": ".buy-box",
  "mode": "not_contains", "keywords": ["Out of stock", "Sold out"] }
```

### JavaScript pages

Add `"render": true` to load the page in headless Chrome or Chromium first. It is found on `PATH`, or
set `"chrome"` in the config. Only one browser runs at a time, and each render gets a throwaway profile.
Give render watchers a longer interval, such as 1800 seconds or more.

### Tuning selectors

`./sitewatch preview <watcher>` runs one extraction and prints the result. It sends no alerts and saves
no state.

## Telegram

1. Create a bot with [@BotFather](https://t.me/BotFather), open it, and press **Start**.
2. With sitewatch stopped, run `./sitewatch telegram-setup <bot_token>`. This saves the token and your
   chat id to `secrets.json` and sends a test message.

Every chat in `telegram.chat_ids` gets the alerts and can use `/health`. To add another person, have
them press Start on the bot. The log then shows `telegram: unlisted chat <id>`, and you add that id to
`chat_ids` in `secrets.json`. The change is picked up without a restart.

## Configuration

`config.json` is safe to commit. Secrets go in `secrets.json`, which is gitignored and overlaid on top
of `config.json`:

```json
{
  "auth_token": "long random string",
  "telegram": { "bot_token": "123:abc", "chat_ids": [111111111, 222222222] }
}
```

Top-level settings:

- `host` and `port` (default `0.0.0.0:8478`).
- `user_agent`.
- `notify_file`, a local log of alerts.
- `ntfy_topic`.
- `filter`, the default include/exclude patterns.
- `chrome`, the browser path for render watchers.

Changing `host`, `port` or `auth_token` needs a restart.

## Run it permanently (systemd user service)

```sh
# edit the paths in sitewatch.service if the checkout isn't at ~/sitewatch
ln -s "$PWD/sitewatch.service" ~/.config/systemd/user/
systemctl --user enable --now sitewatch
loginctl enable-linger "$USER"   # keep running after logout
journalctl --user -u sitewatch -f
```

## Other commands

- `./sitewatch rebaseline` (service stopped) marks everything currently listed as seen, without alerts.
- `go test ./...` runs the tests.

## API

All endpoints need `Authorization: Bearer <auth_token>`.

- `GET /api/status` returns every watcher's last check, status and last alert.
- `GET /api/events?limit=50` returns recent alerts, newest first.
- `POST /api/check/{name}` checks a watcher now.

## License

[PolyForm Noncommercial 1.0.0](LICENSE.md). You're free to use, modify and share it for personal,
hobby, research, educational or other non-commercial purposes. Commercial use needs separate
permission from the author.
