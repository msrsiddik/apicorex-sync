# apicorex-sync

An offline-first data sync plugin for [ApiCoreX](https://github.com/msrsiddik/apicorex).
Any offline-capable app (mobile, desktop, web) can push local changes and pull
server changes through this plugin — no custom sync logic needed per app.

## How it works

- **Push**: clients send a batch of changes (create / update / delete). Each
  change carries a stable `record_id`, a client clock `updated_at`, and a
  unique `change_id` for idempotency.
- **Pull**: clients fetch all changes newer than their last cursor
  (`version > since`). Responses are ordered by a server-side monotonic
  sequence (clock-skew safe).
- **Conflict resolution**: last-write-wins by `updated_at`. An incoming change
  that is older than the stored version is returned as `stale` — no data is
  lost, the client simply ignores it.
- **Tombstones**: deletes are soft (`deleted: true`) so every device learns
  about them. A periodic GC sweep removes tombstones older than the retention
  window (default 90 days).

## Multi-tenancy & scoping

- ApiCoreX Core injects `X-ApiCoreX-Schema` (e.g. `tenant_acme`) and
  `X-ApiCoreX-User-ID` into every proxied request.
- Every DB query runs inside a transaction with `SET LOCAL search_path TO
  "<schema>"` — no cross-tenant data leakage, no pool contamination.
- Records are **per-user** by default (`user_id = caller`).
- Collections listed in `SYNC_SHARED_COLLECTIONS` are **tenant-wide**
  (`user_id = ''`): written and read by all users in the tenant (useful for
  shared catalogs, config, etc.).

## Plugin contract

This plugin follows the ApiCoreX plugin contract: it serves a manifest at
`GET /_apicorex/manifest` (which declares its migrations), registers itself
with Core via `POST /_core/register`, and sends periodic heartbeats. On
restart, Core will re-register this plugin; on network failure, it retries with
exponential backoff (1 s → 30 s cap, no give-up).

## Configuration

Copy `.env.example` to `.env` and fill in your values:

| Variable | Required | Default | Description |
|---|---|---|---|
| `DATABASE_URL` | yes | — | Postgres DSN |
| `PLUGIN_API_KEY` | yes | — | Shared secret with Core |
| `CORE_URL` | no | `http://localhost:8080` | ApiCoreX Core base URL |
| `PLUGIN_ADDR` | no | `:50052` | Address this plugin listens on |
| `PLUGIN_BASE_URL` | no | auto | Externally-reachable URL of this plugin |
| `SYNC_SHARED_COLLECTIONS` | no | *(none)* | Comma-separated tenant-wide collections |
| `SYNC_TOMBSTONE_RETENTION` | no | `2160h` (90d) | Go duration for tombstone GC window |

## Running

```sh
cp .env.example .env   # fill DATABASE_URL, PLUGIN_API_KEY, CORE_URL
go run ./cmd/sync
```

Or build a binary:

```sh
go build -o sync-plugin ./cmd/sync
./sync-plugin
```

### With Docker

This repo ships a `Dockerfile` and a standalone `docker-compose.yml` (plugin +
its Postgres). It needs a running Core to register with and the Identity plugin
to have created the tenant schemas. Point `CORE_URL` at your Core.

```bash
docker compose up --build
```

For an all-in-one stack (Core + Postgres + Redis + every plugin) use the
compose file in the [Core repo](../apicorex) instead.

## API

Both endpoints require a valid JWT (issued by Identity, verified by Core).
Core strips the token and injects `X-ApiCoreX-*` headers before forwarding.

### POST /sync/push

Push a batch of local changes.

```json
{
  "changes": [
    {
      "change_id":  "uuid-v4",
      "collection": "notes",
      "record_id":  "uuid-v4",
      "deleted":    false,
      "payload":    { "title": "hello" },
      "updated_at": "2026-06-28T10:00:00Z"
    }
  ]
}
```

Response:

```json
{
  "results": [
    {
      "change_id":        "uuid-v4",
      "record_id":        "uuid-v4",
      "status":           "applied",
      "version":          4271,
      "server_updated_at":"2026-06-28T10:00:01Z"
    }
  ],
  "cursor": 4271
}
```

`status` values: `applied` | `duplicate` | `stale` | `rejected`.

### GET /sync/pull

Pull changes newer than a cursor.

```
GET /sync/pull?since=4270&collection=notes&limit=100
```

| Param | Default | Description |
|---|---|---|
| `since` | `0` | Pull all changes with `version > since`. `0` = full sync. |
| `collection` | *(all)* | Filter to one collection. |
| `limit` | `500` | Page size (max 1000). |

Response:

```json
{
  "changes":  [ { "collection":"notes", "record_id":"...", "deleted":false,
                  "payload":{...}, "version":4271, "server_updated_at":"..." } ],
  "cursor":   4271,
  "has_more": false
}
```

Tombstones (`deleted: true`) are included so devices can remove the record locally.

## Data model

One `sync_records` table per tenant schema (created by the plugin's migration):

| Column | Type | Notes |
|---|---|---|
| `collection` | text | Logical group (e.g. `notes`, `tasks`) |
| `record_id` | text | Client-stable UUID |
| `user_id` | text | Owner; `''` for tenant-wide records |
| `payload` | jsonb | Arbitrary client data |
| `deleted` | boolean | Tombstone flag |
| `client_updated_at` | timestamptz | LWW compare key |
| `server_updated_at` | timestamptz | Set by server on every write |
| `version` | bigint | Monotonic cursor (from schema-local sequence) |
| `last_change_id` | text | Idempotency key (last winning `change_id`) |

Primary key: `(collection, record_id, user_id)`.

Your app's model does not need any extra fields — `record_id`, `updated_at`, and
`change_id` are the only requirements for a sync-able record.

## Testing

See [TESTING.md](TESTING.md) for integration-test setup (local Docker or remote
Podman) and E2E curl examples.

```sh
go test ./...
```
