// Native free-tier transport for opencode-cc.
//
// opencode-cc translates Anthropic/OpenAI/Responses traffic into OpenAI Chat
// Completions, but Zen's free tier accepts a request only when it arrives over
// the same TLS/HTTP stack as the stock opencode CLI (Bun). The exact same body,
// headers, credential and session replayed through Go, Node, Python or curl is
// answered with FreeUsageLimitError. This process is therefore the one Bun
// hop; opencode-cc starts it on an ephemeral loopback port and owns its
// lifetime. There is no public :18790 service.
//
// This layer makes the request look exactly like a stock opencode CLI request:
// canonical system prompt, canonical tool prefix, stock headers and session
// shape, SSE, and per-IP quota recovery.
import { createHash, randomBytes } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";
import { Database } from "bun:sqlite";

const HOME = homedir();
const IDENTITY_DIR = process.env.BRIDGE_IDENTITY_DIR || join(HOME, ".local", "share", "opencode-native");
const IDENTITY_PATH = process.env.BRIDGE_IDENTITY || join(IDENTITY_DIR, "identity.json");
const CREDENTIAL_DB = process.env.BRIDGE_CREDENTIAL_DB || join(HOME, ".local", "share", "opencode", "opencode.db");
const AUTH_JSON = join(HOME, ".local", "share", "opencode", "auth.json");
const CAPTURE = process.env.BRIDGE_CAPTURE || join(IDENTITY_DIR, "capture.mjs");
const CATALOG_URL = "https://opencode.ai/inference/v1/models";
const PORT = Number(process.env.BRIDGE_PORT || 0);
const HOST = process.env.BRIDGE_HOST || "127.0.0.1";
// Only the owning opencode-cc process knows this token; it is an admission
// gate, not an upstream credential. The upstream bearer always comes from the
// live OpenCode credential database.
const TOKEN = process.env.BRIDGE_TOKEN || "";
const MAX_BODY = 32 * 1024 * 1024;
// The free tier's tool-schema gate is measured, not documented: through this
// bridge 37,381 chars of tools answers 200 while 39,588 gets the generic
// FreeUsageLimitError. Reject oversized surfaces here so callers see the real
// reason instead of a quota error that triggers credential rotation.
const MAX_TOOL_CHARS = Number(process.env.BRIDGE_MAX_TOOL_CHARS || 38000);
// A 100k-token context with reasoning can take well over 30s to first byte on a
// free-tier queue; aborting at 30s turned slow-but-fine turns into ECONNRESETs.
// Bun closes an idle socket at `idleTimeout` (255s below), so waiting longer
// than that only leaks an upstream fetch nobody is listening to.
const HEADER_TIMEOUT = Number(process.env.BRIDGE_HEADER_TIMEOUT_MS || 240_000);
const MAX_INFLIGHT = Number(process.env.BRIDGE_MAX_INFLIGHT || 8);
// Requests that start within the same few milliseconds are what the free tier
// answers with an instant 429, so space the starts out instead of firing a
// parallel burst at it.
const MIN_START_GAP_MS = Number(process.env.BRIDGE_MIN_START_GAP_MS || 400);
const RESET_ENGINE = process.env.BRIDGE_RESET_ENGINE || join(HOME, ".config", "opencode", "scripts", "request-reset.mjs");
// systemd gives this unit a bare PATH, so resolve node by absolute path first.
const NODE_BIN = [
  process.env.BRIDGE_NODE,
  join(HOME, ".local", "share", "mise", "installs", "node", "latest", "bin", "node"),
  "/usr/bin/node",
].find(p => p && existsSync(p)) || "node";
const FREE_LIMIT = /free usage limit/i;
const MAX_RECOVERY_ATTEMPTS = Number(process.env.BRIDGE_MAX_RECOVERY || 2);
// Measured on 2026-11-01: the free tier accepts the stock 13 OpenCode tools
// plus at most 6 client tools (19 total). A 20-tool request is answered with
// FreeUsageLimitError even when the stock probe on the same /64 succeeds, so
// the gate is the tool set, not the budget or the character count.
const MAX_TOOLS = Number(process.env.BRIDGE_MAX_TOOLS || 19);
// Opt-in request capture for diagnosing shape rejections. Off in production.
const TRACE_DIR = process.env.BRIDGE_TRACE_DIR || "";
// Circuit breaker: a 429 caused by request *shape* (wrong prompt prefix, stale
// date, odd tool list) must never cycle the hotspot. One rotation per interval,
// and only when the last one actually succeeded.
const RECOVERY_MIN_INTERVAL_MS = Number(process.env.BRIDGE_RECOVERY_INTERVAL_MS || 60_000);

