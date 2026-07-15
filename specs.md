# Spec: local web inspector for the tunnel client

Goal: add a local web UI to the tunnel client (ngrok's `localhost:4040` style) that shows the
requests going through the tunnel live, with request/response header and body detail, and the
ability to *replay* against the local server.

This document is self-contained: it includes all the context of the existing project so you can
work without further references. Runtime: **Deno + TypeScript**, **zero-build**, and the result
must keep compiling to a single binary with `deno compile`.

> Note: this spec targets a Deno/TypeScript client. This repository implements the client in **Go**
> instead (single binary via `go build`, inspector UI embedded with `//go:embed`); the design and
> the relay contract below still apply.

---

## 1. Context of the existing project

It is a single-user ngrok-style HTTP tunnel, but built on a PaaS (onvibe) that **does not allow
persistent connections** (no server-side WebSockets, ~20s time-to-first-byte cap). That is why the
transport is a **relay over Postgres with double long-polling**, not a persistent connection.

Pieces:

- **Relay** (app deployed at `https://tunnel.onvibe.run`): the public server. When a public request
  comes in, it queues it in Postgres and waits (≤17s) for the response. It exposes a control channel
  authenticated by a shared secret.
- **Client** (`client.ts`, Deno, runs on the user's machine): N workers long-poll the relay, claim
  requests, forward them to `http://localhost:PORT`, and return the response to the relay. **This is
  the file you are going to modify.**

The inspector is **100% local inside the client process**. It does not touch the database or the
relay: it captures from inside the client's `handle()`, which already holds both the request and the
response. **Do not modify the onvibe app or the SQL schema.**

### 1.1. Relay contract (reference, do not change it)

Auth: `x-tunnel-secret: <TUNNEL_SECRET>` header on every control call. Bodies travel in **base64**
in both directions (or `null` if there is no body).

| Endpoint | Method | Input | Output |
|---|---|---|---|
| `/__tunnel/poll` | POST | — | `200 {request:{id,method,path,headers,body}}` or `204` (no work, held ≤15s) |
| `/__tunnel/respond` | POST | `{id,status,headers,body}` | `{ok:true}` |
| `/__tunnel/status` | GET | — | `{online,last_poll_at,pending}` |
| any other route | * | public traffic | queued; relay waits ≤17s → 504; 502 if no client connected |

`path` includes the query string. `headers` is a plain `Record<string,string>` object.

### 1.2. Current structure of `client.ts` (where you hook capture in)

Config via flags or env vars:

- `--url` / `TUNNEL_URL` → relay base (default `https://tunnel.onvibe.run`)
- `--secret` / `TUNNEL_SECRET` → shared secret (required)
- `--local` / `LOCAL` → local target, accepts `3000` | `localhost:3000` | `http://127.0.0.1:8080`
- `--concurrency` / `CONCURRENCY` → number of workers (default 8)

Helpers already present: `b64encode(Uint8Array)`, `b64decode(string)`, `sleep(ms)`, and two sets of
headers to drop: `DROP_REQ` (host, connection, content-length, transfer-encoding, keep-alive,
proxy-connection, upgrade) and `DROP_RES` (content-length, transfer-encoding, content-encoding,
connection, keep-alive).

Relevant flow (the capture hook point):

```
worker() {                          // there are CONCURRENCY workers in Promise.all
  loop:
    POST /__tunnel/poll
    204 → re-poll; 401 → error; !ok → backoff
    200 → handle(data.request)
}

handle({id, method, path, headers, body}) {
  build Headers dropping DROP_REQ
  init.body = b64decode(body) if there is a body
  res = fetch(LOCAL + path, {method, headers, body, redirect:"manual"})
    catch → respond(id, 502, ..., "Local server unreachable")   // <- capture as error
  outHeaders = res.headers minus DROP_RES
  buf = new Uint8Array(await res.arrayBuffer())
  respond(id, res.status, outHeaders, buf.length ? b64encode(buf) : null)
}

respond(id, status, headers, body) → POST /__tunnel/respond
```

In `handle()` you already have: the request bytes (`init.body`), request headers, status, response
headers and bytes (`buf`). **Capturing is just saving copies.** The current boot ends in
`await Promise.all(workers)`.

---

## 2. What to build

A local web inspector, served by the client itself with `Deno.serve`, listening on
**`127.0.0.1:4040`** (loopback, never `0.0.0.0`). ngrok style:

- Live list of requests (newest on top), auto-refreshing.
- Per-request detail: request and response headers and body, with JSON pretty-printing.
- Replay: re-sends a captured request to the local server (without going through the tunnel) and
  records it as a new entry.
- Filter by method/path/status. Clear button. Connection indicator (relay + local).

### 2.1. Captured data model

```ts
type Phase = "pending" | "done" | "error";

interface CapturedRequest {
  id: string;                          // tunnel request id (or new uuid if replay)
  at: number;                          // epoch ms of reception
  method: string;
  path: string;                        // includes query
  reqHeaders: Record<string, string>;
  reqBody: string | null;              // base64
  reqBytes: number;
  phase: Phase;
  status?: number;
  resHeaders?: Record<string, string>;
  resBody?: string | null;             // base64, possibly truncated (see §2.4)
  resBytes?: number;                   // real size, before truncation
  durationMs?: number;
  error?: string;                      // present if phase === "error"
  replayOf?: string | null;            // original id if it is a replay
}
```

The **summary** (for list and SSE, without bodies to stay light) is the same object without
`reqHeaders/reqBody/resHeaders/resBody`.

### 2.2. In-memory store

Ring buffer of the last `MAX_LOG` (default 500) entries + a `Map<id, CapturedRequest>` for detail
and updates. When the limit is exceeded, drop the oldest (from the Map too). Since Deno is
single-threaded and everything runs on the same event loop, no locking is needed.

Expose a simple pub/sub: `subscribe(cb) → unsubscribe` so the SSE endpoint receives `new` /
`update` / `cleared`.

### 2.3. Capture hook in the tunnel (two phases)

So the UI also shows in-flight requests, capture in two phases:

1. On receiving the request in `handle()` (before the local `fetch`): create the `CapturedRequest`
   with `phase:"pending"`, store it, and emit a `new` event (summary). Mark the start with
   `performance.now()`.
2. When you have the response (or the error): update
   `status/resHeaders/resBody/resBytes/durationMs`, set `phase:"done"` (or `"error"` + `error`), and
   emit `update` (summary).

**Critical:** capture must NOT alter the tunnel's behavior. The `respond()` to the relay still sends
the **full** body; the truncation in §2.4 applies **only to the copy stored for the inspector**.

### 2.4. Body limits

- `MAX_BODY` (default 2 MB): if `resBytes`/`reqBytes` exceeds it, store the body truncated to
  `MAX_BODY` and mark `truncated:true` in the summary/detail. What is sent to the relay goes in full.
- Binary bodies (images, gzip already decoded by fetch, octet-stream): stored the same way in
  base64. The binary/text rendering is decided in the UI by content-type (§2.6).

---

## 3. Local inspector API

Served by `Deno.serve` on `127.0.0.1:INSPECT_PORT`.

| Endpoint | Method | Description |
|---|---|---|
| `/` | GET | UI HTML (embedded as a template string, see §5) |
| `/api/state` | GET | `{relayUrl, localTarget, publicUrl, inspectPort, online?, pending?}` — config + optional relay state (calls `/__tunnel/status` with the secret) |
| `/api/requests?limit=N` | GET | array of summaries (default 200, newest first) |
| `/api/requests/:id` | GET | full `CapturedRequest` (with base64 bodies) or 404 |
| `/api/stream` | GET | SSE. Events: `event: new` / `event: update` with `data:` = summary JSON; `event: cleared` without data |
| `/api/requests/:id/replay` | POST | re-sends the request to the local target (outside the tunnel), creates a new entry with `replayOf` set, returns `{id}` |
| `/api/clear` | POST | empties the store, emits `cleared` |

Notes:
- The UI uses `fetch` for list/detail/replay/clear and opens an `EventSource` to `/api/stream` for
  real time. The `EventSource` reconnects on its own; on the server, send a keep-alive comment
  (`:\n\n`) every ~15s to keep the connection alive.
- **Replay** runs the same forwarding logic as `handle()` (same DROP_REQ, same `redirect:"manual"`)
  but against `LOCAL` directly, and captures the response as a new `phase:"done"/"error"` entry. It
  does not call `/__tunnel/respond`.

---

## 4. UI (single HTML, vanilla JS, no framework or build)

ngrok-style layout, two panels:

- **Header:** public URL (`https://tunnel.onvibe.run`) with a copy button, local target, and a
  connection badge (green if `state.online`, gray otherwise). "Clear" button.
- **Left panel (list):** rows with `time · method (color badge) · path · status (color by family
  2xx/3xx/4xx/5xx) · duration · size`. `pending` row with a spinner. New entries come in from the
  top via SSE. Filter box (substring over method+path+status).
- **Right panel (detail):** on selecting a row, `fetch /api/requests/:id` and show two blocks
  (Request / Response), each with a headers table and the body. Buttons: "Replay", "Copy as cURL"
  (optional, stretch), "Copy body".

Body rendering (decide by `content-type`):
- textual (`application/json`, `text/*`, `application/xml`, `application/x-www-form-urlencoded`,
  `*/*+json`, js/css): `b64decode` → UTF-8 → if JSON, pretty-print with `JSON.stringify(_,null,2)`.
- binary: show `binary · N bytes` and a download link (data URL from the base64).
- render cap at ~256 KB; if larger, show a notice + download.

Aesthetics: clean and native, monospace for headers/bodies, no external dependencies (all CSS/JS
inline). Do not use localStorage/sessionStorage except to remember the panel width or the filter
(optional).

---

## 5. Suggested file structure

Keep everything pure TS so `deno compile` keeps working (UI embedded as a string, no external
assets):

```
client.ts        # CLI + boot: parse config, create the store, launch tunnel + inspector
lib/tunnel.ts    # startTunnel({config, store}): worker()/handle()/respond() (moved from client.ts)
lib/store.ts     # RequestStore: ring buffer + Map + pub/sub (subscribe/emit)
lib/inspector.ts # startInspector({config, store}): Deno.serve with the API (§3) + embedded HTML (§4)
```

`client.ts` stays as the orchestrator:

```ts
const store = new RequestStore(MAX_LOG);
startInspector({ config, store });     // Deno.serve on 127.0.0.1:INSPECT_PORT
await startTunnel({ config, store });  // Promise.all(workers) — never resolves
```

New flags/env: `--inspect-port` / `INSPECT_PORT` (default 4040), `--no-inspect` to disable,
`--max-log` (default 500), `--max-body` (default 2097152).

On boot, log: `inspector on http://127.0.0.1:4040`.

---

## 6. Constraints and non-goals

- **Do not** modify the onvibe app (relay) or the SQL schema. The inspector is client-only.
- **Do not** change the tunnel's observable behavior: same relayed headers, full body to the relay,
  same error handling. Capture is a cheap side-effect.
- Deno runtime, **no new dependencies** if possible (stdlib is enough; if you need something,
  `jsr:@std/*` is acceptable, avoid npm unless truly needed).
- `deno compile --allow-net --allow-env -o mytunnel client.ts` → single binary must keep working.
  No reading UI files from disk at runtime.
- Inspector only on loopback (`127.0.0.1`), no HTTPS, no auth by default (like ngrok). Optional:
  `--inspect-token` flag that requires `?token=` to serve the UI/API.
- No disk persistence in v1 (entries live in memory). Optional persistence = stretch.

---

## 7. Edge cases to cover

- **Local down:** `fetch(LOCAL)` throws → `phase:"error"` entry with the message; the tunnel keeps
  returning 502 to the relay as before.
- **Binary bodies / images:** don't break; store base64, render as binary+download.
- **Large bodies:** truncate only the inspector's copy to `MAX_BODY`; mark `truncated`.
- **Concurrent requests:** several in flight at once (there are N workers). Each entry by its `id`;
  the `pending→done` phase updates in-place. The UI must support updates of existing rows.
- **SSE drops:** `EventSource` reconnects; on reconnect, the UI re-does `GET /api/requests` to
  resync (don't assume no events were missed).
- **`GET`/`HEAD` without body:** `reqBody` = null, `reqBytes` = 0.
- **Ring buffer full:** drop the oldest from the array and the Map consistently.

---

## 8. Acceptance criteria (definition of done)

1. `deno run --allow-net --allow-env client.ts --local 3000 --secret <S>` starts the tunnel and logs
   `inspector on http://127.0.0.1:4040`.
2. Opening `http://127.0.0.1:4040`, hitting the tunnel's public URL makes the request appear in the
   list in <1s (pending → done phase via SSE).
3. The detail shows request and response headers, and the body with JSON pretty-printing.
4. "Replay" re-sends to the local server and adds a new entry marked as replay, with its own
   response.
5. The filter filters; "Clear" empties the list.
6. A request with an image (e.g. `GET /favicon.ico` of a real app) does not corrupt anything: it
   shows as binary with size and download; the body relayed to the tunnel stays intact (the image
   loads fine in the browser hitting the public URL).
7. `deno compile ... -o mytunnel client.ts` produces a binary that works the same (inspector
   included).

---

## 9. Suggested incremental plan

1. Extract `worker()/handle()/respond()` from `client.ts` to `lib/tunnel.ts` with signature
   `startTunnel({config, store})`; verify the tunnel still works the same (no UI yet).
2. `lib/store.ts`: RequestStore + pub/sub; hook two-phase capture into `handle()` and the error
   catch.
3. `lib/inspector.ts`: `Deno.serve` with `/api/requests`, `/api/requests/:id`, `/api/state`. Test
   with `curl`.
4. Add `/api/stream` (SSE) + keep-alive.
5. Embedded UI: list + SSE + detail + body rendering.
6. `/api/requests/:id/replay` + button. `/api/clear` + button. Filter.
7. New flags, loopback binding, boot log. Verify `deno compile`.

---

## 10. How to test locally

```bash
# 1. Any local test server (or use your real app):
deno run --allow-net - <<'EOF'
Deno.serve({ port: 3000 }, (req) =>
  Response.json({ ok: true, path: new URL(req.url).pathname, method: req.method }));
EOF

# 2. The client with the inspector:
deno run --allow-net --allow-env client.ts --local 3000 --secret <YOUR_SECRET>

# 3. Generate traffic against the public URL and watch it in the inspector:
curl https://tunnel.onvibe.run/hola
curl -X POST https://tunnel.onvibe.run/eco -d '{"a":1}' -H 'content-type: application/json'
# open http://127.0.0.1:4040
```
