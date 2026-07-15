-- Current schema of the deployed `tunnel` relay app (introspected 2026-07-15).
-- Reference only — the live schema is managed on onvibe. Evolve it with
-- forward-only migrations under app/migrations/<version>.sql + apply_migrations.

CREATE TABLE IF NOT EXISTS tunnel_meta (
  id           integer PRIMARY KEY,
  last_poll_at timestamptz
);
-- Seeded with a single row: INSERT INTO tunnel_meta (id) VALUES (1);

CREATE TABLE IF NOT EXISTS tunnel_requests (
  id          text PRIMARY KEY,
  method      text NOT NULL,
  path        text NOT NULL,
  headers     jsonb NOT NULL,
  body        text,               -- base64 or null
  status      text NOT NULL DEFAULT 'pending', -- 'pending' | 'claimed' | 'done'
  res_status  integer,
  res_headers jsonb,
  res_body    text,               -- base64 or null
  created_at  timestamptz NOT NULL DEFAULT now(),
  claimed_at  timestamptz,
  done_at     timestamptz
);

-- IP allowlist (see migrations/20260715_ip_allowlist.sql).
CREATE TABLE IF NOT EXISTS tunnel_allowed_ips (
  ip           text PRIMARY KEY,
  label        text,
  permanent    boolean NOT NULL DEFAULT false,
  expires_at   timestamptz,          -- NULL when permanent
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz
);

CREATE TABLE IF NOT EXISTS tunnel_denied (
  id     bigserial PRIMARY KEY,
  ip     text NOT NULL,
  method text NOT NULL,
  path   text NOT NULL,
  at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS tunnel_denied_at_idx ON tunnel_denied (at DESC);