const log = event => console.log(JSON.stringify({ time: new Date().toISOString(), ...event }));
const sha = v => createHash("sha256").update(v).digest("hex");

let identity = null;
let inflight = 0;

// ---------------------------------------------------------------- credentials
let credential = { value: "", readAt: 0 };
function liveCredential() {
  const now = Date.now();
  if (credential.value && now - credential.readAt < 60_000) return credential.value;
  let key = "";
  try {
    const db = new Database(CREDENTIAL_DB, { readonly: true });
    const row = db.query(
      "select value from credential where integration_id = 'opencode' and active = 1 limit 1").get();
    db.close();
    if (row?.value) {
      const parsed = JSON.parse(row.value);
      key = parsed.key || parsed.access || "";
    }
  } catch {}
  if (!key) {
    try {
      const auth = JSON.parse(readFileSync(AUTH_JSON, "utf8"));
      key = auth?.opencode?.key || auth?.opencode?.access || "";
    } catch {}
  }
  credential = { value: key, readAt: now };
  return key;
}

// ------------------------------------------------------------------ identity
async function loadIdentity() {
  try {
    const loaded = JSON.parse(await readFile(IDENTITY_PATH, "utf8"));
    if (typeof loaded.system === "string" && Array.isArray(loaded.tools) && loaded.tools.length) return loaded;
  } catch {}
  return null;
}

function installedVersion() {
  try {
    const out = Bun.spawnSync(["opencode", "--version"], { stdout: "pipe", stderr: "ignore" });
    return new TextDecoder().decode(out.stdout).trim().split("\n")[0];
  } catch {
    return "unknown";
  }
}

let refreshing = false;
async function refreshIdentity(reason) {
  if (refreshing) return;
  refreshing = true;
  log({ event: "identity_refresh_start", reason });
  try {
    const proc = Bun.spawn([process.execPath, CAPTURE], { stdout: "pipe", stderr: "pipe" });
    const code = await proc.exited;
    const out = (await new Response(proc.stdout).text()).trim().slice(0, 300);
    log({ event: "identity_refresh_done", code, out });
    const fresh = await loadIdentity();
    if (fresh) identity = fresh;
  } catch (error) {
    log({ event: "identity_refresh_failed", error: String(error) });
  } finally {
    refreshing = false;
  }
}

async function ensureIdentity() {
  identity = await loadIdentity();
  if (!identity) {
    await refreshIdentity("missing");
    if (!identity) throw new Error("no opencode identity available");
    return;
  }
  const installed = installedVersion();
  if (installed !== "unknown" && identity.version && installed !== identity.version) {
    log({ event: "identity_stale", have: identity.version, installed });
    refreshIdentity("version_change"); // keep serving the last known good identity meanwhile
  }
}

// ------------------------------------------------------------------- headers
const sessions = new Map();
const ID_ALPHABET = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";
let lastSessionMs = 0;
let sessionCounter = 0;

// Stock opencode session ids are `ses_` + 12 hex chars encoding the inverted
// millisecond timestamp and a per-millisecond counter, followed by 14 base62
// random chars. The free tier rejects ids that do not match this shape (a
// plain random id is answered with `FreeUsageLimitError`), so reproduce the
// client's generator exactly instead of inventing our own format.
function newSessionId() {
  const now = Date.now();
  if (now !== lastSessionMs) {
    lastSessionMs = now;
    sessionCounter = 0;
  }
  sessionCounter++;
  const raw = BigInt(now) * 0x1000n + BigInt(sessionCounter);
  const inverted = ~raw;
  const time = Array.from({ length: 6 }, (_, i) =>
    Number((inverted >> BigInt(40 - 8 * i)) & 0xffn).toString(16).padStart(2, "0")).join("");
  const random = Array.from(randomBytes(14), v => ID_ALPHABET[v % 62]).join("");
  return "ses_" + time + random;
}

