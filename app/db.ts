import { Pool } from "npm:pg";

// pgConfig() is required for onvibe Postgres: ssl:false + decode the password
// + strip IPv6 brackets. See onvibe://docs/database.
function pgConfig(s: string) {
  const u = new URL(s);
  return {
    host: u.hostname.replace(/^\[|\]$/g, ""),
    port: parseInt(u.port) || 5432,
    user: u.username,
    password: decodeURIComponent(u.password),
    database: u.pathname.slice(1).split("?")[0],
    ssl: false,
    connectionTimeoutMillis: 8000,
    max: 12,
  };
}

export const pool = new Pool(pgConfig(Deno.env.get("DATABASE_URL")!));
