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
// shape, and SSE. A 429 is surfaced as-is: quota recovery is a manual operator
// decision, never an automatic probe, retry or egress rotation.
import { createHash, randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
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
// reason instead of a quota error that looks like budget exhaustion.
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
// The free tier accepts the captured stock tool set; any extra tool name makes
// the request look like an unknown client and comes back as
// FreeUsageLimitError even when budget remains. Keep client-only tools out of
// the wire by default. Capability is not lost: the Go layer translates the
// canonical call to the client's own tool (shell->Bash, read->Read, skill->
// Skill, plus argument keys). Set BRIDGE_CLIENT_TOOL_EXTRAS=1 only to
// experiment with a model whose gate is known to accept extras.
const CLIENT_TOOL_EXTRAS = process.env.BRIDGE_CLIENT_TOOL_EXTRAS === "1";
// Upper bound for the opt-in extras experiment, including the canonical set.
const MAX_TOOLS = Number(process.env.BRIDGE_MAX_TOOLS || 19);
// Opt-in request capture for diagnosing shape rejections. Off in production.
const TRACE_DIR = process.env.BRIDGE_TRACE_DIR || "";

const log = event => console.log(JSON.stringify({ time: new Date().toISOString(), ...event }));
const sha = v => createHash("sha256").update(v).digest("hex");

let identity = null;
let inflight = 0;
// Last client tool-name set reported as suppressed, so a long session logs the
// decision once instead of on every request.
let suppressedSignature = "";

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

// Canonical tool names and their Claude Code equivalents. Used only to avoid
// paying a free-tier slot twice for one capability: the proxy translates the
// canonical call back to the client's tool, including argument keys.
const CLIENT_TO_CANONICAL = {
  bash: "shell", run_command: "shell",
  read: "read", read_file: "read",
  write: "write", write_file: "write",
  edit: "edit", edit_file: "edit",
  glob: "glob", grep: "grep",
  webfetch: "webfetch", websearch: "websearch",
  skill: "skill", agent: "subagent", task: "subagent",
  askuserquestion: "question",
};
const canonicalToolKey = name =>
  CLIENT_TO_CANONICAL[String(name || "").toLowerCase()] || "";

// Tools that add capability the canonical set cannot provide. Kept first when
// the free-tier budget cannot hold every client tool.
const DEFAULT_CLIENT_TOOL_PRIORITY =
  "ToolSearch,DeferredToolPlaceholder,ReportFindings,ScheduleWakeup,Workflow,ListAgents";
const clientToolPriority = () =>
  (process.env.BRIDGE_CLIENT_TOOL_PRIORITY || DEFAULT_CLIENT_TOOL_PRIORITY)
    .split(",").map(s => s.trim()).filter(Boolean);

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

  // Canonical tools only, by default. Their names are part of the cached
  // prefix, and the free tier treats a different tool set as an unknown client
  // and answers FreeUsageLimitError. Sending a client's extras therefore costs
  // an extra upstream request per turn (the retry at the stock shape) and made
  // every turn 2-3x as expensive. Translation keeps the capability: a canonical
  // shell/read/skill call is rewritten to the client's Bash/Read/Skill.
  const canonicalTools = identity.tools || [];
  const canonicalNames = new Set(canonicalTools.map(toolName));
  let extraTools = [];
  if (CLIENT_TOOL_EXTRAS) {
    const canonicalKey = canonicalToolKey;
    const allExtras = (parsed.tools || []).filter(t => !canonicalNames.has(toolName(t)));
    const isDuplicate = t => {
      const key = canonicalKey(toolName(t));
      return Boolean(key) && canonicalNames.has(key);
    };
    const priority = clientToolPriority();
    const rank = t => {
      const i = priority.indexOf(toolName(t));
      return i === -1 ? priority.length : i;
    };
    const unique = allExtras.filter(t => !isDuplicate(t))
      .map((t, i) => ({ t, i }))
      .sort((a, b) => rank(a.t) - rank(b.t) || a.i - b.i)
      .map(x => x.t);
    const keep = Math.max(0, MAX_TOOLS - canonicalTools.length);
    extraTools = unique.slice(0, keep);
    if (extraTools.length < keep) {
      const chosen = new Set(extraTools.map(toolName));
      for (const t of allExtras) {
        if (extraTools.length >= keep) break;
        if (chosen.has(toolName(t))) continue;
        extraTools.push(t);
        chosen.add(toolName(t));
      }
    }
    if (allExtras.length) {
      log({
        event: "tools_extras_applied", model: parsed.model, canonical: canonicalTools.length,
        kept: extraTools.length, dropped: allExtras.length - extraTools.length,
      });
    }
  } else {
    const suppressed = (parsed.tools || []).filter(t => !canonicalNames.has(toolName(t)));
    if (suppressed.length) {
      const names = [...new Set(suppressed.map(toolName))];
      const signature = [...names].sort().join(",");
      if (signature !== suppressedSignature) {
        suppressedSignature = signature;
        log({
          event: "tools_suppressed", model: parsed.model, canonical: canonicalTools.length,
          dropped: suppressed.length,
          droppedSample: names.slice(0, 8),
        });
      }
    }
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

  return { body: JSON.stringify(parsed), wantsStream, shape };
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

// ---------------------------------------------------------------- upstream
// A 429 is never probed, retried, or “recovered” here. Retrying a
// FreeUsageLimitError only spends more of the same budget and hid the real
// state behind automatic rotation, which is an operator-only decision.
async function upstream(body, inbound, meta) {
  let transportFailures = 0;
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

    // Free-tier quota. Do not probe the budget, do not retry, do not rotate the
    // egress: all three are forbidden, and a retry spends the same budget twice.
    // Surface the provider's own status and Retry-After so the caller knows the
    // real state.
    const text = await response.text();
    log({
      event: "upstream_429", ...meta, attempt,
      exhausted: /free usage limit/i.test(text),
      body: text.slice(0, 400),
    });
    const headers = new Headers({
      "content-type": response.headers.get("content-type") || "application/json",
    });
    const retryAfter = response.headers.get("retry-after");
    if (retryAfter) headers.set("retry-after", retryAfter);
    headers.set("x-oc-native", "1");
    headers.set("x-oc-reset", "manual");
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
        pacing: { minStartGapMs: MIN_START_GAP_MS, headerTimeoutMs: HEADER_TIMEOUT },
        tools: { canonicalOnly: !CLIENT_TOOL_EXTRAS },
        quota: { automaticRecovery: false },
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