function sessionFor(scope) {
  const now = Date.now();
  const hit = sessions.get(scope);
  if (hit && now - hit.at < 240_000) return hit.id;
  const id = newSessionId();
  if (sessions.size > 512) sessions.delete(sessions.keys().next().value);
  sessions.set(scope, { id, at: now });
  return id;
}

function traceIds() {
  const hex = n => Array.from(randomBytes(n), v => v.toString(16).padStart(2, "0")).join("");
  const t = hex(16), s = hex(8);
  return { b3: `${t}-${s}-1-${hex(8)}`, traceparent: `00-${t}-${s}-01` };
}

function baseHeaders() {
  const ident = identity?.headers || {};
  return {
    accept: "*/*",
    "content-type": "application/json",
    "user-agent": ident["user-agent"] || "opencode/latest/cli",
    "x-opencode-client": ident["x-opencode-client"] || "cli",
    "x-opencode-project": ident["x-opencode-project"] || "global",
    ...(ident["x-opencode-org-id"] ? { "x-opencode-org-id": ident["x-opencode-org-id"] } : {}),
  };
}

// Accept a Headers object, a Request, or a plain header map. A refactor once
// passed the whole Request here, which made every inference die with
// "inbound.get is not a function"; keep this tolerant so a caller cannot
// take the whole bridge down over a header lookup.
function headerValue(source, name) {
  if (!source) return "";
  if (typeof source.get === "function") return source.get(name) || "";
  if (source.headers) return headerValue(source.headers, name);
  return source[name] ?? source[name.toLowerCase()] ?? "";
}

function upstreamHeaders(inbound) {
  const scope = sha(
    headerValue(inbound, "authorization") + "|" + headerValue(inbound, "x-opencode-session") +
    "|" + headerValue(inbound, "x-session-id"));
  const session = sessionFor(scope);
  const headers = new Headers({
    ...baseHeaders(),
    "accept-encoding": "gzip, deflate, br, zstd",
    "x-opencode-session": session,
    "x-opencode-session-id": session,
    "x-session-affinity": session,
    "x-session-id": session,
    ...traceIds(),
  });
  const key = liveCredential();
  if (key) headers.set("authorization", `Bearer ${key}`);
  return headers;
}

// ---------------------------------------------------------------------- body
const toolName = tool => tool?.function?.name || tool?.name || "";

