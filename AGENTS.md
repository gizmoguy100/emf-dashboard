# Agent Notes

This file is for Codex or other AI coding agents working on this repo later.

## Project Shape

- Go web app in `cmd/server/main.go`.
- Server-rendered HTML template in `web/templates/dashboard.html`.
- CSS in `web/static/dashboard.css`.
- Docker Compose stack in `docker-compose.yml`.
- Runtime config template in `config/config.example.yaml`.
- Local runtime config in `config/config.yaml`, ignored by Git.
- `.env` is ignored and should only contain Compose bootstrap values.

## Hard Rules

- Do not commit secrets.
- Do not print or copy secret values from `.env` or `config/config.yaml` into responses, logs, docs, or tracked files.
- Keep `config/config.example.yaml` safe and placeholder-only.
- Keep `.env.example` safe and placeholder-only.
- Keep the app usable without private config by falling back to sample or degraded data.
- Do not add client-side JavaScript unless there is a strong reason. Current design goal is server-rendered HTML plus CSS.

## Current Runtime Config Model

Compose passes only:

```sh
APP_CONFIG_FILE=/app/config/config.yaml
```

The app reads YAML from that path.

Important YAML fields:

```yaml
app:
  http_addr: ":8080"
  public_base_url: "http://localhost:8080"
  http_timeout: "12s"

owner:
  display_name: "Your Name"

event:
  timezone: "Europe/London"

external_apis:
  emf:
    favourites_url: ""
    request_timeout: "10s"
  bar:
    enabled: true
    base_url: "https://emftill.assorted.org.uk"
    poll_interval: "10m"
```

## External APIs

EMF favourites:

- Configured by `external_apis.emf.favourites_url`.
- The URL contains a token, so it belongs only in ignored config.
- The app fetches favourites at request time today.

EMF bar:

- Configured by `external_apis.bar.base_url`.
- Pre-event test URL: `https://emftill.assorted.org.uk`.
- Live event URL should be `https://bar.emf.camp`.
- Bar data is cached in-process for `external_apis.bar.poll_interval`, currently 10 minutes.
- Avoid polling expensive endpoints frequently.
- Current bar calls:
  - `/api/sessions.json`
  - `/api/progress.json`
  - `/api/on-tap.json`
  - `/api/cybar-on-tap.json`
  - `/api/department/75.json` for Club Mate

## Design Direction

The visual style is "90s space terminal", not generic SaaS.

Keep:

- responsive mobile and desktop layout
- dense dashboard-first UI
- neon terminal/control-panel look
- no landing page
- no decorative cards inside cards
- no client-side JS by default

Avoid:

- marketing hero sections
- generic gradient SaaS polish
- heavy JavaScript frameworks
- external font/asset dependencies unless deliberately introduced

## Verification

After Go or template changes, run:

```sh
go test ./...
docker compose build app
docker compose up -d --force-recreate app
curl -fsS http://localhost:8080/healthz
```

Useful render check:

```sh
curl -fsS http://localhost:8080/ | rg "Orbit Desk|Bar Status|Schedule Radar"
```

Secret scan before finishing:

```sh
rg -n "token=[A-Za-z0-9_-]{12,}" . -g '!.env' -g '!config/config.yaml'
rg -n "TELEGRAM_[A-Z_]+=.+" . -g '!.env' -g '!config/config.yaml'
rg -n "bot_token: \".+\"" . -g '!.env' -g '!config/config.yaml'
```

The command should return no tracked secret values.

## Implementation Preferences

- Keep code standard-library-first unless a dependency clearly earns its place.
- YAML parsing currently uses `gopkg.in/yaml.v3`.
- Keep new behavior small and explicit.
- Prefer server-side formatting of display labels.
- Add caching before adding background workers unless persistence is required.
- PostgreSQL is present for future state but should not be forced into features before it is useful.
