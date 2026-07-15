import { withErrorReporting } from "./.onvibe/helpers.ts";
import { pool } from "./db.ts";

// ── Config ────────────────────────────────────────────────
const CONTROL_PREFIX = "/__tunnel/";
const PUBLIC_WAIT_MS = 17_000; // how long a public request waits for the local response (< onvibe's ~20s TTFB cap)
const POLL_HOLD_MS = 15_000; // how long the client's long-poll is held open before returning 204
const OFFLINE_MS = 30_000; // if the client hasn't polled in this long, treat the tunnel as offline
// Allowlist: by default the tunnel only serves public traffic from allowed IPs.
// Set TUNNEL_ALLOWLIST=off to open it to everyone (legacy behavior).
const ALLOWLIST = (Deno.env.get("TUNNEL_ALLOWLIST") ?? "on").toLowerCase() !== "off";

// ── Helpers ──────────────────────────────────────────────
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

function b64encode(bytes: Uint8Array): string {
  let bin = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    bin += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(bin);
}
function b64decode(str: string): Uint8Array {
  const bin = atob(str);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function headersToObject(h: Headers): Record<string, string> {
  const o: Record<string, string> = {};
  h.forEach((v, k) => (o[k] = v));
  return o;
}

// Constant-time-ish comparison of the shared secret.
function authed(req: Request): boolean {
  const secret = Deno.env.get("TUNNEL_SECRET");
  if (!secret) return false;
  const given = req.headers.get("x-tunnel-secret") ?? "";
  if (given.length !== secret.length) return false;
  let diff = 0;
  for (let i = 0; i < secret.length; i++) diff |= given.charCodeAt(i) ^ secret.charCodeAt(i);
  return diff === 0;
}

// Best-effort source IP of the caller. Behind onvibe's edge the real client IP
// arrives in a forwarded header; fall back to loopback for local dev.
function clientIp(req: Request): string {
  const xff = req.headers.get("x-forwarded-for");
  if (xff) return xff.split(",")[0].trim();
  return req.headers.get("x-real-ip") ?? req.headers.get("cf-connecting-ip") ?? "127.0.0.1";
}

async function ipAllowed(ip: string): Promise<boolean> {
  const r = await pool.query(
    `SELECT 1 FROM tunnel_allowed_ips
      WHERE ip = $1 AND (permanent OR (expires_at IS NOT NULL AND expires_at > now()))
      LIMIT 1`,
    [ip],
  );
  return r.rows.length > 0;
}

const json = (data: unknown, status = 200) =>
  new Response(JSON.stringify(data), { status, headers: { "content-type": "application/json" } });
const text = (body: string, status: number) =>
  new Response(body, { status, headers: { "content-type": "text/plain; charset=utf-8" } });

// ── Control channel ──────────────────────────────────────

// The client long-polls here to receive the next queued request. Held open up to
// POLL_HOLD_MS; claims one pending row atomically (SKIP LOCKED lets several client
// workers poll in parallel without ever grabbing the same request).
async function handlePoll(req: Request): Promise<Response> {
  if (!authed(req)) return text("unauthorized", 401);

  await pool.query("UPDATE tunnel_meta SET last_poll_at = now() WHERE id = 1");
  // Backstop cleanup of anything abandoned (client died mid-flight, etc.).
  await pool.query("DELETE FROM tunnel_requests WHERE created_at < now() - interval '2 minutes'");

  const deadline = Date.now() + POLL_HOLD_MS;
  while (Date.now() < deadline) {
    const r = await pool.query(
      `UPDATE tunnel_requests SET status = 'claimed', claimed_at = now()
         WHERE id = (
           SELECT id FROM tunnel_requests
            WHERE status = 'pending'
            ORDER BY created_at
              FOR UPDATE SKIP LOCKED
            LIMIT 1
         )
       RETURNING id, method, path, headers, body`,
    );
    if (r.rows.length) {
      const row = r.rows[0];
      return json({
        request: {
          id: row.id,
          method: row.method,
          path: row.path,
          headers: row.headers,
          body: row.body, // base64 or null
        },
      });
    }
    await sleep(200);
  }
  return new Response(null, { status: 204 }); // no work; client re-polls immediately
}

// The client posts the local server's response back here, unblocking the waiting
// public request.
async function handleRespond(req: Request): Promise<Response> {
  if (!authed(req)) return text("unauthorized", 401);
  let payload: { id?: string; status?: number; headers?: Record<string, string>; body?: string | null };
  try {
    payload = await req.json();
  } catch {
    return json({ error: "bad json" }, 400);
  }
  if (!payload.id) return json({ error: "missing id" }, 400);
  await pool.query(
    `UPDATE tunnel_requests
        SET status = 'done', res_status = $2, res_headers = $3::jsonb, res_body = $4, done_at = now()
      WHERE id = $1`,
    [payload.id, payload.status ?? 200, JSON.stringify(payload.headers ?? {}), payload.body ?? null],
  );
  return json({ ok: true });
}

// Quick status/debug endpoint.
async function handleStatus(req: Request): Promise<Response> {
  if (!authed(req)) return text("unauthorized", 401);
  const m = await pool.query(
    "SELECT last_poll_at, (now() - last_poll_at) < make_interval(secs => $1) AS online FROM tunnel_meta WHERE id = 1",
    [OFFLINE_MS / 1000],
  );
  const q = await pool.query("SELECT count(*)::int AS pending FROM tunnel_requests WHERE status = 'pending'");
  return json({ online: m.rows[0]?.online ?? false, last_poll_at: m.rows[0]?.last_poll_at ?? null, pending: q.rows[0].pending });
}

// ── Allowlist control ────────────────────────────────────

// Returns the caller's IP as the edge sees it — the client uses this to know
// which IP to allowlist for its own browser traffic.
function handleWhoami(req: Request): Response {
  if (!authed(req)) return text("unauthorized", 401);
  return json({ ip: clientIp(req) });
}

async function handleAllowedList(req: Request): Promise<Response> {
  if (!authed(req)) return text("unauthorized", 401);
  const r = await pool.query(
    `SELECT ip, label, permanent, expires_at, created_at, last_seen_at
       FROM tunnel_allowed_ips ORDER BY created_at DESC`,
  );
  return json({ allowed: r.rows });
}

// Add or refresh an allowed IP. Body (all optional):
//   { ip?, label?, permanent?, ttl_seconds? }
// ip defaults to the caller's IP (the client registering its own current IP).
// permanent ⇒ never expires; otherwise expires after ttl_seconds (default 300).
async function handleAllowAdd(req: Request): Promise<Response> {
  if (!authed(req)) return text("unauthorized", 401);
  let p: { ip?: string; label?: string; permanent?: boolean; ttl_seconds?: number } = {};
  try {
    p = await req.json();
  } catch { /* empty body ⇒ register caller IP with defaults */ }
  const ip = (p.ip && String(p.ip).trim()) || clientIp(req);
  const label = p.label ? String(p.label) : null;
  const permanent = !!p.permanent;
  const ttl = Number.isFinite(p.ttl_seconds) ? Number(p.ttl_seconds) : 300;
  await pool.query(
    `INSERT INTO tunnel_allowed_ips (ip, label, permanent, expires_at, last_seen_at)
       VALUES ($1, $2, $3, CASE WHEN $3 THEN NULL ELSE now() + make_interval(secs => $4) END, now())
     ON CONFLICT (ip) DO UPDATE SET
       label = COALESCE(EXCLUDED.label, tunnel_allowed_ips.label),
       permanent = EXCLUDED.permanent,
       expires_at = CASE WHEN EXCLUDED.permanent THEN NULL ELSE now() + make_interval(secs => $4) END,
       last_seen_at = now()`,
    [ip, label, permanent, ttl],
  );
  const r = await pool.query(
    "SELECT ip, label, permanent, expires_at FROM tunnel_allowed_ips WHERE ip = $1",
    [ip],
  );
  return json({ ok: true, ip, entry: r.rows[0] ?? null });
}

async function handleAllowRemove(req: Request): Promise<Response> {
  if (!authed(req)) return text("unauthorized", 401);
  let p: { ip?: string } = {};
  try {
    p = await req.json();
  } catch { /* ignore */ }
  const ip = p.ip && String(p.ip).trim();
  if (!ip) return json({ error: "missing ip" }, 400);
  await pool.query("DELETE FROM tunnel_allowed_ips WHERE ip = $1", [ip]);
  return json({ ok: true });
}

async function handleDenied(req: Request): Promise<Response> {
  if (!authed(req)) return text("unauthorized", 401);
  const r = await pool.query(
    "SELECT ip, method, path, at FROM tunnel_denied ORDER BY at DESC LIMIT 100",
  );
  return json({ denied: r.rows, allowlist: ALLOWLIST });
}

// ── Public traffic ───────────────────────────────────────
// Everything that is not a control path gets queued and tunneled to the local machine.
async function handlePublic(req: Request, url: URL): Promise<Response> {
  // Fail fast if no client is connected.
  const meta = await pool.query(
    "SELECT (now() - last_poll_at) < make_interval(secs => $1) AS online FROM tunnel_meta WHERE id = 1",
    [OFFLINE_MS / 1000],
  );
  if (!meta.rows[0]?.online) {
    return text("Tunnel offline: no local client is connected.\n", 502);
  }

  // Access control: only allowed IPs get tunneled. Denied requests are recorded
  // so the client can surface them and offer to allowlist the source.
  if (ALLOWLIST) {
    const ip = clientIp(req);
    if (!(await ipAllowed(ip))) {
      await pool.query(
        "INSERT INTO tunnel_denied (ip, method, path) VALUES ($1, $2, $3)",
        [ip, req.method, url.pathname + url.search],
      );
      await pool.query("DELETE FROM tunnel_denied WHERE at < now() - interval '1 hour'");
      return text(`Forbidden: IP ${ip} is not allowed. Add it from the tunnel client inspector.\n`, 403);
    }
    await pool.query("UPDATE tunnel_allowed_ips SET last_seen_at = now() WHERE ip = $1", [ip]);
  }

  const id = crypto.randomUUID();
  const method = req.method;
  const path = url.pathname + url.search;
  const headers = headersToObject(req.headers);
  let body: string | null = null;
  if (method !== "GET" && method !== "HEAD") {
    const buf = new Uint8Array(await req.arrayBuffer());
    body = buf.length ? b64encode(buf) : null;
  }

  await pool.query(
    "INSERT INTO tunnel_requests (id, method, path, headers, body) VALUES ($1, $2, $3, $4::jsonb, $5)",
    [id, method, path, JSON.stringify(headers), body],
  );

  const deadline = Date.now() + PUBLIC_WAIT_MS;
  try {
    while (Date.now() < deadline) {
      const r = await pool.query(
        "SELECT status, res_status, res_headers, res_body FROM tunnel_requests WHERE id = $1",
        [id],
      );
      const row = r.rows[0];
      if (!row) return text("Tunnel error: request dropped.\n", 500);
      if (row.status === "done") {
        await pool.query("DELETE FROM tunnel_requests WHERE id = $1", [id]);
        const out = new Headers();
        for (const [k, v] of Object.entries(row.res_headers ?? {})) out.set(k, String(v));
        const bytes = row.res_body ? b64decode(row.res_body) : null;
        return new Response(bytes, { status: row.res_status ?? 200, headers: out });
      }
      await sleep(150);
    }
    await pool.query("DELETE FROM tunnel_requests WHERE id = $1", [id]);
    return text("Tunnel timeout: the local server did not respond in time.\n", 504);
  } catch (e) {
    await pool.query("DELETE FROM tunnel_requests WHERE id = $1", [id]).catch(() => {});
    return text("Tunnel error: " + (e as Error).message + "\n", 500);
  }
}

// ── Router ──────────────────────────────────────────────
async function handler(req: Request): Promise<Response> {
  const url = new URL(req.url);
  const p = url.pathname;

  if (p.startsWith(CONTROL_PREFIX)) {
    if (p === "/__tunnel/poll" && req.method === "POST") return handlePoll(req);
    if (p === "/__tunnel/respond" && req.method === "POST") return handleRespond(req);
    if (p === "/__tunnel/status" && req.method === "GET") return handleStatus(req);
    if (p === "/__tunnel/whoami" && req.method === "GET") return handleWhoami(req);
    if (p === "/__tunnel/allowed" && req.method === "GET") return handleAllowedList(req);
    if (p === "/__tunnel/allow" && req.method === "POST") return handleAllowAdd(req);
    if (p === "/__tunnel/allow" && req.method === "DELETE") return handleAllowRemove(req);
    if (p === "/__tunnel/denied" && req.method === "GET") return handleDenied(req);
    return text("not found", 404);
  }

  return handlePublic(req, url);
}

export default withErrorReporting(handler);
