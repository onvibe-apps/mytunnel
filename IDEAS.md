# Ideas / roadmap

## IP access control (allowlist on the relay) — ✅ IMPLEMENTED

> Implemented. Relay: gate in `handlePublic` + `tunnel_allowed_ips`/`tunnel_denied` tables +
> `/__tunnel/whoami|allowed|allow|denied` endpoints. Go client: heartbeat that auto-registers the
> IP (label `system`, refreshable TTL) + inspector proxy + **Access** panel (add/remove permanent
> or temporary IPs, edit label, view denied and allowlist them). Escape hatch:
> `TUNNEL_ALLOWLIST=off`. See README.

Goal: keep the public tunnel from being open to the whole internet by default.

- **By default the app (relay) does NOT accept external requests.** All public traffic arrives
  blocked unless the source IP is on the allowlist.
- **API for the client to add its current IP.** The client (from the user's machine) calls a relay
  endpoint that registers the public IP it egresses from, authenticated with `TUNNEL_SECRET`.
- **Expiry on inactivity.** An IP added by the client expires after a while without traffic; it
  must be refreshed (implicit heartbeat on each request, or periodic client refresh).
- **Extra IPs from the client UI.** In the local inspector you can add other IPs (e.g. for
  third-party webhooks), either **permanent** or **temporary**.
- **Denial visibility.** Ability to see requests from disallowed IPs (denied) to decide whether to
  add them to the allowlist with a click.

Design notes:
- Requires touching the onvibe app (the relay) — until now the inspector spec said not to touch
  it; this line of work does. Developed locally with `onvibe-dev`.
- Data model: allowed IP table `{ip, label, permanent, expires_at, created_at, last_seen_at}`.
- New relay endpoints:
  - `POST /__tunnel/allow` (secret): register/refresh the client's current IP (temporary).
  - `POST /__tunnel/allow` with `{ip, permanent}`: add a specific IP (webhooks).
  - `DELETE /__tunnel/allow` with `{ip}`: remove an IP.
  - `GET /__tunnel/allowed`: list allowed IPs.
  - `GET /__tunnel/denied`: recent denied requests (IP, path, ts) to allowlist them.
- The client inspector consumes these endpoints to manage everything from the UI.

## Secure secret / API key storage in the client — ✅ IMPLEMENTED

> Implemented. `mytunnel setup` (prompt with hidden input) stores the URL/local in a `0600`
> `config.json` and the secret in the **macOS Keychain** (fallback to a `0600` `secrets.json` on
> other OSes), keyed by endpoint. Resolution: `--secret` > `TUNNEL_SECRET` > keychain > prompt.
> Flag/env remain as an override for CI. See `config.go`, `secretstore.go`, `setup.go`, README.

Possible future improvements:
- Avoid the momentary passing of the secret through `security`'s argv (use `security`'s stdin if
  possible).
- Native Secret Service (Linux) / Credential Manager (Windows) support instead of the file
  fallback.
