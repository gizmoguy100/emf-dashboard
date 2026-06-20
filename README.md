# EMF Dashboard

This is Alex's little EMF 2026 dashboard thing.

It was, with very little shame, vibe coded with Codex in a burst of "what if my festival dashboard looked like a 90s space terminal?" energy. It is a work in progress. It may become genuinely useful. It may also become a highly themed way to discover that the bar is closed.

Current features:

- server-rendered Go web app
- Docker Compose setup
- EMF favourites schedule display
- EMF bar status using the test/live bar API
- 90s space dashboard styling
- no client-side JavaScript (for now)

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

Open:

```text
http://localhost:8080
```

Stop it:

```sh
docker compose down
```

## Configure It

`.env` is just the tiny Docker Compose bootstrap file. Keep it boring:

```sh
APP_CONFIG_FILE=/app/config/config.yaml
APP_HTTP_PORT=8080
```

The real app config lives in:

```text
config/config.yaml
```

That file is ignored by Git because it can contain private URLs and tokens.

Useful values:

```yaml
owner:
  display_name: "Alex"

external_apis:
  emf:
    favourites_url: "https://www.emfcamp.org/favourites.json?token=..."
  bar:
    enabled: true
    base_url: "https://emftill.assorted.org.uk"
    poll_interval: "10m"
```

For the bar API:

- before EMF 2026, use `https://emftill.assorted.org.uk`
- when live event data is ready, switch to `https://bar.emf.camp`

## Public Repo Rule

Do not commit secrets.

Commit:

- `.env.example`
- `config/config.example.yaml`

Do not commit:

- `.env`
- `config/config.yaml`
- real favourites URLs
- Telegram tokens
- production hostnames/passwords

## HTTPS With Caddy

The Compose stack includes Caddy in front of the Go app. Caddy terminates TLS
for the configured hostname and wildcard hostname, then proxies to `app:8080`
on the private Compose network.

For production, set this in ignored `config/config.yaml`:

```yaml
domain:
  hostname: "emf.example.com"
  dns_provider: "cloudflare"
  cloudflare:
    api_token: "..."
```

The Cloudflare token needs `Zone:DNS:Edit` and `Zone:Zone:Read` on the relevant
zone so Caddy can complete DNS-01 certificate challenges.

## Notes

PostgreSQL is in Compose because the app will probably want real state later: reminder state, cached API payloads, Telegram bot state, preferences, and sync checkpoints. It is not doing much yet.

Caddy is not wired in yet. Production can put Caddy, Traefik, nginx, or some other ingress in front of the Go app on port `8080`.
