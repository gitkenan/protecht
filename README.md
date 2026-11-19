# protecht — vertical slice

A cross-platform accountability prototype: devices register, upload hashed
evidence, and submit events that reference that evidence. The server persists
both and renders an activity timeline (HTML + JSON).

The device/evidence protocol is plain JSON + multipart over HTTP and contains
no platform-specific concepts; the included `cmd/device` is a simulated
device that exercises the whole flow.

## Layout

```
cmd/server        HTTP API + HTML timeline (binds 127.0.0.1 by default)
cmd/device        simulated device client (runs the full flow, then exits)
cmd/agent         Windows foreground-window agent (long-running client)
internal/core     platform-independent domain types, validation, id/token helpers
internal/store    SQLite persistence
internal/api      HTTP handlers, auth, HTML template
```

Requires Go 1.23+. Only third-party dependency: `github.com/mattn/go-sqlite3`.

## Run

```sh
go build ./cmd/server ./cmd/device
./server -addr 127.0.0.1:8080 -db protecht.db &
./device -server http://127.0.0.1:8080 -name sim-device
# then open http://127.0.0.1:8080/ for the timeline
```

## Windows activity agent

`cmd/agent` polls the foreground window and, when it changes, uploads a JSON
observation as evidence and submits an `application.foreground` event that
references it. The first poll is reported immediately; later polls default to
every 3 seconds. An unchanged window is not uploaded again.

The agent is Windows-only. It reads the foreground window's title, the
process image name, and the executable path. The path (and therefore the
process name) is left empty when the process cannot be opened, which is
normal for some protected processes. The process reported is the one that
owns the foreground window; for some Store apps that is
`ApplicationFrameHost.exe` rather than the hosted app.

Build it from any platform:

```sh
GOOS=windows GOARCH=amd64 go build -o agent.exe ./cmd/agent
```

On Windows, with the server already running:

```sh
go build -o agent.exe ./cmd/agent
agent.exe -server http://127.0.0.1:8080 -name my-pc -interval 3s -config agent.json
```

| flag | default | meaning |
|------|---------|---------|
| `-server` | `http://127.0.0.1:8080` | server base URL |
| `-name` | machine hostname, or `win-agent` | device name, used only on first registration |
| `-interval` | `3s` | foreground poll interval |
| `-config` | `agent.json` | file holding the device id and bearer token |

The token is returned only at registration. The agent saves it in the config
file (mode `0600`) and reuses it on later runs; `-name` is then ignored.
Delete the file to register a different device. Do not share it — it
authenticates writes. A file that exists but cannot be parsed is left as-is
rather than replaced with a new registration.

Observations are printed to stdout when the foreground window changes. Upload
failures are logged to stderr and retried on the next poll with the same
event id, so a lost response does not create a duplicate event. Stop the
agent with Ctrl+C.

## Test

```sh
go test ./...
```

Covers event validation, evidence association, duplicate event submissions,
timestamp handling, token hashing, persistence across restarts, and the full
HTTP flow end to end.

## API contract

Base URL: `http://127.0.0.1:8080`. All timestamps are RFC 3339. All stored
timestamps are UTC. Error responses are `{"error": "<message>"}` with a
suitable status code (400 malformed, 401 unauthorized, 404 not found,
422 well-formed but invalid).

### POST /api/v1/devices — register

No auth. Body: `{"name": "sim-device"}` (1–100 chars).

→ `201`:
```json
{"id":"<uuid>","name":"sim-device","token":"<64 hex chars>","created_at":"..."}
```
**The token is returned only here.** Store it; it authenticates all writes.

### POST /api/v1/evidence — upload evidence

Auth: `Authorization: Bearer <token>`. `multipart/form-data` with fields:

| field         | meaning                                             |
|---------------|-----------------------------------------------------|
| `kind`        | free-form label, 1–50 chars (e.g. `screenshot`)     |
| `sha256`      | claimed SHA-256 of the content, lowercase hex       |
| `captured_at` | device-claimed capture time (RFC 3339)              |
| `file`        | the content bytes (1 byte – 10 MiB)                 |

The server computes the content's SHA-256 and rejects mismatches with `400`.
`captured_at` more than 5 min ahead of the server clock → `422`.

→ `201`:
```json
{"id":"<uuid>","kind":"screenshot","sha256":"...","byte_size":131072,
 "content_type":"application/octet-stream","captured_at":"...","received_at":"..."}
```
`content_type` is sniffed server-side (`http.DetectContentType`).

### GET /api/v1/evidence/{id} — retrieve evidence

No auth (read endpoint; see assumptions). → `200` with the raw content bytes,
`Content-Type` as sniffed at upload, and an `X-SHA256` header carrying the
integrity hash. Unknown id → `404`.

### POST /api/v1/events — submit an event

Auth: bearer token. Body:
```json
{"id":"<client-generated uuid>","type":"check_in",
 "occurred_at":"...","evidence_ids":["<uuid>", ...]}
```

Rules:

- `id` is chosen by the client and is the **idempotency key**, unique per
  device. First submission → `201` with the stored event. Resubmission of the
  same id → `200` with the **original** record; nothing is overwritten, even
  if the replayed payload differs.
- `type`: free-form, 1–50 chars. `occurred_at` is device-claimed and must not
  be more than 5 min in the future (`422`). `evidence_ids`: ≤ 20 distinct
  UUIDs.
- Every referenced evidence must exist and belong to the **same device**,
  else `422`. The device identity comes from the token, never from the body.

### GET /api/v1/events — timeline JSON

No auth. Optional `?device_id=<uuid>` filter.

→ `200`:
```json
{"events":[{"id":"...","device_id":"...","device_name":"sim-device",
 "type":"check_in","occurred_at":"...","received_at":"...",
 "evidence":[{"id":"...","kind":"screenshot"}]}]}
```
Ordered by `occurred_at`, ties broken by `received_at`, then `id`.

### GET / — HTML timeline

No auth. Renders the same timeline as a table; each evidence item links to
its retrieval URL.

## Assumptions & decisions

- **Local prototype safety**: the server binds to `127.0.0.1` and that —
  plus per-device tokens on writes — is the entire security model. Read
  endpoints (timeline, evidence content) are unauthenticated on purpose.
  Do not expose this slice to a network.
- **Tokens at rest**: only the SHA-256 hash of a device token is stored; the
  plaintext token exists in the registration response and (transiently) in
  the server's auth check.
- **Idempotency is per device**: the key is `(device_id, event id)`. Replays
  are last-write-never — the original record wins.
- **Two clocks, both kept**: devices claim `occurred_at`/`captured_at`; the
  server records `received_at`. Device timestamps are bounded to at most
  5 min of positive clock skew; old timestamps are accepted. The timeline
  orders by the device-claimed time (that's when it happened), tie-broken by
  server receipt.
- **Evidence bytes live in SQLite** as blobs, with the SHA-256 as the
  integrity anchor. Verified on upload, echoed on retrieval.
- **`kind`/`type` are free-form strings** (length-capped) rather than enums,
  to keep the protocol platform-independent. Conventions emerge from clients.
- **Limits** (all in `internal/core/validate.go`): 10 MiB evidence, 20
  evidence refs/event, 100-char device names, 50-char kinds/types.

## Explicitly out of scope (per slice definition)

AI/ML analysis, platform-specific capture, VPN monitoring, user accounts or
any auth beyond the above, multi-tenancy, retention/deletion, evidence
download streaming for large files.
