# Architecture

protecht is a single-process HTTP service plus device clients. There is no separate worker, queue, or user account system.

```
cmd/device  --HTTP-->  cmd/server
cmd/agent   --HTTP-->      |
                        internal/api
                         /        \
                 internal/core   internal/store
                                      |
                                   SQLite
```

## Components

`cmd/server` opens the SQLite file (`-db`, default `protecht.db`) and listens (`-addr`, default `127.0.0.1:8080`). It does nothing else.

`internal/api` is the HTTP front end: JSON and multipart handlers, bearer-token checks on writes, and the HTML timeline. It maps validation and association failures to status codes. The clock is injectable so tests can control "now".

`internal/core` is the platform-independent domain: device, evidence, and event types, validation limits, UUID helpers, and token hashing. It does not know about HTTP or storage. Protocol limits live here; the README states them as the contract.

`internal/store` persists those types in SQLite. `Open` creates the schema. Evidence bytes are stored as blobs. Event idempotency is the primary key `(device_id, id)`: a replay returns the original row and does not overwrite it. The pool is one connection, so store methods must finish one query before starting the next.

`cmd/device` is not part of the server. It registers, uploads evidence, submits an event, replays it, and prints the timeline, then exits. It decodes the timeline JSON into `store.EventRow`, so the simulator currently depends on a store view type.

`cmd/agent` is not part of the server either. It is a Windows foreground-window client: it polls the active window, registers once, and submits an event per change. The registration token is stored in a local file and reused. The agent speaks only the HTTP contract and does not import `internal/store`.

## Boundaries

Writes are authenticated by a per-device bearer token. The server stores only the token's SHA-256 hash and takes device identity from that token, never from the request body. Read endpoints (timeline and evidence bytes) are unauthenticated. That, plus the localhost bind, is the whole security model. Do not expose this process beyond localhost.

Device-claimed times (`occurred_at`, `captured_at`) and server receipt (`received_at`) are both kept. The timeline orders by the device-claimed time.

`kind` and event `type` are length-capped free-form strings, not enums.
