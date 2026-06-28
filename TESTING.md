# Running Integration Tests

Tests use [testcontainers-go](https://testcontainers.com/guides/getting-started-with-testcontainers-for-go/)
to spin up a real Postgres 16 container for each test run.

## Local Docker / Podman (same machine)

```sh
# Docker Desktop or local podman machine running — just run:
go test ./...
```

## Remote Podman (container engine on another machine)

If your container engine lives on a different host (e.g. a Linux VM or headless
server), forward the Podman socket over SSH and set two env vars before running
tests.

### 1 — Forward the socket

```sh
ssh -fN -L /tmp/podman-remote.sock:/run/user/1000/podman/podman.sock siddik@100.119.254.20
```

Keep this running (the `-fN` flags background it and open no shell). Re-run after
a reboot or SSH disconnect.

### 2 — Export env vars

```sh
export DOCKER_HOST=unix:///tmp/podman-remote.sock
export TESTCONTAINERS_RYUK_DISABLED=true   # Ryuk reaper doesn't work over remote socket
export TEST_DB_HOST=100.119.254.20          # mapped port lives on the remote host
```

### 3 — Run tests

```sh
go test -v -timeout 120s ./internal/syncdb/...
```

Expected output (all 8 tests pass):

```
--- PASS: TestRoundTrip
--- PASS: TestLastWriteWins
--- PASS: TestDeletePropagation
--- PASS: TestIdempotentRetry
--- PASS: TestCursorPagination
--- PASS: TestPerUserIsolation
--- PASS: TestTenantIsolation
--- PASS: TestTenantWideCollection
```

### Notes

- `TEST_DB_HOST` replaces `localhost`/`127.0.0.1` in the connection string so the
  test process can reach the container port on the remote machine.
- Tests skip automatically if no container engine is reachable (e.g. in CI without
  Docker), so they never block a plain `go build`.

## E2E via ApiCoreX Core (curl)

Requirement: Core + Identity + Sync all running. See each project's `.env.example`.

```sh
# 1. Register a tenant + owner. `slug` must be a valid identifier
#    (3-32 chars, lowercase letter first, then letters/digits/underscore).
curl -s -X POST http://localhost:8080/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"slug":"acme","name":"Acme Corp","email":"owner@acme.com","password":"secret123","full_name":"Ada Owner"}'

# 2. Login → grab the access token
TOKEN=$(curl -s -X POST http://localhost:8080/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"slug":"acme","email":"owner@acme.com","password":"secret123"}' \
  | jq -r '.access_token')

# 3. Find your tenant_id (decoded from the JWT, or from the register response)
TENANT_ID=$(echo "$TOKEN" | cut -d. -f2 | base64 -d 2>/dev/null | jq -r '.tenant_id')

# 4. Install the sync plugin for YOUR tenant (tenant_id must match the token's)
curl -s -X POST http://localhost:8080/plugins/install \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"tenant_id\":\"$TENANT_ID\",\"plugin_name\":\"sync\"}"

# 5. Push a change
curl -s -X POST http://localhost:8080/sync/push \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "changes": [{
      "change_id": "c1", "collection": "notes", "record_id": "r1",
      "payload": {"title":"hello"}, "updated_at": "2026-06-28T10:00:00Z"
    }]
  }'

# 6. Pull back
curl -s "http://localhost:8080/sync/pull?since=0" \
  -H "Authorization: Bearer $TOKEN"
```

The pull response should include the pushed record with `version > 0` and
`has_more: false`.

## E2E via Scalar UI (browser)

Core serves interactive API docs at <http://localhost:8080/docs>. The sync
routes appear there only after the sync plugin has registered with Core (so
start the plugins, then refresh the page).

1. **Register + login** — in the `auth` section run `POST /auth/register`
   (valid `slug`), then `POST /auth/login`. Copy the `access_token` from the
   login response.
2. **Authorize** — click the **Authorize** button (lock icon), pick
   `bearerAuth`, and paste the `access_token`. Every request now carries
   `Authorization: Bearer <token>`.
3. **Install sync** — run `POST /plugins/install` with
   `{"tenant_id":"<your t_...>","plugin_name":"sync"}`. Get `tenant_id` from
   the JWT (or `GET /me`). It must match your token's tenant.
4. **Push** — run `POST /sync/push` with a `changes` array.
5. **Pull** — run `GET /sync/pull?since=0` and confirm your record comes back.

> Tip: access tokens expire in 15 minutes. If requests start returning 401,
> re-run `POST /auth/login` and re-authorize with the fresh token.
