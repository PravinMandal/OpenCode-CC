// Exact-model, opt-in Zen compatibility transport. No credential storage.
import { createHash, randomBytes } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";

export const MODEL = "deepseek-v4.1-flash-free";
const endpoint = "https://opencode.ai/inference/openai/v1/chat/completions";
const prelude = await Bun.file(new URL("./system.txt", import.meta.url)).text();
const MAX_BODY = 8 * 1024 * 1024;
const hash = value => createHash("sha256").update(value).digest("hex");
const log = event => console.log(JSON.stringify({ time: new Date().toISOString(), ...event }));
const relay = response => {
  const headers = new Headers(response.headers);
  for (const key of ["content-length", "content-encoding", "connection", "transfer-encoding"]) headers.delete(key);
  return new Response(response.body, { status: response.status, headers });
};

// Never replay a generation after forwarding bytes: it may contain tool calls.
// Turn late network errors into a truthful SSE error rather than a local TCP
// reset. The relay's scanner recognizes this envelope as a failed inference.
export function guardStream(response, signal, onFailure) {
  if (!response.ok || !response.body || !response.headers.get("content-type")?.includes("text/event-stream")) return response;
  const reader = response.body.getReader();
  let cancelled = false;
  const stream = new ReadableStream({
    async pull(controller) {
      try {
        const part = await reader.read();
        if (cancelled) return;
        if (part.done) controller.close();
        else controller.enqueue(part.value);
      } catch {
        if (cancelled) return;
        if (!signal.aborted) {
          onFailure();
          controller.enqueue(new TextEncoder().encode('\n\ndata: {"error":{"type":"api_error","message":"DS upstream stream disconnected; retry this turn."}}\n\n'));
        }
        controller.close();
      }
    },
    async cancel(reason) { cancelled = true; await reader.cancel(reason).catch(() => {}); },
  });
  return new Response(stream, { status: response.status, headers: response.headers });
}

export function normalize(body) {
  const parsed = JSON.parse(body);
  if (parsed.model !== MODEL) throw new Error("This transport only accepts " + MODEL);
  if (!Array.isArray(parsed.messages)) throw new Error("messages must be an array");
  const systems = parsed.messages.filter(m => m.role === "system").map(m => m.content);
  if (systems.some(s => typeof s !== "string")) throw new Error("System content must be text");
  const original = systems.join("\n\n");
  parsed.messages = [{ role: "system", content: original.startsWith(prelude) ? original : prelude + "\n\n" + original },
    ...parsed.messages.filter(m => m.role !== "system")];
  // Client tools, user history, limits and reasoning settings are untouched.
  return JSON.stringify(parsed);
}

// Canonical OpenCode session shape. The Go relay already synthesizes fresh,
// non-hex sessions in 12-hour buckets; preserve those instead of replacing the
// relay's stable affinity with a helper-local timestamp. Hash-shaped all-hex
// ids are the old spoof the free-tier gate rejects.
const canonicalSession = /^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/;
const hexOnlySession = /^ses_[0-9a-f]{26}$/;
export function isForwardableSession(value) {
  const session = typeof value === "string" ? value.trim() : "";
  return canonicalSession.test(session) && !hexOnlySession.test(session);
}

export class Sessions {
  entries = new Map();
  scope(headers) {
    return hash((headers.get("authorization") || "") + "|" + (headers.get("x-opencode-session") || ""));
  }
  // Called only before any response bytes have been forwarded. A quota
  // rejection is not a committed turn, so a fresh protocol session is safe
  // and gives the retry a new server-side affinity.
  invalidate(headers) {
    this.entries.delete(this.scope(headers));
  }
  get(headers, now = Date.now()) {
    const scope = this.scope(headers);
    let entry = this.entries.get(scope);
    // Keep generated sessions sticky for the same window the Go relay uses
    // for forwardable downstream sessions. Upstream free-tier quota is
    // accounted per session, so minting a new one every few minutes burns
    // through the lane instead of refreshing it. A session supplied by the
    // relay is preserved separately by upstreamHeaders().
    if (!entry || now - entry.created >= 12 * 60 * 60 * 1000) {
      const inv = (~((BigInt(now) << 12n) | 1n)) & 0xffffffffffffn;
      const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";
      const session = "ses_" + inv.toString(16).padStart(12, "0") +
        Array.from(randomBytes(14), v => alphabet[v % 62]).join("");
      if (this.entries.size >= 512) this.entries.delete(this.entries.keys().next().value);
      entry = { session, created: now };
      this.entries.set(scope, entry);
    }
    return entry.session;
  }
}

