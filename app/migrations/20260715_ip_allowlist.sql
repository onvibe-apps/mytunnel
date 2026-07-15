-- IP allowlist for the public tunnel.
-- Idempotent (safe to re-run / apply on startup with onvibe-dev --migrate).

CREATE TABLE IF NOT EXISTS tunnel_allowed_ips (
  ip           text PRIMARY KEY,
  label        text,
  permanent    boolean NOT NULL DEFAULT false,
  expires_at   timestamptz,          -- NULL when permanent
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz
);

-- Recent denied requests, so the client can surface them and offer to allowlist.
CREATE TABLE IF NOT EXISTS tunnel_denied (
  id     bigserial PRIMARY KEY,
  ip     text NOT NULL,
  method text NOT NULL,
  path   text NOT NULL,
  at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS tunnel_denied_at_idx ON tunnel_denied (at DESC);
