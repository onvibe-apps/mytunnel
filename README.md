# mytunnel

A single-user ngrok-style HTTP tunnel, built on top of **onvibe** (a PaaS that does not allow
persistent connections). The transport is a **relay over Postgres with double long-polling**,
not a persistent socket.

Two pieces:

- **Relay** (`app/`): an onvibe app you deploy yourself (this repo runs at
  `https://tunnel.onvibe.run`, but each user deploys their own and uses their own URL). It queues
  public traffic in Postgres and waits for the client's response. Deno + TypeScript.
- **Client** (`*.go`): a Go binary that runs on your machine. It long-polls the relay, forwards
  each request to your local server, and returns the response. Ships with a local **web
  inspector** (ngrok's `localhost:4040` style).

```
 public curl ─▶ relay (onvibe) ─▶ Postgres ─▶ Go client ─▶ http://localhost:PORT
                    ▲                             │
                    └──────── response ◀──────────┘
                                                  │
                                     web inspector at 127.0.0.1:4040
```

---

## Client (Go)

### Install

Download the binary for your platform from the [releases page](https://github.com/onvibe-apps/mytunnel/releases)
(macOS Intel/ARM, Linux amd64/arm64, Windows). Example for macOS Apple Silicon:

```bash
VER=v0.1.0
curl -LO "https://github.com/onvibe-apps/mytunnel/releases/download/$VER/mytunnel-$VER-darwin-arm64.tar.gz"
tar xzf "mytunnel-$VER-darwin-arm64.tar.gz"
./mytunnel version
```

Or build it yourself (single binary, no external dependencies; the inspector UI is embedded with
`//go:embed`):

```bash
go build -o mytunnel .
```

To publish a release, see [RELEASING.md](RELEASING.md) (`scripts/release.sh vX.Y.Z`).

### Configure (recommended: `setup`)

There is no default endpoint: **each user deploys their own relay** (a fork of `app/` on onvibe)
and configures its URL.

```bash
./mytunnel setup
#   Relay URL (e.g. https://your-relay.onvibe.run):
#   Secret (hidden): ********      ← not echoed as you type
#   Default local target [3000]:
```

`setup` stores the **URL and default local target** in `~/.config/mytunnel/config.json`
(macOS: `~/Library/Application Support/mytunnel/`, mode `0600`) and the **secret in the OS
keychain** (macOS Keychain), keyed by endpoint — each URL has its own secret. On Linux/Windows the
secret falls back to a `0600` `secrets.json`.

### Run

```bash
./mytunnel                       # uses the saved config + the secret from the keychain
./mytunnel --local 3000          # override just the local target
```

On startup it logs `inspector on http://127.0.0.1:4040`. Open that URL to watch traffic live.

**Secret precedence**: `--secret` > `TUNNEL_SECRET` > keychain (per endpoint) > interactive
prompt. **URL/local precedence**: flag > env > saved config.

> ⚠️ Passing `--secret` on the CLI exposes it in `ps` and shell history; `TUNNEL_SECRET` is a bit
> better but still visible in the process environment. For normal use prefer `mytunnel setup` (the
> secret lives in the keychain). The flag/env remain available as an override for CI.

### Configuration (flags or env)

| Flag | Env | Default | Description |
|---|---|---|---|
| `--url` | `TUNNEL_URL` | config (required) | Relay base URL (your deployment; no default) |
| `--secret` | `TUNNEL_SECRET` | keychain (via `setup`) | Shared secret; if missing, prompted for |
| `--local` | `LOCAL` | config | Local target: `3000` \| `localhost:3000` \| `http://127.0.0.1:8080` |
| `--concurrency` | `CONCURRENCY` | `8` | Parallel workers |
| `--inspect-port` | `INSPECT_PORT` | `4040` | Inspector port (loopback) |
| `--no-inspect` | — | — | Disable the inspector |
| `--max-log` | `MAX_LOG` | `500` | Requests kept in the ring buffer |
| `--max-body` | `MAX_BODY` | `2097152` | Max body stored by the inspector (bytes) |
| `--inspect-token` | `INSPECT_TOKEN` | — | Require `?token=` to serve the UI/API |
| `--no-allow-self` | — | — | Do not auto-register this machine's IP on the relay allowlist |
| `--allow-ttl` | `ALLOW_TTL` | `300` | Seconds a temporary allowed IP lives before refresh |

### Web inspector

Served on `127.0.0.1:INSPECT_PORT` (loopback only, no HTTPS or auth by default):

- **Live request list** (SSE), newest first, with method, path, status, duration and size.
  Filter by method/path/status. **Clear** button.
- **Per-request detail**: request and response headers and body, with JSON pretty-printing and
  binary render + download based on content-type.
- **Replay**: re-sends a captured request to your local server (outside the tunnel) and records
  it as a new entry.
- **Access**: manage the IP allowlist (see below).

API (for scripts): `GET /api/state`, `GET /api/requests[?limit=N]`, `GET /api/requests/:id`,
`GET /api/stream` (SSE), `POST /api/requests/:id/replay`, `POST /api/clear`,
`GET /api/whoami`, `GET|POST|DELETE /api/allowed`, `GET /api/denied`.

---

## IP allowlist (access control)

By default **the public tunnel only serves traffic from allowed IPs**. This keeps the tunnel from
being open to the whole internet while you develop.

- The client **auto-registers your IP** (the one the relay sees as the source) on startup and
  refreshes it periodically. If you stop the client, your IP **expires** after `--allow-ttl`
  seconds.
- From the inspector's **Access** panel you can:
  - Add extra IPs (e.g. for **webhooks**), **permanent** or **temporary** with a TTL.
  - See the **denied requests** (IP, method, path) and allow their IP with one click.
  - Edit the label of an entry, or remove an IP.
- To restore the open behavior (no allowlist), run the relay with `TUNNEL_ALLOWLIST=off`.

> The relay detects the source IP from `x-forwarded-for` (falling back to `x-real-ip` /
> `cf-connecting-ip`, and to `127.0.0.1` locally). `GET /__tunnel/whoami` returns the detected IP.

---

## Relay (onvibe app)

The app code lives in `app/` (`main.ts`, `db.ts`, `views.ts`), with the schema in `app/schema.sql`
and migrations in `app/migrations/`.

### Local development with onvibe-dev

[`onvibe-dev`](https://onvibe.run/blog/onvibe-dev) runs the app locally with the same runtime as
production. Requires [Deno](https://deno.com) and Node/npx.

```bash
# 1. Local Postgres + schema
createdb tunnel_dev
psql -d tunnel_dev -f app/schema.sql
psql -d tunnel_dev -c "INSERT INTO tunnel_meta (id) VALUES (1) ON CONFLICT DO NOTHING;"

# 2. Local relay on :8787 (with a dev secret)
TUNNEL_SECRET=devsecret npx -y https://onvibe.run/onvibe-dev.tgz ./app \
  --db postgres://$USER@localhost/tunnel_dev --port 8787 --watch

# 3. Go client pointing at the local relay
go run . --url http://localhost:8787 --secret devsecret --local 3000

# 4. Test traffic
curl http://localhost:8787/hola
```

Alternative without local Postgres: `--remote-db` or `--db-from fork-schema:tunnel` (ephemeral
online DB; needs `ONVIBE_API_KEY`). Other useful flags: `--migrate` (applies `migrations/*.sql` on
startup), `--as <email>` (auth simulation), `-w/--watch` (hot reload).

### Deploying relay changes to production

Schema changes go as forward-only migrations under `app/migrations/<version>.sql`. To avoid
touching real data, iterate in a **draft** and promote with `apply_changes` (see
`onvibe://docs/schema-migration`).

---

## Repository layout

```
main.go            # CLI + boot (config, subcommands, inspector, tunnel, allowlist heartbeat)
tunnel.go          # workers, long-poll, forward, respond, callRelay, heartbeat
store.go           # RequestStore: ring buffer + Map + pub/sub, body truncation
inspector.go       # http.Serve: inspector API + allowlist proxy
ui.html            # inspector UI (vanilla, embedded with //go:embed)
config.go          # config.json (0600) load/save
secretstore.go     # secret storage: OS keychain + 0600 fallback
setup.go           # `mytunnel setup` interactive command + prompts
app/               # onvibe relay (main.ts, db.ts, views.ts, schema.sql, migrations/)
scripts/release.sh # cross-compile + GitHub release (see RELEASING.md)
IDEAS.md           # roadmap / pending ideas
specs.md           # original inspector spec
```