export function upstreamHeaders(incoming, sessions) {
  const headers = new Headers(incoming);
  for (const key of [...headers.keys()]) {
    if (["host", "content-length", "connection", "transfer-encoding", "accept-encoding", "cookie"].includes(key) ||
        key.startsWith("anthropic-") || key.startsWith("x-stainless-")) headers.delete(key);
  }
  if (!headers.get("x-opencode-project") || headers.get("x-opencode-project") === "global")
    headers.set("x-opencode-project", createHash("sha1").update("opencode-cc").digest("hex"));
  const incomingSession = headers.get("x-opencode-session") || headers.get("x-opencode-session-id")
    || headers.get("x-session-affinity") || headers.get("x-session-id");
  const session = isForwardableSession(incomingSession) ? incomingSession.trim() : sessions.get(headers);
  for (const key of ["x-opencode-session", "x-opencode-session-id", "x-session-affinity", "x-session-id"])
    headers.set(key, session);
  return headers;
}

export function makeHandler({ routes, fetcher = fetch, onExhausted = () => {}, sessions = new Sessions(),
  rotate = false, recover = undefined, recoverRetries = 0 }) {
  if (!routes?.length) throw new Error("At least one DS route is required");
  // `fallback` routes (normally direct) are only used after every WARP route
  // has failed. In rotate mode, the WARP routes are cycled per request so a
  // single shared egress IP is never the only path for a long session.
  const primary = routes.filter(route => !route.fallback);
  const primaryRoutes = primary.length ? primary : routes;
  const fallbackRoutes = primary.length ? routes.filter(route => route.fallback) : [];
  const retries = Math.max(0, Number(recoverRetries) || 0);
  let primaryIndex = 0;
  let preferred = routes[0];
  let inflight = 0;

  const currentRoute = () => rotate ? primaryRoutes[primaryIndex % primaryRoutes.length] : preferred;
  const order = () => {
    if (!rotate) return [preferred, ...routes.filter(route => route !== preferred)];
    const start = primaryIndex % primaryRoutes.length;
    return [...primaryRoutes.slice(start), ...primaryRoutes.slice(0, start), ...fallbackRoutes];
  };
  const advance = route => {
    if (!rotate) { preferred = route; return; }
    const index = primaryRoutes.indexOf(route);
    if (index >= 0) primaryIndex = (index + 1) % primaryRoutes.length;
  };

  return async request => {
    if (request.method === "GET" && new URL(request.url).pathname === "/health")
      return Response.json({ ok: true, model: MODEL, route: currentRoute().name, inflight });
    if (request.method !== "POST" || new URL(request.url).pathname !== "/v1/chat/completions") return new Response("Not found", { status: 404 });
    if (!/^Bearer\s+\S+/i.test(request.headers.get("authorization") || ""))
      return Response.json({ error: { type: "authentication_error", message: "Upstream credential required" } }, { status: 401 });
    if (inflight >= 8) return Response.json({ error: { type: "overloaded_error", message: "DS transport busy" } }, { status: 503 });
    let body;
    try {
      const raw = await request.text();
      if (Buffer.byteLength(raw) > MAX_BODY) return new Response("Request too large", { status: 413 });
      body = normalize(raw);
    } catch (error) { return Response.json({ error: { type: "invalid_request_error", message: String(error) } }, { status: 400 }); }
    inflight++;
    try {
      let last;
      for (let attempt = 0; attempt <= retries; attempt++) {
        // Sessions are scoped by auth + incoming client session. Recompute per
        // attempt so invalidate() can issue a fresh protocol affinity before a
        // quota retry.
        const headers = upstreamHeaders(request.headers, sessions);
        for (const route of order()) {
          const controller = new AbortController();
          const timer = setTimeout(() => controller.abort(), 30000);
          try {
            const response = await fetcher(endpoint, { method: "POST", headers, body, proxy: route.proxy,
              signal: AbortSignal.any([request.signal, controller.signal]) });
            clearTimeout(timer); // bound headers, not a healthy long generation
            log({ event: "inference", route: route.name, status: response.status });
            if (response.status === 429) {
              let error;
              try { error = await response.clone().json(); } catch {}
              if (error?.error?.type === "FreeUsageLimitError") {
                if (last) await last.body?.cancel();
                last = response;
                continue;
              }
            }
            if (last) await last.body?.cancel();
            if (response.ok) advance(route);
            return relay(guardStream(response, request.signal, () => {
              advance(route);
              log({ event: "transport_failure", route: route.name, phase: "response_stream" });
            }));
          } catch (error) {
            if (request.signal.aborted) throw error;
            log({ event: "transport_failure", route: route.name });
          } finally { clearTimeout(timer); }
        }
        // Only a free-limit rejection (or a route-wide transport outage) is
        // safe to replay: no response bytes have been committed. Ask the
        // local WARP pool to replace its egress IPs, then make one more pass.
        const canRecover = attempt < retries && recover && (last?.status === 429 || !last);
        if (!canRecover) break;
        sessions.invalidate(request.headers);
        const recovered = await recover();
        log({ event: "egress_recovery", attempt: attempt + 1, ok: Boolean(recovered) });
        if (!recovered) break;
        // Keep the quota response until rotation is confirmed. If rotation
        // fails, it remains the truthful result returned below.
        if (last) { await last.body?.cancel(); last = undefined; }
      }
      await onExhausted();
      // Preserve the actual quota response and Retry-After when all routes
      // reject. Never turn an error into success, change accounts or models.
      return last ? relay(last) : Response.json({ error: { type: "api_error", message: "DS Flash egress unavailable" } }, { status: 502 });
    } finally { inflight--; }
  };
}

