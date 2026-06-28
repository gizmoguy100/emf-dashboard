# EMF Dashboard

Alex's little EMF 2026 personal dashboard thing.

It was, with very little shame, vibe coded with Codex in a burst of "what if my festival dashboard looked like a 90s space terminal?" energy. It is a work in progress. It may become genuinely useful. It may also become a highly themed way to discover that the bar is closed.

## Current Features

- Server-rendered Go web app with a 90s space dashboard style.
- Docker Compose stack with the app, PostgreSQL, and Caddy.
- Public contact page plus dashboard host routing.
- Caddy HTTPS with Cloudflare DNS-01 certificate support.
- EMF favourites schedule display.
- EMF bar status from the test/live bar API.
- Open-Meteo current weather for Eastnor/Ledbury.
- Home Assistant phone vitals, including configurable metrics and daily-delta values.
- Telegram-backed MiniBlog for short text updates.
- PostgreSQL-backed API caching and bot state.
- Tiny client-side refresh script for 5-minute dashboard reloads.

## Build And Run

First create local config files:

```sh
cp .env.example .env
cp config/config.example.yaml config/config.yaml
```

Then run it:

```sh
docker compose up --build
```

Local URLs:

```text
http://localhost:8080/
http://localhost:8080/dashboard
```

Stop it:

```sh
docker compose down
```

## Configure It

`.env` is only the Docker Compose bootstrap file. Keep it boring:

```sh
APP_CONFIG_FILE=/app/config/config.yaml
APP_HTTP_PORT=8080
```

The real app config lives in:

```text
config/config.yaml
```

That file is ignored by Git because it can contain private URLs, bot tokens, Home Assistant tokens, database passwords, and DNS provider credentials.

Start from `config/config.example.yaml`. Useful sections:

```yaml
owner:
  display_name: "Your Name"

database:
  url: "postgres://emf_dashboard:change-me-local-only@postgres:5432/emf_dashboard?sslmode=disable"

external_apis:
  emf:
    favourites_url: "https://www.emfcamp.org/favourites.json?token=..."
    cache_interval: "30m"
  bar:
    enabled: true
    base_url: "https://emftill.assorted.org.uk"
    poll_interval: "30m"
  weather:
    enabled: true
    base_url: "https://api.open-meteo.com"
    location: "Eastnor Deer Park"
    latitude: 52.0367
    longitude: -2.3918
    cache_interval: "30m"
  home_assistant:
    enabled: true
    base_url: "https://home-assistant.example.com"
    access_token: "..."
    cache_interval: "30m"

telegram:
  enabled: true
  bot_token: "..."
  allowed_chat_ids:
    - 123456789
  miniblog:
    max_length: 140
    dashboard_limit: 8
```

For the bar API:

- Before EMF 2026, use `https://emftill.assorted.org.uk`.
- When live event data is ready, switch to `https://bar.emf.camp`.

Dashboard API data is cached in PostgreSQL. Favourites, bar data, weather, and Home Assistant vitals are refreshed during page renders only when their cached value is stale. Failed refreshes return stale or degraded data quickly, then make a short background retry attempt before backing off.

## Telegram MiniBlog

The MiniBlog is a small text-only posting flow driven by a Telegram bot.

1. Create a bot with BotFather and put its token in ignored `config/config.yaml`.
2. Send the bot a message.
3. Use Telegram `getUpdates` or the app logs/tools to find your chat/user id.
4. Add that id to `telegram.allowed_chat_ids`.
5. Restart the app.

Only allowed chat ids can post. Plain text messages become MiniBlog posts if they are within `telegram.miniblog.max_length`. The bot also supports:

- `/start`
- `/help`
- `/post`
- `/latest`
- `/delete_latest`

The bot uses Telegram long polling, so no webhook route is required.

## HTTPS With Caddy

The Compose stack includes Caddy in front of the Go app. Caddy terminates TLS for the configured hostname and wildcard hostname, then proxies to `app:8080` on the private Compose network.

For production, set this in ignored `config/config.yaml`:

```yaml
domain:
  hostname: "emf.example.com"
  dashboard_hostname: "dashboard.emf.example.com"
  dns_provider: "cloudflare"
  cloudflare:
    api_token: "..."
```

The Cloudflare token needs `Zone:DNS:Edit` and `Zone:Zone:Read` on the relevant zone so Caddy can complete DNS-01 certificate challenges.

The contact page is served from `domain.hostname`. The dashboard is served from `domain.dashboard_hostname`, with `/dashboard` retained as a local fallback.

## Public Repo Rule

Do not commit secrets.

Commit:

- `.env.example`
- `config/config.example.yaml`

Do not commit:

- `.env`
- `config/config.yaml`
- real favourites URLs
- Telegram bot tokens
- Home Assistant access tokens
- Cloudflare API tokens
- production hostnames/passwords unless they are intentionally public placeholders

## Verification

After Go, template, or CSS changes:

```sh
go test ./...
docker compose build app
docker compose up -d --force-recreate app
curl -fsS http://localhost:8080/healthz
```

Useful render check:

```sh
curl -fsS -H 'Host: dashboard.emf.example.com' http://localhost:8080/ | grep -E "Orbit Desk|MiniBlog|Bar Status|Schedule Radar"
```

## Notes

PostgreSQL stores API cache rows, Home Assistant/weather/bar snapshots, Telegram MiniBlog posts, and bot polling state. The app still degrades to sample or missing-state UI where private config is absent, so the repo remains clonable without Alex's credentials.
