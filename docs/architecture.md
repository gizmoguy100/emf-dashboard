# Architecture Notes

## Services

- `app`: Go HTTP service, currently server-rendering the dashboard from favourites data.
- `postgres`: persistent state store.
- `caddy`: intentionally omitted for now; add later as a production deployment concern.

## App Responsibilities

The Go service currently serves the dashboard. It can eventually contain:

- HTTP dashboard pages or JSON endpoints.
- Event API sync jobs.
- Bar API status summaries.
- Reminder scheduling.
- Telegram bot command handling.
- State reconciliation between external APIs and local preferences.

Keep these as separate internal packages once code exists, for example:

- `internal/config`
- `internal/http`
- `internal/events`
- `internal/reminders`
- `internal/telegram`
- `internal/store`

## Database

Use PostgreSQL for:

- canonical user/dashboard preferences
- talks and schedule cache
- reminder state
- Telegram chat/user allow-list state
- sync checkpoints and API response metadata

Store normalized records for things the app queries often. Use `jsonb` for source API payloads, rarely queried metadata, and forward-compatible event fields.

## Secret Handling

Secrets enter the app through the ignored local YAML config or through a deployment secret store that writes the same config shape.

Examples:

- `telegram.bot_token`
- `external_apis.emf.token`
- `external_apis.emf.favourites_url`
- `database.url`

## Portability

The same image should run locally and in production. `APP_CONFIG_FILE` and mounted config decide behavior.

Use Compose profiles later if separate local-only tooling is added, such as database admin UI, fake API services, or development mail/notification sinks.

## Bar API Polling

The dashboard polls the small bar summary endpoints through an in-process cache. The default interval is 10 minutes, configured as `external_apis.bar.poll_interval`. This avoids WebSocket complexity and avoids hammering expensive stock endpoints.