function normalize(raw) {
  const parsed = JSON.parse(raw);
  if (!Array.isArray(parsed.messages) || parsed.messages.length === 0)
    throw new Error("messages must be a non-empty array");
  if (!identity) throw new Error("no opencode identity captured yet");

  // The canonical system prompt goes first so the upstream prompt cache matches
  // the stock client byte for byte. The caller's own system text becomes a
  // second system message and keeps full effect.
  //
  // The captured prompt embeds the capture day ("Today's date: ..."), and the
  // free tier rejects a prompt whose date is not today. Substitute it at
  // request time so an identity captured yesterday keeps working.
  const canonical = identity.system.replace(
    /^Today's date: .*$/m, `Today's date: ${new Date().toDateString()}`);
  const messages = parsed.messages;
  const clientSystem = messages.filter(m => m.role === "system");
  const rest = messages.filter(m => m.role !== "system");
  const isCanonical = m => typeof m.content === "string" &&
    (m.content === identity.system || m.content === canonical);
  parsed.messages = [
    { role: "system", content: canonical },
    ...clientSystem.filter(m => !isCanonical(m)),
    ...rest,
  ];

  // Zen replaced the legacy prompt_cache_retention field with
  // prompt_cache_options. DeepSeek tolerates the old name, but a routed model
  // (GLM) rejects the whole request with unsupported_parameter, so normalise
  // to the stock client's shape before anything leaves this process.
  if (parsed.prompt_cache_retention !== undefined) {
    delete parsed.prompt_cache_retention;
    if (!parsed.prompt_cache_options) parsed.prompt_cache_options = { mode: "implicit" };
  }

  // Canonical tools come first: their names are part of the cached prefix.
  // Extra client tools are appended; duplicates keep the canonical definition.
  // The free tier accepts at most MAX_TOOLS total, so trim extras here rather
  // than sending a request that is guaranteed a shape 429. Client order is
  // preserved: a client puts its most important tools first.
  const canonicalTools = identity.tools || [];
  const canonicalNames = new Set(canonicalTools.map(toolName));
  let extraTools = (parsed.tools || []).filter(t => !canonicalNames.has(toolName(t)));
  if (canonicalTools.length && extraTools.length > Math.max(0, MAX_TOOLS - canonicalTools.length)) {
    const keep = Math.max(0, MAX_TOOLS - canonicalTools.length);
    log({
      event: "tools_trimmed", model: parsed.model, canonical: canonicalTools.length,
      kept: keep, dropped: extraTools.length - keep,
      // Sample, not the whole list: Codex ships ~90 tools and a full dump
      // makes the journal unreadable without adding diagnostic value. Names
      // can legitimately repeat across MCP namespaces, so dedupe the sample.
      droppedSample: [...new Set(extraTools.slice(keep).map(toolName))].slice(0, 8),
    });
    extraTools = extraTools.slice(0, keep);
  }
  if (canonicalTools.length) parsed.tools = [...canonicalTools, ...extraTools];

  // The free tier answers a non-streaming request with the same generic
  // FreeUsageLimitError it uses for unknown clients, even when quota is
  // available. Always ask upstream for SSE and rebuild a non-stream response
  // for callers that requested one.
  const wantsStream = parsed.stream === true;
  parsed.stream = true;
  parsed.stream_options = { ...(parsed.stream_options || {}), include_usage: true };

  // Request shape, so a 429 can be traced to what the caller actually sent:
  // tool count, system size, and whether reasoning is being replayed.
  const shape = {
    tools: (parsed.tools || []).length,
    messages: parsed.messages.length,
    systemChars: parsed.messages.filter(m => m.role === "system")
      .reduce((n, m) => n + (typeof m.content === "string" ? m.content.length : JSON.stringify(m.content).length), 0),
    toolChars: JSON.stringify(parsed.tools || []).length,
    messageChars: JSON.stringify(parsed.messages).length,
    reasoningChars: parsed.messages
      .reduce((n, m) => n + (typeof m.reasoning_content === "string" ? m.reasoning_content.length : 0), 0),
  };

  // A no-op guard is worse than none: this threshold is the measured free-tier
  // boundary minus a safety margin, so a caller that grows its tool surface is
  // told exactly what broke instead of getting a quota 429. Only the free tier
  // has this gate; paid models may legitimately carry larger tool surfaces.
  const capped = /free/i.test(String(parsed.model || "")) || process.env.BRIDGE_FORCE_TOOL_LIMIT === "1";
  if (capped && shape.toolChars > MAX_TOOL_CHARS) {
    log({ event: "shape_rejected", reason: "tool_surface_too_large", ...shape, maxToolChars: MAX_TOOL_CHARS });
    throw new Error(
      `tool surface too large: ${shape.toolChars} chars across ${shape.tools} tools ` +
      `exceeds the free-tier limit of ${MAX_TOOL_CHARS}; trim tools or set BRIDGE_MAX_TOOL_CHARS`);
  }

  return { body: JSON.stringify(parsed), wantsStream, shape, canonicalCount: canonicalTools.length };
}

// ------------------------------------------------------------------- routing
const json = (body, status = 200) => Response.json(body, { status });

// Pull one complete JSON object out of the stream tail. The usage object nests
// prompt_tokens_details, so a `[^}]*` match truncates it into unparseable text.
function extractUsage(text) {
  const at = text.lastIndexOf('"usage"');
  if (at < 0) return null;
  const start = text.indexOf("{", at);
  if (start < 0) return null;
  let depth = 0;
  for (let i = start; i < text.length; i++) {
    if (text[i] === "{") depth++;
    else if (text[i] === "}" && --depth === 0) return text.slice(start, i + 1);
  }
  return null;
}

// Count what actually crossed the wire and pull the token accounting out of the
// final stream chunk, so quota consumption is measurable instead of guessed.
// The client aborts most long requests, which cancels the stream: report on
// cancel too, otherwise the only requests that get measured are the short ones.
function tap(response, meta) {
  if (!response.body) return response;
  let bytes = 0;
  let tail = "";
  let done = false;
  const decoder = new TextDecoder();
  const report = aborted => {
    if (done) return;
    done = true;
    log({ event: "usage", ...meta, bytes, aborted, usage: extractUsage(tail) });
  };
  const stream = response.body.pipeThrough(new TransformStream({
    transform(chunk, controller) {
      bytes += chunk?.byteLength ?? 0;
      tail = (tail + decoder.decode(chunk ?? new Uint8Array(), { stream: true })).slice(-8000);
      controller.enqueue(chunk);
    },
    flush() { report(false); },
    cancel() { report(true); },
  }));
  // fetch decompresses the body, so forwarding the upstream framing headers
  // would leave clients with a gzip claim and a stale length.
  const headers = new Headers(response.headers);
  for (const key of ["content-length", "content-encoding", "connection", "transfer-encoding"]) headers.delete(key);
  return new Response(stream, { status: response.status, headers });
}

// Upstream is always asked for SSE (see normalize). Rebuild a non-stream
// completion for callers that did not ask for a stream.
async function aggregate(response, meta) {
  const text = await response.text();
  let id = null, created = null, model = null, role = "assistant", finish = null, usage = null;
  let content = "", reasoning = "";
  let done = false;
  const calls = [];
  for (const line of text.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed.startsWith("data:")) continue;
    const data = trimmed.slice(5).trim();
    if (!data) continue;
    if (data === "[DONE]") { done = true; continue; }
    let chunk;
    try { chunk = JSON.parse(data); } catch { continue; }
    id ??= chunk.id;
    created ??= chunk.created;
    model ??= chunk.model;
    usage = chunk.usage || usage;
    const choice = chunk.choices?.[0];
    if (!choice) continue;
    finish = choice.finish_reason || finish;
    const delta = choice.delta || choice.message || {};
    if (delta.role) role = delta.role;
    if (typeof delta.content === "string") content += delta.content;
    if (typeof delta.reasoning_content === "string") reasoning += delta.reasoning_content;
    for (const tc of delta.tool_calls || []) {
      const idx = tc.index ?? 0;
      const cur = calls[idx] || (calls[idx] = { id: "", type: "function", function: { name: "", arguments: "" } });
      if (tc.id) cur.id = tc.id;
      if (tc.type) cur.type = tc.type;
      if (tc.function?.name) cur.function.name += tc.function.name;
      if (tc.function?.arguments) cur.function.arguments += tc.function.arguments;
    }
  }
  const message = { role, content: content || null };
  if (reasoning) message.reasoning_content = reasoning;
  if (calls.length) message.tool_calls = calls.filter(Boolean);
  log({ event: "usage", ...meta, bytes: text.length, done, usage: usage ? JSON.stringify(usage) : null });
  // A stream that ends with neither [DONE] nor a finish_reason was cut short.
  // Returning the partial text as a normal answer makes a broken turn look
  // complete, which is worse than an error the caller can retry.
  if (!done && !finish) {
    log({ event: "upstream_truncated", ...meta, bytes: text.length, contentChars: content.length });
    return json({ error: { type: "upstream_truncated", message: "upstream stream ended before a terminal event" } }, 502);
  }
  return json({
    id: id || `chatcmpl-${randomBytes(8).toString("hex")}`,
    object: "chat.completion",
    created: created || Math.floor(Date.now() / 1000),
    model: model || meta.model,
    choices: [{ index: 0, message, finish_reason: finish || "stop" }],
    ...(usage ? { usage } : {}),
  }, response.status);
}

// --------------------------------------------------------------- start pacing
let startGate = Promise.resolve();
let lastStartAt = 0;
function paceStart() {
  const slot = startGate.then(async () => {
    const wait = lastStartAt + MIN_START_GAP_MS - Date.now();
    if (wait > 0) await Bun.sleep(wait);
    lastStartAt = Date.now();
  });
  startGate = slot.catch(() => {});
  return slot;
}

// ------------------------------------------------------------ quota recovery
// The free tier's budget is per public IPv6 /64. When it runs out, the right
// move is to rotate the prefix and replay the request, not to hand the caller a
// 429 it will turn into a failed turn. One recovery runs at a time; everybody
// waiting on it retries afterwards on the new address.
let recovery = null;
let lastRecovery = { at: 0, code: null, rotated: false };
let lastAttemptAt = 0;

async function recoverQuota(reason, force) {
  if (recovery) return recovery;
  lastAttemptAt = Date.now();
  recovery = (async () => {
    const started = Date.now();
    log({ event: "recovery_start", reason, force });
    if (!existsSync(RESET_ENGINE)) {
      log({ event: "recovery_unavailable", engine: RESET_ENGINE });
      recovery = null;
      return false;
    }
    try {
      const proc = Bun.spawn([NODE_BIN, RESET_ENGINE, `--source=opencode-cc`, ...(force ? ["--force"] : [])], {
        stdout: "pipe", stderr: "pipe", env: process.env,
      });
      const killer = setTimeout(() => { try { proc.kill(); } catch {} }, 150_000);
      const code = await proc.exited;
      clearTimeout(killer);
      // request-reset.mjs logs to stdout, not stderr. Exit 0 also covers
      // "skip: a reset succeeded Ns ago", which is not a rotation: treating it
      // as one armed the breaker and blocked the real reset for minutes.
      const out = (await new Response(proc.stdout).text()).trim().slice(-600);
      const rotated = /\bok via\b/.test(out);
      if (rotated) lastRecovery = { at: Date.now(), code, rotated };
      log({ event: "recovery_done", code, rotated, ms: Date.now() - started, out });
      return rotated;
    } catch (error) {
      log({ event: "recovery_failed", error: String(error) });
      return false;
    } finally {
      recovery = null;
    }
  })();
  return recovery;
}

// Does the free tier still serve a request that looks exactly like the stock
// client? If yes, the 429 is about this request's shape, not the budget, and
// rotating the phone's /64 would only drop every other client for ~30s.
let probeCache = { at: 0, status: null, model: null };
async function stockProbe(model) {
  if (probeCache.status !== null && probeCache.model === model && Date.now() - probeCache.at < 15_000)
    return probeCache.status;
  let status = -1;
  try {
    const body = normalize(JSON.stringify({
      model, max_tokens: 1, stream: true,
      messages: [{ role: "user", content: "ok" }],
    })).body;
    const response = await fetch(identity.endpoint, {
      method: "POST", headers: upstreamHeaders({}), body, signal: AbortSignal.timeout(45_000),
    });
    status = response.status;
    await response.text().catch(() => {});
  } catch (error) {
    status = -1;
  }
  probeCache = { at: Date.now(), status, model };
  return status;
}

// canonicalOnlyBody rebuilds a normalized body with just the stock tool
// prefix. The free tier accepts the stock set on every model, so this is the
// last resort when a client's extra tools still trip the shape gate.
function canonicalOnlyBody(raw, canonicalCount) {
  try {
    const parsed = JSON.parse(raw);
    if (!Array.isArray(parsed.tools) || parsed.tools.length <= canonicalCount) return null;
    parsed.tools = parsed.tools.slice(0, canonicalCount);
    return JSON.stringify(parsed);
  } catch {
    return null;
  }
}

async function upstream(body, inbound, meta) {
  let transportFailures = 0;
  let recoveryWaits = 0;
  let shapeRetried = false;
  for (let attempt = 1; ; attempt++) {
    await paceStart();
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), HEADER_TIMEOUT);
    let response;
    try {
      response = await fetch(identity.endpoint, {
        method: "POST",
        headers: upstreamHeaders(inbound),
        body,
        signal: AbortSignal.any([inbound.signal, controller.signal]),
      });
    } catch (error) {
      // A network reset changes the egress while a request is in flight, so the
      // next attempt can die on a stale socket. Retry instead of surfacing 502.
      if (inbound.signal.aborted || transportFailures >= 2) throw error;
      transportFailures++;
      log({ event: "transport_retry", ...meta, attempt, error: String(error) });
      await Bun.sleep(transportFailures === 1 ? 750 : 2000);
      continue;
    } finally {
      clearTimeout(timer);
    }
    log({ event: "inference", ...meta, attempt, status: response.status, ms: Date.now() - meta.started });

    // Only an SSE body can be rebuilt into a completion. An upstream error
    // body (400/500 JSON) must pass through with its real status, or the
    // caller loses the provider's message behind a fake "truncated stream".
    if (response.status !== 429) {
      if (meta.wantsStream) return tap(response, meta);
      const type = response.headers.get("content-type") || "";
      if (!response.ok || !type.includes("event-stream")) return tap(response, meta);
      return aggregate(response, meta);
    }

    // 429 bodies are tiny: read them to tell "budget gone" apart from "this
    // request looked unfamiliar", which retry differently.
    const text = await response.text();
    const exhausted = FREE_LIMIT.test(text);
    log({ event: "upstream_429", ...meta, attempt, exhausted, body: text.slice(0, 400) });
    if (exhausted) {
      const probe = await stockProbe(meta.model);
      log({ event: "quota_probe", ...meta, attempt, status: probe });
      const throttled = lastAttemptAt && Date.now() - lastAttemptAt < RECOVERY_MIN_INTERVAL_MS;
      // Only rotate when the stock shape is rejected too. A shape rejection is
      // fixed by changing the request, not by cycling the hotspot.
      if (probe !== 200 && attempt <= MAX_RECOVERY_ATTEMPTS && !throttled) {
        await recoverQuota(`free-limit attempt ${attempt}`, false);
        continue;
      }
      // The stock probe succeeded, so this is a shape rejection. Retry once
      // with only the stock tool prefix; some client tool sets trip the gate
      // even under the character cap.
      if (probe === 200 && !shapeRetried && meta.canonicalCount > 0 &&
          (meta.shape?.tools ?? 0) > meta.canonicalCount) {
        const trimmed = canonicalOnlyBody(body, meta.canonicalCount);
        if (trimmed) {
          shapeRetried = true;
          let toolChars = meta.shape?.toolChars ?? 0;
          try { toolChars = JSON.stringify(JSON.parse(trimmed).tools).length; } catch {}
          body = trimmed;
          meta = { ...meta, shape: { ...meta.shape, tools: meta.canonicalCount, toolChars } };
          log({ event: "shape_retry", ...meta, attempt, tools: meta.canonicalCount, toolChars });
          continue;
        }
      }
      // Another request is already rotating. Wait for its result instead of
      // returning a 429 that the caller would only retry into the same wall.
      // Bounded, because this loop has no other exit.
      if (recovery && recoveryWaits < 1) {
        recoveryWaits++;
        const rotated = await recovery;
        if (rotated) continue;
      }
      const headers = new Headers({ "content-type": response.headers.get("content-type") || "application/json" });
      // Never arm a long cooldown in the relay/CPA/proxy layers.
      headers.set("retry-after", probe === 200 ? "30" : "5");
      // Tell the Go layer (and a human reading the log) whether this 429 is a
      // budget wall that needs a new egress prefix, or a request-shape problem
      // that rotation cannot fix.
      headers.set("x-oc-native", "1");
      headers.set("x-oc-reset", probe === 200 ? "shape" : "needed");
      if (lastRecovery?.at) headers.set("x-oc-reset-last", new Date(lastRecovery.at).toISOString());
      if (throttled) headers.set("x-oc-reset-throttled", "1");
      return new Response(text, { status: 429, headers });
    }
    const headers = new Headers({ "content-type": response.headers.get("content-type") || "application/json" });
    headers.set("retry-after", "2");
    headers.set("x-oc-native", "1");
    headers.set("x-oc-reset", "shape");
    return new Response(text, { status: 429, headers });
  }
}

let catalog = { at: 0, body: null };
async function models() {
  if (catalog.body && Date.now() - catalog.at < 300_000) return catalog.body;
  try {
    const response = await fetch(CATALOG_URL, { headers: baseHeaders(), signal: AbortSignal.timeout(10_000) });
    if (response.ok) {
      catalog = { at: Date.now(), body: await response.text() };
      return catalog.body;
    }
  } catch {}
  return catalog.body || JSON.stringify({ object: "list", data: [] });
}

const server = Bun.serve({
  port: PORT,
  hostname: HOST,
  maxRequestBodySize: MAX_BODY,
  idleTimeout: 255,
  async fetch(request) {
    const url = new URL(request.url);
    // Admission gate: only the opencode-cc process that spawned us knows this
    // token, so nothing else on the loopback interface can borrow the
    // first-party transport. Loopback alone is not an authorization boundary.
    if (TOKEN && request.headers.get("x-bridge-token") !== TOKEN) {
      return json({ error: { type: "unauthorized", message: "native transport token missing" } }, 401);
    }
    if (request.method === "GET" && url.pathname === "/health") {
      return json({
        ok: Boolean(identity), port: server.port, inflight,
        pacing: { minStartGapMs: MIN_START_GAP_MS, headerTimeoutMs: HEADER_TIMEOUT, maxRecovery: MAX_RECOVERY_ATTEMPTS },
        recovery: { running: Boolean(recovery), last: lastRecovery, lastAttemptAt, probe: probeCache },
        identity: identity
          ? { version: identity.version, capturedAt: identity.capturedAt,
              systemLen: identity.system.length, tools: identity.tools.length }
          : null,
        installed: installedVersion(),
        credential: Boolean(liveCredential()),
      });
    }
    if (request.method === "GET" && (url.pathname === "/v1/models" || url.pathname === "/models")) {
      return new Response(await models(), { headers: { "content-type": "application/json" } });
    }
    if (request.method !== "POST" || !url.pathname.endsWith("/chat/completions")) {
      return json({ error: { type: "not_found", message: `no route ${request.method} ${url.pathname}` } }, 404);
    }
    if (!identity) return json({ error: { type: "unavailable", message: "opencode identity not captured yet" } }, 503);
    if (inflight >= MAX_INFLIGHT) return json({ error: { type: "overloaded_error", message: "bridge busy" } }, 503);

    let normalized;
    try {
      normalized = normalize(await request.text());
    } catch (error) {
      return json({ error: { type: "invalid_request_error", message: String(error?.message || error) } }, 400);
    }
    const body = normalized.body;

    let model = "-";
    try { model = JSON.parse(body).model; } catch {}

    if (TRACE_DIR) {
      try {
        await Bun.write(join(TRACE_DIR, `${Date.now()}-${String(model).replace(/[^\w.-]+/g, "_")}.json`), body);
        log({ event: "trace", model, bytes: body.length, shape: normalized.shape });
      } catch (error) { log({ event: "trace_failed", error: String(error) }); }
    }

    inflight++;
    const started = Date.now();
    try {
      return await upstream(body, request, {
        model, started, bytes: body.length, wantsStream: normalized.wantsStream, shape: normalized.shape,
        canonicalCount: normalized.canonicalCount,
      });
    } catch (error) {
      if (request.signal.aborted) return new Response(null, { status: 499 });
      log({ event: "transport_failure", model, ms: Date.now() - started, error: String(error) });
      return json({ error: { type: "server_error", message: "upstream transport failed" } }, 502);
    } finally {
      inflight--;
    }
  },
});

await ensureIdentity();
// The supervisor reads this line to learn the ephemeral port. Keep the
// "listening" event name stable; it is the Go side's readiness signal.
log({
  event: "listening", port: server.port, host: HOST, identity: identity?.version ?? null,
  systemLen: identity?.system?.length ?? 0, tools: identity?.tools?.length ?? 0,
});

// Re-capture when the client upgrades.
setInterval(() => {
  const installed = installedVersion();
  if (!identity || (installed !== "unknown" && installed !== identity.version)) {
    log({ event: "identity_watch", installed, have: identity?.version ?? null });
    refreshIdentity("watch");
  }
}, 60 * 60 * 1000);