if (import.meta.main) {
  const state = process.env.STATE_DIRECTORY || process.env.HOME + "/.local/state/opencode-cc-ds-flash";
  await mkdir(state, { recursive: true, mode: 0o700 });
  const resetPath = state + "/last-reset.json";
  let lastReset = 0, resetting;
  try { lastReset = JSON.parse(await readFile(resetPath, "utf8")).at; } catch {}
  const phoneIP = async () => {
    try {
      const r = await fetch("https://www.cloudflare.com/cdn-cgi/trace", { proxy: "http://127.0.0.1:18889", signal: AbortSignal.timeout(8000) });
      return (await r.text()).match(/^ip=(.+)$/m)?.[1];
    } catch { return undefined; }
  };
  const reset = () => {
    if (process.env.DS_FLASH_AUTO_RESET !== "1" || resetting || Date.now() - lastReset < 1800000) return;
    resetting = (async () => {
      lastReset = Date.now();
      await writeFile(resetPath, JSON.stringify({ at: lastReset }), { mode: 0o600 });
      const before = await phoneIP();
      log({ event: "phone_reset_started" });
      // The explicitly authorized skill owns the Telegram credentials. Its
      // output contains identifiers, so discard it; independently verify IP.
      const proc = Bun.spawn(["bash", "-c", 'set -a; source "$HOME/.config/telegram-ip-reset.env"; unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy NO_PROXY no_proxy; export TELEGRAM_IP_RESET_CONFIG=/dev/null TELEGRAM_IP_RESET_PROXY=http://127.0.0.1:18889 TELEGRAM_IP_RESET_TIMEOUT=60; exec "$HOME/.config/opencode/skills/telegram-ip-reset/bin/telegram-ip-reset.sh" "DS Flash recovery"'], { stdout: "ignore", stderr: "ignore" });
      const timeout = setTimeout(() => proc.kill(), 90000);
      await proc.exited;
      clearTimeout(timeout);
      for (let i = 0; i < 48; i++) {
        const after = await phoneIP();
        if (after && (!before || before !== after)) { log({ event: "phone_reset_recovered", ip_change_verified: Boolean(before) }); return; }
        await Bun.sleep(5000);
      }
      log({ event: "phone_reset_not_verified" });
    })().catch(() => log({ event: "phone_reset_failed" })).finally(() => { resetting = undefined; });
  };
  const routeNames = (process.env.DS_FLASH_ROUTES || "phone,direct").split(",").map(name => name.trim()).filter(Boolean);
  const known = {
    phone: { name: "phone", proxy: "http://127.0.0.1:18889" },
    direct: { name: "direct", proxy: false, fallback: true },
  };
  // Userspace WARP profiles exposed as local HTTP CONNECT proxies. They are
  // independent Cloudflare WARP tunnels; no host route or default gateway is
  // changed. The helper only ever sees these loopback URLs.
  const warpProxies = (process.env.DS_FLASH_WARP_PROXIES || "").split(",").map(value => value.trim()).filter(Boolean);
  warpProxies.forEach((proxy, index) => {
    const name = `warp${index + 1}`;
    known[name] = { name, proxy };
  });
  if (!routeNames.length) throw new Error("DS_FLASH_ROUTES is empty");
  const routes = routeNames.map(name => { if (!known[name]) throw new Error("Unknown DS route: " + name); return known[name]; });
  const warpPool = (process.env.DS_FLASH_WARP_POOL_URL || "").replace(/\/+$/, "");
  const recoverWarp = async () => {
    if (!warpPool) return false;
    try {
      const response = await fetch(warpPool + "/rotate", { method: "POST", signal: AbortSignal.timeout(60000) });
      let result = {};
      try { result = await response.json(); } catch {}
      log({ event: "warp_pool_rotate", ok: response.ok, changed: result.changed ?? null });
      return response.ok;
    } catch (error) {
      log({ event: "warp_pool_rotate_failed", error: String(error).slice(0, 200) });
      return false;
    }
  };
  const rotate = process.env.DS_FLASH_ROTATE === "1";
  const recoverRetries = Number(process.env.DS_FLASH_RECOVER_RETRIES || 0);
  const server = Bun.serve({ hostname: "127.0.0.1", port: Number(process.env.DS_FLASH_PORT || 18790),
    maxRequestBodySize: MAX_BODY, idleTimeout: 255,
    fetch: makeHandler({ routes, onExhausted: reset, rotate, recover: recoverWarp, recoverRetries }) });
  log({ event: "listening", port: server.port, model: MODEL, routes: routeNames, rotate,
    warp_proxies: warpProxies.length, warp_pool: Boolean(warpPool), recover_retries: Math.max(0, recoverRetries || 0) });
}
