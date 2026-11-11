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
// shape, and SSE. The free tier leases quota to the established session id, so
// this process persists its ids and falls back to the last accepted id when a
// new conversation has no lease yet. A real FreeUsageLimitError (the leased id
// itself is exhausted) is answered once, the egress is rotated through the
// shared reset script, and the same request is retried a single time. Every
// non-OpenCode harness (Claude Code, Codex, T3) reaches Zen through here, so
// recovery must live here and not only in the OpenCode plugin.
import { createHash, randomBytes } from "node:crypto";
import {
  chmodSync, closeSync, existsSync, fsyncSync, mkdirSync, openSync, readFileSync, readdirSync,
  renameSync, statSync, unlinkSync, writeFileSync, writeSync,
} from "node:fs";
import { chmod, readFile, writeFile, mkdir } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";
import { Database } from "bun:sqlite";

const HOME = homedir();
const IDENTITY_DIR = process.env.BRIDGE_IDENTITY_DIR || join(HOME, ".local", "share", "opencode-native");
const IDENTITY_PATH = process.env.BRIDGE_IDENTITY || join(IDENTITY_DIR, "identity.json");
const CREDENTIAL_DB = process.env.BRIDGE_CREDENTIAL_DB || join(HOME, ".local", "share", "opencode", "opencode.db");
const AUTH_JSON = process.env.BRIDGE_AUTH_JSON || join(HOME, ".local", "share", "opencode", "auth.json");
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
// A socket reset caused by an egress rotation is worth retrying; a header
// timeout is not. The retry budget is shared across attempts so a black-holed
// upstream cannot pin MAX_INFLIGHT slots for 3 x HEADER_TIMEOUT.
const TRANSPORT_RETRY_BUDGET_MS = Number(process.env.BRIDGE_TRANSPORT_RETRY_BUDGET_MS || 30_000);
// A successful reset is the recovery the request was waiting for. Grant one
// immediate attempt even if the reset finished at the deadline; without this
// the post-reset `continue` hit the deadline check and turned a recovered
// quota into a 502. The grace attempt is capped by the retry budget.
const POST_RESET_GRACE_MS = Math.min(HEADER_TIMEOUT, TRANSPORT_RETRY_BUDGET_MS);
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
// Keep the full canonical tool set even when the client cannot run a tool.
// Escape hatch for the free-tier shape gate: if a shorter prefix is rejected,
// set this to 1 and the client note alone discourages the dead call.
const KEEP_DEAD_TOOLS = process.env.BRIDGE_KEEP_DEAD_TOOLS === "1";
// The canonical <env> block names the directory capture.mjs ran in. Rewriting
// it to the client's real working directory keeps the prompt truthful. Set
// BRIDGE_REWRITE_ENV=0 to ship the captured block untouched.
const REWRITE_ENV = process.env.BRIDGE_REWRITE_ENV !== "0";
// Upper bound for the opt-in extras experiment, including the canonical set.
const MAX_TOOLS = Number(process.env.BRIDGE_MAX_TOOLS || 19);
// Opt-in request capture for diagnosing shape rejections. Off in production.
const TRACE_DIR = process.env.BRIDGE_TRACE_DIR || "";
// Free-tier requests are also limited by prompt size in the current window.
// Keep the newest tool results verbatim; older ones are replaced with a short
// preview. The stored client history is untouched, and the model can re-run a
// tool when it needs the full output. This is request-time pruning, not a
// smaller context window.
const PRUNE_TOOL_RESULTS = process.env.BRIDGE_PRUNE_TOOL_RESULTS !== "0";
const PRUNE_KEEP_RESULTS = Math.max(2, Number(process.env.BRIDGE_PRUNE_KEEP_RESULTS || 8) || 8);
const PRUNE_PREVIEW_CHARS = Math.max(200, Number(process.env.BRIDGE_PRUNE_PREVIEW_CHARS || 600) || 600);
const PRUNE_MARKER = "\n\n[opencode-cc: older tool output pruned to fit the free-tier window. Re-run the tool if you need the full result.]";
// Routed models disagree about the prompt-cache field. DeepSeek and GLM accept
// `prompt_cache_options`; some smaller models reject the whole request with
// 400 unsupported_parameter. Remember which model rejected it so later turns
// send the shape that model accepts instead of paying a failed round trip.
const cacheOptionsRejected = new Set();

function stripPromptCacheFields(body, model) {
  let parsed;
  try { parsed = JSON.parse(body); } catch { return null; }
  let changed = false;
  for (const key of ["prompt_cache_options", "prompt_cache_retention", "prompt_cache_key"]) {
    if (key in parsed) {
      delete parsed[key];
      changed = true;
    }
  }
  if (!changed) return null;
  // Store the canonical name: the request builder checks cacheOptionsRejected
  // with cleanModelName(...), so storing the raw alias would never match and
  // every turn would pay the same 400 repair again.
  if (model) cacheOptionsRejected.add(cleanModelName(model));
  return JSON.stringify(parsed);
}

function toolContentSize(content) {
  if (typeof content === "string") return content.length;
  try { return JSON.stringify(content ?? "").length; } catch { return 0; }
}

function pruneOldToolResults(messages) {
  if (!PRUNE_TOOL_RESULTS || !Array.isArray(messages)) return { pruned: 0, chars: 0 };
  const idx = [];
  for (let i = 0; i < messages.length; i++) {
    if (messages[i]?.role === "tool") idx.push(i);
  }
  if (idx.length <= PRUNE_KEEP_RESULTS) return { pruned: 0, chars: 0 };

  let keep = new Set(idx.slice(-PRUNE_KEEP_RESULTS));
  // A handful of huge recent results can still exceed the window's remaining
  // budget. In that case keep the newest three and prune the rest.
  const keptChars = [...keep].reduce((n, i) => n + toolContentSize(messages[i].content), 0);
  if (keptChars > 64_000 && idx.length > 3) {
    keep = new Set(idx.slice(-3));
  }

  let pruned = 0;
  let chars = 0;
  for (const i of idx) {
    if (keep.has(i)) continue;
    const m = messages[i];
    const before = toolContentSize(m.content);
    if (before <= PRUNE_PREVIEW_CHARS + PRUNE_MARKER.length) continue;
    const text = typeof m.content === "string" ? m.content : JSON.stringify(m.content ?? "");
    m.content = text.slice(0, PRUNE_PREVIEW_CHARS) + PRUNE_MARKER;
    pruned++;
    chars += before - PRUNE_PREVIEW_CHARS;
  }
  return { pruned, chars };
}
// Free-tier quota recovery. The reset script is the single writer of reset
// state and owns the radio; this layer only decides when to invoke it and
// retries the request once. A concurrent reset (the OpenCode plugin, or a
// sibling request) is joined rather than duplicated.
const RESET_SCRIPT = process.env.BRIDGE_RESET_SCRIPT ||
  join(HOME, ".config", "opencode", "scripts", "request-reset.mjs");
const RESET_SOURCE = process.env.BRIDGE_RESET_SOURCE || "opencode-cc";
const RESET_NODE = process.env.BRIDGE_NODE || "node";
const RESET_COOLDOWN_MS = Number(process.env.BRIDGE_RESET_COOLDOWN_MS || 60_000);
// The engine caps a whole run at RESET_BUDGET_MS=540s (IPv6 repair, ADB
// fallback, probes, Telegram retries). A shorter caller timeout would kill a
// healthy reset and surface a 429 while the radio is still cycling.
const RESET_TIMEOUT_MS = Number(process.env.BRIDGE_RESET_TIMEOUT_MS || 600_000);
// The engine heartbeats the lock every 20s, caps a run at BUDGET_MS=540s and
// treats a lock older than BUDGET_MS + 60s as stale. Match that 600s exactly;
// a longer threshold made a live-but-hung lock unreclaimable while the caller
// was still waiting.
const RESET_LOCK_STALE_MS = Number(process.env.BRIDGE_RESET_LOCK_STALE_MS || 540_000 + 60_000);
// The shared wait must outlast the stale threshold, or the caller gives up at
// the same instant the lock becomes reclaimable.
const RESET_SHARED_WAIT_MS = Number(process.env.BRIDGE_RESET_SHARED_WAIT_MS || RESET_LOCK_STALE_MS + 60_000);
const RESET_LOCK = process.env.BRIDGE_RESET_LOCK ||
  join(HOME, ".local", "share", "opencode", "reset.lock");
const RESET_STATE = process.env.BRIDGE_RESET_STATE ||
  join(HOME, ".local", "share", "opencode", "reset-state.json");

const log = event => console.log(JSON.stringify({ time: new Date().toISOString(), ...event }));
const sha = v => createHash("sha256").update(v).digest("hex");

let identity = null;
let inflight = 0;
// Last client tool-name set reported as suppressed, so a long session logs the
// decision once instead of on every request.
let suppressedSignature = "";

// ---------------------------------------------------------------- credentials
let credential = { value: "", readAt: 0 };
function liveCredential(forceRefresh = false, override = "") {
  // A key supplied by the Go layer (multi-key pool) wins over the local
  // OpenCode database. Same-account keys share the captured identity, so this
  // does not change the request shape.
  if (override) return override;
  const now = Date.now();
  if (!forceRefresh && credential.value && now - credential.readAt < 60_000) return credential.value;
  let key = "";
  let db = null;
  try {
    db = new Database(CREDENTIAL_DB, { readonly: true });
    const row = db.query(
      "select value from credential where integration_id = 'opencode' and active = 1 limit 1").get();
    if (row?.value) {
      const parsed = JSON.parse(row.value);
      key = parsed.key || parsed.access || "";
    }
  } catch {
  } finally {
    // A locked/corrupt OpenCode DB must not leak one handle per refresh.
    try { db?.close(); } catch {}
  }
  if (!key) {
    try {
      const auth = JSON.parse(readFileSync(AUTH_JSON, "utf8"));
      key = auth?.opencode?.key || auth?.opencode?.access || "";
    } catch {}
  }
  credential = { value: key, readAt: now };
  return key;
}

// Credential fingerprint: the free-tier lease belongs to the account that
// minted it. Mixing this into the scope (and tagging every stored id with it)
// means a credential/account rotation starts a new lease lane instead of
// pinning the old account's id forever. Go deletes the inbound Authorization
// before this leg, so the sidecar's own live credential is the real input.
function credentialFingerprint(forceRefresh = false, override = "") {
  return sha(liveCredential(forceRefresh, override) || "anonymous").slice(0, 16);
}

// accountScope is the stable identity of the account, not of one key. Leases
// and identity captures belong to the account: rotating an expired key must
// not throw away an established quota lease or force a full recapture.
function accountScope() {
  const org = identity?.headers?.["x-opencode-org-id"] || identity?.headers?.["x-opencode-workspace-id"] || "";
  return org ? "account:" + org : "";
}

// A credential from a different account must not inherit this account's
// workspace header or session lease. Zen answers with "Workspace access
// denied" when the org in the header does not own the bearer key. The set is
// learned on the first 403 and remembered for the process lifetime, so a pool
// holding keys from several accounts works without any account configuration.
const foreignCredentials = new Set();

function inboundCredential(inbound) {
  return String(headerValue(inbound, "x-bridge-credential") || "").trim();
}

// identityReady reports whether the capture has the fields the free-tier gate
// needs, including the account/workspace header. An identity written by an
// older bridge may predate that header; capturing once is cheap compared with
// sending a request the upstream will reject.
function identityReady() {
  return !!(identity && typeof identity.system === "string" &&
    Array.isArray(identity.tools) && identity.tools.length && accountScope());
}

// leaseAccount scopes lease reuse to the account. When the identity has no
// org header yet, fall back to the key fingerprint for a stable bootstrap
// scope. Once the capture is loaded, key rotation no longer resets leases.
function leaseAccount(override = "") {
  const fp = override ? credentialFingerprint(false, override) : "";
  if (fp && foreignCredentials.has(fp)) return "cred:" + fp;
  return accountScope() || ("cred:" + credentialFingerprint(false, override));
}

// ------------------------------------------------------------------ identity
// Per-model captures are cached with a short TTL: a refresh rewrites the files
// in place, and a cache with no expiry would keep serving the pre-upgrade
// prompt for the life of the process.
const MODEL_IDENTITY_TTL_MS = Number(process.env.BRIDGE_IDENTITY_TTL_MS || 600_000);
const modelIdentities = new Map();
// Models this process has actually served. Captures for models nobody asks for
// (a delisted free model) must not make the hourly watch re-capture the whole
// catalogue forever.
const requestedModels = new Set();
// Model names become filenames (`identity-<model>.json`). Anything that is
// not a plain lowercase name is rejected rather than sanitized: a client must
// not be able to make the sidecar read or write outside the identity
// directory with a name such as `x/../../evil-free`. The same rule is enforced
// again in capture.mjs before it builds an output path.
const MODEL_NAME_RE = /^[a-z0-9][a-z0-9._-]*$/;
const MAX_MODEL_NAME_CHARS = 128;
const cleanModelName = name => {
  const cleaned = String(name || "").toLowerCase()
    .replace(/^opencode\//, "").replace(/^oc\//, "");
  // Model names become filenames and journal fields. A 32 MiB name must not
  // reach a write path or an unbounded log line.
  if (cleaned.length > MAX_MODEL_NAME_CHARS) return "";
  if (!cleaned || !MODEL_NAME_RE.test(cleaned) || cleaned.includes("..")) return "";
  return cleaned;
};

function specificModelIdentity(modelName) {
  const clean = cleanModelName(modelName);
  if (!clean) return null;
  const hit = modelIdentities.get(clean);
  if (hit && Date.now() - hit.at < MODEL_IDENTITY_TTL_MS) return hit.ident;
  try {
    const loaded = JSON.parse(readFileSync(join(IDENTITY_DIR, `identity-${clean}.json`), "utf8"));
    if (typeof loaded.system === "string" && Array.isArray(loaded.tools) && loaded.tools.length) {
      modelIdentities.set(clean, { ident: loaded, at: Date.now() });
      return loaded;
    }
  } catch {}
  return null;
}

function getModelIdentity(modelName) {
  return specificModelIdentity(modelName) || identity;
}

// hasSpecificModelIdentity reports whether this model has its own usable
// capture. A truncated or `{}` file is not a capture: treating its mere
// existence as one skipped the Model ID rename and the refresh, so the request
// silently served the primary model's prompt under another model's name.
function hasSpecificModelIdentity(modelName) {
  return Boolean(specificModelIdentity(modelName));
}

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

// Spawning `opencode --version` is not free, and it is only used to notice an
// upgrade; a ten-minute cache is more than fresh enough.
let versionCache = { value: null, at: 0 };
function installedVersionCached() {
  if (versionCache.value && Date.now() - versionCache.at < 600_000) return versionCache.value;
  const value = installedVersion();
  versionCache = { value, at: Date.now() };
  return value;
}

let refreshing = false;
let activeCapture = null;
// A refresh walks every known model sequentially. The old 120 s cap killed
// the tail of a 10-model capture on every run, so the budget is now large
// enough for a per-model timeout (capture.mjs reads the same variable).
const CAPTURE_TIMEOUT_MS = Number(process.env.BRIDGE_CAPTURE_TIMEOUT_MS || 300_000);
// Models requested while a capture is already running. The old guard dropped
// them entirely, so a newly used model could stay on the primary capture for
// the life of the process. Queue them and run one follow-up batch.
let pendingCaptureModels = new Set();
function killCapture(signal) {
  const proc = activeCapture;
  if (!proc?.pid) return;
  try { process.kill(-proc.pid, signal); }
  catch { try { proc.kill(signal); } catch {} }
}
process.on("exit", () => { try { killCapture("SIGKILL"); } catch {} });
async function refreshIdentity(reason, models) {
  if (refreshing) {
    for (const model of models || []) {
      const clean = cleanModelName(model);
      if (clean) pendingCaptureModels.add(clean);
    }
    return;
  }
  refreshing = true;
  const captureStartedAt = Date.now();
  log({ event: "identity_refresh_start", reason, models: models || null });
  try {
    const env = { ...process.env };
    if (Array.isArray(models) && models.length) env.CAPTURE_MODELS = models.join(",");
    const proc = Bun.spawn([process.execPath, CAPTURE], {
      stdout: "pipe", stderr: "pipe", env, detached: true,
    });
    activeCapture = proc;
    // A hung `opencode --standalone run` must not pin `refreshing` forever:
    // every later refresh would be skipped and the stale capture served
    // indefinitely. SIGTERM first so capture.mjs can kill its own child;
    // SIGKILL the whole group if it does not exit.
    const timer = setTimeout(() => {
      killCapture("SIGTERM");
      setTimeout(() => killCapture("SIGKILL"), 2000).unref?.();
    }, CAPTURE_TIMEOUT_MS);
    const code = await proc.exited;
    clearTimeout(timer);
    activeCapture = null;
    const out = (await new Response(proc.stdout).text()).trim().slice(0, 300);
    log({ event: "identity_refresh_done", code, out });
    const fresh = await loadIdentity();
    const freshCapturedAt = Date.parse(fresh?.capturedAt || "");
    if (fresh && freshCapturedAt >= captureStartedAt) {
      identity = fresh;
    } else if (fresh) {
      // Capture failed and left the old file in place. Keep serving the old
      // identity; the next scheduled refresh will try again.
      log({ event: "identity_refresh_not_current", capturedAt: fresh.capturedAt || null });
    }
    // The per-model cache holds the old captures; drop it so the next lookup
    // reads the files the refresh just wrote.
    modelIdentities.clear();
  } catch (error) {
    log({ event: "identity_refresh_failed", error: String(error) });
  } finally {
    refreshing = false;
    if (pendingCaptureModels.size) {
      const queued = [...pendingCaptureModels];
      pendingCaptureModels = new Set();
      setTimeout(() => {
        refreshIdentity("queued_models", queued).catch(error =>
          log({ event: "identity_refresh_failed", error: String(error) }));
      }, 0).unref?.();
    }
  }
}

// Models whose captured prompt predates the installed opencode build. Only the
// primary identity was ever version-checked, so a stale GLM/kimi capture could
// outlive any number of upgrades and keep sending an outdated canonical prompt.
function staleIdentityModels(installed) {
  if (!installed || installed === "unknown") return [];
  const stale = [];
  try {
    for (const name of readdirSync(IDENTITY_DIR)) {
      const match = name.match(/^identity-(.+)\.json$/);
      if (!match) continue;
      try {
        const ident = JSON.parse(readFileSync(join(IDENTITY_DIR, name), "utf8"));
        // A truncated file is worse than a stale one: it can never be used,
        // and ignoring it here meant it was never re-captured either.
        if (!ident || typeof ident.system !== "string" ||
          !Array.isArray(ident.tools) || !ident.tools.length) {
          stale.push(match[1]);
          continue;
        }
        if (ident.version && ident.version !== installed) stale.push(match[1]);
      } catch {
        stale.push(match[1]);
      }
    }
  } catch {}
  return stale;
}

async function ensureIdentity() {
  identity = await loadIdentity();
  if (!identity) {
    await refreshIdentity("missing");
    if (!identity) throw new Error("no opencode identity available");
    return;
  }
  const installed = installedVersionCached();
  const stale = staleIdentityModels(installed);
  if (installed !== "unknown" && identity.version && installed !== identity.version) {
    log({ event: "identity_stale", have: identity.version, installed });
    // Refresh only the captures whose version is actually stale; an empty list
    // means capture.mjs decides (all known models), which is still bounded by
    // its per-model timeout.
    refreshIdentity("version_change", stale.length ? stale : undefined);
    return;
  }
  if (stale.length) {
    log({ event: "identity_stale_models", installed, models: stale.slice(0, 12) });
    refreshIdentity("model_version_change", stale);
  }
}

let lastIdentityAttempt = 0;
async function ensureIdentityForCredential(credentialOverride = "") {
  // Identity belongs to the account, not to an individual key. A configured
  // pool may rotate an expired key at any time; that must not trigger a full
  // recapture (which can take minutes and would stall the request).
  if (identityReady()) return;
  while (refreshing) await Bun.sleep(100);
  if (identityReady()) return;
  // If a previous capture failed, serve the old shape for a minute instead of
  // spawning a capture process on every request.
  if (identity && Date.now() - lastIdentityAttempt < 60_000) return;
  lastIdentityAttempt = Date.now();
  await refreshIdentity("missing_identity");
  while (refreshing) await Bun.sleep(100);
  if (identity) return;
  throw new Error("no OpenCode identity available for active credential");
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

// Session ids are the upstream's cache/routing affinity handle *and* the
// handle the free tier leases quota to. A restart that forgets them hands the
// next request a brand-new id, which the gate answers with
// FreeUsageLimitError even though the previous id is still allowed. Persist
// the mapping so a service restart reuses the established lease instead of
// burning it. BRIDGE_SESSION_ID pins one id (smoke tests, recovery); the store
// is capped and written atomically with 0600.
const SESSION_TTL_MS = Number(process.env.BRIDGE_SESSION_TTL_MS || 86_400_000);
const SESSION_STORE = process.env.BRIDGE_SESSION_STORE || join(IDENTITY_DIR, "sessions.json");
const SESSION_STORE_BASE = SESSION_STORE.slice(SESSION_STORE.lastIndexOf("/") + 1);
const SESSION_STORE_LIMIT = 512;
// Zen accepts exactly the stock opencode shape. The earlier loose regex also
// accepted hash-derived all-hex ids, which Zen rejects outright: an operator
// following the documented BRIDGE_SESSION_ID seed path could pin a dead id.
const SESSION_ID_RE = /^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/;
const SESSION_HEX_RE = /^ses_[0-9a-f]{26}$/;
const validSessionId = id => {
  const value = String(id || "");
  return SESSION_ID_RE.test(value) && !SESSION_HEX_RE.test(value);
};
const SESSION_OVERRIDE = (() => {
  const raw = String(process.env.BRIDGE_SESSION_ID || "").trim();
  if (!raw) return "";
  if (!validSessionId(raw)) {
    log({ event: "session_override_rejected", value: raw.slice(0, 40) });
    return "";
  }
  return raw;
})();
// A pool of recently accepted ids, not one immortal id. The free tier leases
// quota per id, so converging every conversation onto `lastGood` made that id
// exhaust first while the other established ids sat idle. New fallbacks now
// rotate through the pool least-recently-good first.
const LEASE_POOL_TTL_MS = Number(process.env.BRIDGE_LEASE_POOL_TTL_MS || 86_400_000);
const LEASE_POOL_LIMIT = Number(process.env.BRIDGE_LEASE_POOL_LIMIT || 16);
// Try only a couple of known-good ids before egress recovery. Walking all 16 on
// every FreeUsageLimitError produced bursts of identical 429s and delayed the
// IP reset; the upstream error does not tell us whether an id or the egress is
// exhausted. Operators may tune this explicitly for controlled diagnostics.
const LEASE_FALLBACK_ATTEMPTS = Number(
  process.env.BRIDGE_LEASE_FALLBACK_ATTEMPTS || Math.min(2, LEASE_POOL_LIMIT),
);
// A request shape that survived a successful reset and a fresh-id retry is not
// a quota problem. Remember it briefly so the phone does not cycle data once
// per turn for a shape the free-tier gate will always reject.
const SHAPE_FAILURE_TTL_MS = Number(process.env.BRIDGE_SHAPE_FAILURE_TTL_MS || 600_000);
const SHAPE_FAILURE_LIMIT = 32;
let lastGoodSession = SESSION_OVERRIDE;
let lastGoodAt = SESSION_OVERRIDE ? Date.now() : 0;
let lastGoodCred = "";
const leasePool = new Map(); // id -> {goodAt, cred}
const shapeFailures = new Map(); // shape signature -> failedAt
let sessionPersistTimer = null;

function writeFileDurable(path, body) {
  const fd = openSync(path, "w", 0o600);
  try {
    writeSync(fd, body);
    fsyncSync(fd);
  } finally {
    closeSync(fd);
  }
}

function fsyncDir(dir) {
  try {
    const fd = openSync(dir, "r");
    try { fsyncSync(fd); } finally { closeSync(fd); }
  } catch {}
}

function cleanupSessionTemps() {
  try {
    for (const name of readdirSync(IDENTITY_DIR)) {
      if (!name.startsWith(`${SESSION_STORE_BASE}.`) || !name.endsWith(".tmp")) continue;
      const path = join(IDENTITY_DIR, name);
      try {
        if (Date.now() - statSync(path).mtimeMs > 3_600_000) unlinkSync(path);
      } catch {}
    }
  } catch {}
}

function pruneLeasePool(now = Date.now()) {
  for (const [id, rec] of leasePool) {
    if (!validSessionId(id) || !rec.goodAt || now - rec.goodAt >= LEASE_POOL_TTL_MS) {
      leasePool.delete(id);
    }
  }
  if (leasePool.size > LEASE_POOL_LIMIT) {
    const keep = [...leasePool.entries()].sort((a, b) => b[1].goodAt - a[1].goodAt)
      .slice(0, LEASE_POOL_LIMIT);
    leasePool.clear();
    for (const [id, rec] of keep) leasePool.set(id, rec);
  }
}

function rememberLease(id, at = Date.now(), cred = leaseAccount()) {
  if (!validSessionId(id)) return;
  const prev = leasePool.get(id);
  leasePool.set(id, { goodAt: Math.max(at, prev?.goodAt || 0), cred });
  pruneLeasePool();
}

// A rejected id must never be handed back by the scope mapping or the pool.
function forgetLease(id) {
  leasePool.delete(id);
  if (lastGoodSession === id) {
    lastGoodSession = "";
    lastGoodAt = 0;
    lastGoodCred = "";
  }
  for (const rec of sessions.values()) {
    if (rec.id === id) rec.at = 0;
  }
}

function nextLeaseCandidate(exclude) {
  const now = Date.now();
  const cred = leaseAccount();
  pruneLeasePool(now);
  const candidates = [];
  for (const [id, rec] of leasePool) {
    if (id === exclude) continue;
    if (rec.cred && rec.cred !== cred) continue;
    if (!rec.goodAt || now - rec.goodAt >= LEASE_POOL_TTL_MS) continue;
    candidates.push({ id, goodAt: rec.goodAt });
  }
  candidates.sort((a, b) => a.goodAt - b.goodAt);
  return candidates[0] || null;
}

function leaseShapeKey(meta, credentialOverride = "") {
  const shape = meta?.shape || {};
  const reset = readResetState();
  return sha(JSON.stringify({
    credential: leaseAccount(credentialOverride),
    // A shape failure describes the remaining budget of one key, not the
    // account. Without the key in the signature a key rotated in after a 429
    // inherits the exhausted key's cached result and is never attempted.
    key: credentialFingerprint(false, credentialOverride),
    resetAt: Number(reset.lastSuccessAt || 0),
    egress4: String(reset.lastIp4 || ""),
    egress6: String(reset.lastIp || ""),
    model: String(meta?.model || "").toLowerCase(),
    tools: shape.tools || 0,
    toolChars: shape.toolChars || 0,
    systemChars: shape.systemChars || 0,
    messages: shape.messages || 0,
    messageChars: shape.messageChars || 0,
    reasoningChars: shape.reasoningChars || 0,
  })).slice(0, 16);
}

function shapeFailureAt(key) {
  const at = Number(shapeFailures.get(key) || 0);
  if (!at) return 0;
  if (Date.now() - at >= SHAPE_FAILURE_TTL_MS) {
    shapeFailures.delete(key);
    return 0;
  }
  return at;
}

function rememberShapeFailure(key) {
  shapeFailures.set(key, Date.now());
  if (shapeFailures.size > SHAPE_FAILURE_LIMIT) {
    const oldest = [...shapeFailures.entries()].sort((a, b) => a[1] - b[1])[0];
    if (oldest) shapeFailures.delete(oldest[0]);
  }
}

function loadSessions() {
  if (SESSION_OVERRIDE) return;
  let raw;
  try {
    raw = JSON.parse(readFileSync(SESSION_STORE, "utf8"));
  } catch (error) {
    // ENOENT is the first run; anything else is a corrupt/unreadable lease
    // record and must be visible, not silently replaced.
    if (error?.code !== "ENOENT") {
      log({ event: "session_store_load_failed", error: String(error) });
    }
    return;
  }
  const cred = leaseAccount();
  const savedAt = Number(raw?.savedAt || 0);
  if (validSessionId(raw?.lastGood)) {
    const at = Number(raw?.lastGoodAt || savedAt || Date.now());
    if (Number.isFinite(at) && at > 0 && Date.now() - at < LEASE_POOL_TTL_MS) {
      lastGoodSession = raw.lastGood;
      lastGoodAt = at;
      lastGoodCred = typeof raw?.credential === "string" ? raw.credential : "";
    }
  }
  const entries = Object.entries(raw?.sessions || {})
    .filter(([scope, rec]) => typeof scope === "string" && scope &&
      validSessionId(rec?.id) && Number.isFinite(rec?.at))
    .sort((a, b) => b[1].at - a[1].at)
    .slice(0, SESSION_STORE_LIMIT);
  for (const [scope, rec] of entries) {
    sessions.set(scope, {
      id: rec.id,
      at: rec.at,
      persistedAt: rec.at,
      goodAt: Number(rec.goodAt || 0),
      cred: typeof rec.cred === "string" ? rec.cred : cred,
    });
  }
  // v2 stores an explicit pool. v1 only has lastGood; seed it with the store's
  // savedAt so it ages out instead of living forever.
  const savedPool = Array.isArray(raw?.pool) ? raw.pool : [];
  for (const item of savedPool) {
    if (!validSessionId(item?.id)) continue;
    const at = Number(item?.goodAt || 0);
    if (!Number.isFinite(at) || at <= 0 || Date.now() - at >= LEASE_POOL_TTL_MS) continue;
    const itemCred = typeof item.cred === "string" ? item.cred : "";
    if (itemCred && itemCred !== cred) continue;
    leasePool.set(item.id, { goodAt: at, cred: itemCred || cred });
  }
  if (lastGoodSession && (!lastGoodCred || lastGoodCred === cred)) {
    const at = lastGoodAt || savedAt || Date.now();
    if (!leasePool.has(lastGoodSession)) {
      leasePool.set(lastGoodSession, { goodAt: at, cred: cred });
    }
  }
  // A scope record with goodAt is independent proof of acceptance; use it to
  // seed the pool on the first v2 run.
  for (const rec of sessions.values()) {
    if (!rec.goodAt || rec.goodAt <= 0 || Date.now() - rec.goodAt >= LEASE_POOL_TTL_MS) continue;
    if (rec.cred && rec.cred !== cred) continue;
    const prev = leasePool.get(rec.id);
    if (!prev || prev.goodAt < rec.goodAt) {
      leasePool.set(rec.id, { goodAt: rec.goodAt, cred: rec.cred || cred });
    }
  }
  pruneLeasePool();
  if (entries.length || leasePool.size) {
    log({ event: "session_store_loaded", sessions: entries.length, pool: leasePool.size });
  }
}

function persistSessions() {
  if (SESSION_OVERRIDE) return;
  try {
    mkdirSync(IDENTITY_DIR, { recursive: true, mode: 0o700 });
    try { chmodSync(IDENTITY_DIR, 0o700); } catch {}
    cleanupSessionTemps();
    const entries = [...sessions.entries()]
      .filter(([, rec]) => rec && validSessionId(rec.id))
      .sort((a, b) => b[1].at - a[1].at)
      .slice(0, SESSION_STORE_LIMIT);
    pruneLeasePool();
    const body = JSON.stringify({
      version: 2,
      savedAt: Date.now(),
      lastGood: validSessionId(lastGoodSession) ? lastGoodSession : undefined,
      lastGoodAt: lastGoodAt || undefined,
      credential: leaseAccount(),
      pool: [...leasePool.entries()].map(([id, rec]) => ({
        id, goodAt: rec.goodAt, cred: rec.cred,
      })),
      sessions: Object.fromEntries(entries.map(([scope, rec]) => [scope, {
        id: rec.id,
        at: rec.at,
        goodAt: rec.goodAt || undefined,
        cred: rec.cred || undefined,
      }])),
    });
    const tmp = `${SESSION_STORE}.${process.pid}.tmp`;
    writeFileDurable(tmp, body);
    try { chmodSync(tmp, 0o600); } catch {}
    renameSync(tmp, SESSION_STORE);
    try { chmodSync(SESSION_STORE, 0o600); } catch {}
    fsyncDir(IDENTITY_DIR);
  } catch (error) {
    log({ event: "session_store_write_failed", error: String(error) });
  }
}

function scheduleSessionPersist() {
  if (sessionPersistTimer) return;
  sessionPersistTimer = setTimeout(() => {
    sessionPersistTimer = null;
    persistSessions();
  }, 200);
  sessionPersistTimer.unref?.();
}

function sessionFor(scope) {
  if (SESSION_OVERRIDE) return SESSION_OVERRIDE;
  const now = Date.now();
  const hit = sessions.get(scope);
  if (hit && now - hit.at < SESSION_TTL_MS) {
    // Sliding TTL: an active conversation must keep its affinity handle even
    // when it runs longer than the window. Refreshing only on creation
    // rotated the session id mid-conversation and paid a cold cache (and, on
    // the free tier, a fresh quota lease).
    hit.at = now;
    if (now - (hit.persistedAt || 0) > 300_000) {
      hit.persistedAt = now;
      scheduleSessionPersist();
    }
    return hit.id;
  }
  const id = newSessionId();
  if (sessions.size >= SESSION_STORE_LIMIT) {
    let oldestScope = "";
    let oldestAt = Infinity;
    for (const [key, rec] of sessions) {
      if (rec.at < oldestAt) { oldestAt = rec.at; oldestScope = key; }
    }
    if (oldestScope) sessions.delete(oldestScope);
  }
  sessions.set(scope, { id, at: now, persistedAt: now, goodAt: 0, cred: leaseAccount() });
  scheduleSessionPersist();
  return id;
}

// markGoodSession records that the upstream accepted this id. The mapping for
// the scope is rewritten too: a fallback request must not keep generating a
// fresh, unleased id on every turn.
function markGoodSession(scope, id) {
  if (!validSessionId(id)) return;
  const now = Date.now();
  const cred = leaseAccount();
  lastGoodSession = id;
  lastGoodAt = now;
  lastGoodCred = cred;
  rememberLease(id, now, cred);
  if (SESSION_OVERRIDE) return;
  const rec = sessions.get(scope);
  if (rec && rec.id !== id) {
    rec.id = id;
    rec.at = now;
    rec.persistedAt = 0;
    rec.goodAt = now;
    rec.cred = cred;
  } else if (rec) {
    rec.at = now;
    rec.goodAt = now;
    rec.cred = cred;
  } else {
    sessions.set(scope, { id, at: now, persistedAt: 0, goodAt: now, cred });
  }
  scheduleSessionPersist();
}

loadSessions();
process.on("exit", () => { try { persistSessions(); } catch {} });
for (const signal of ["SIGTERM", "SIGINT"]) {
  process.on(signal, () => {
    try { persistSessions(); } catch {}
    process.exit(0);
  });
}

function traceIds() {
  const hex = n => Array.from(randomBytes(n), v => v.toString(16).padStart(2, "0")).join("");
  const t = hex(16), s = hex(8);
  return { b3: `${t}-${s}-1-${hex(8)}`, traceparent: `00-${t}-${s}-01` };
}

function baseHeaders(modelHeaders) {
  const ident = modelHeaders || identity?.headers || {};
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

function leaseFor(inbound, credentialOverride = "") {
  // Scope by account, never by the individual key: rotating an expired key
  // must not pin a new account lane or discard the leased session.
  const scope = sha(
    headerValue(inbound, "authorization") + "|" + headerValue(inbound, "x-opencode-session") +
    "|" + headerValue(inbound, "x-session-id") + "|" + leaseAccount(credentialOverride));
  return { scope, id: sessionFor(scope) };
}

function upstreamHeaders(modelName, sessionId, credentialOverride = "") {
  const modelIdent = getModelIdentity(modelName);
  const headers = new Headers({
    ...baseHeaders(modelIdent?.headers),
    "accept-encoding": "gzip, deflate, br, zstd",
    "x-opencode-session": sessionId,
    "x-opencode-session-id": sessionId,
    "x-session-affinity": sessionId,
    "x-session-id": sessionId,
    ...traceIds(),
  });
  if (credentialOverride) {
    const fp = credentialFingerprint(false, credentialOverride);
    if (foreignCredentials.has(fp)) {
      headers.delete("x-opencode-org-id");
      headers.delete("x-opencode-workspace-id");
    }
  }
  const key = liveCredential(false, credentialOverride);
  if (key) headers.set("authorization", `Bearer ${key}`);
  return headers;
}

// ---------------------------------------------------------------------- body
const toolName = tool => tool?.function?.name || tool?.name || "";

// Canonical tool names and their Claude Code equivalents. Used only to avoid
// paying a free-tier slot twice for one capability: the proxy translates the
// canonical call back to the client's tool, including argument keys.
const CLIENT_TO_CANONICAL = {
  bash: "shell", run_command: "shell", exec_command: "shell", shell_command: "shell",
  read: "read", read_file: "read", readfile: "read", view_file: "read",
  write: "write", write_file: "write", writefile: "write", create_file: "write",
  edit: "edit", edit_file: "edit", editfile: "edit", str_replace: "edit",
  glob: "glob", list_files: "glob", find_files: "glob",
  grep: "grep", search_files: "grep", ripgrep: "grep",
  webfetch: "webfetch", fetch: "webfetch",
  websearch: "websearch", search: "websearch", web_search: "websearch",
  skill: "skill", load_skill: "skill",
  agent: "subagent", task: "subagent", delegate: "subagent",
  askuserquestion: "question", ask_user: "question",
  execute: "execute", run_code: "execute", code_interpreter: "execute",
  image_generate: "experimental_image_generate",
  generate_image: "experimental_image_generate",
  imagegenerate: "experimental_image_generate",
  experimental_image_generate: "experimental_image_generate",
};
const canonicalToolKey = name =>
  CLIENT_TO_CANONICAL[String(name || "").toLowerCase()] || "";

// Canonical tools that the Go layer can still run through the client's shell
// when the client does not declare the structured equivalent.
const SHELL_FALLBACK_TOOLS = new Set([
  "read", "write", "edit", "grep", "glob", "webfetch", "websearch",
]);

// Reverse of CLIENT_TO_CANONICAL: every client name that can run a canonical
// tool. A canonical tool with no client equivalent and no shell fallback is a
// dead advertisement: the model may call it, but the Go layer has nothing to
// translate it to, so the call is dropped and the turn ends. The captured
// prompt advertises `execute` (Code Mode) even for clients that only have a
// shell, which is exactly how a turn ends after the model asks Code Mode to
// list models.
const CANONICAL_CLIENT_ALIASES = {};
for (const [client, canon] of Object.entries(CLIENT_TO_CANONICAL)) {
  (CANONICAL_CLIENT_ALIASES[canon] ||= []).push(client);
}

const shortToolName = name => {
  const s = String(name || "").trim();
  const i = s.lastIndexOf(".");
  return (i >= 0 && i + 1 < s.length ? s.slice(i + 1) : s).toLowerCase();
};

function clientCanRun(canonicalName, clientNames) {
  const key = shortToolName(canonicalName);
  if (clientNames.has(key)) return true;
  for (const alias of CANONICAL_CLIENT_ALIASES[key] || []) {
    if (clientNames.has(alias)) return true;
  }
  if (SHELL_FALLBACK_TOOLS.has(key)) {
    return clientNames.has("bash") || clientNames.has("shell") ||
      clientNames.has("exec_command") || clientNames.has("shell_command") ||
      clientNames.has("run_command");
  }
  return false;
}

// Tools that add capability the canonical set cannot provide. Kept first when
// the free-tier budget cannot hold every client tool.
const DEFAULT_CLIENT_TOOL_PRIORITY =
  "ToolSearch,DeferredToolPlaceholder,ReportFindings,ScheduleWakeup,Workflow,ListAgents";
const clientToolPriority = () =>
  (process.env.BRIDGE_CLIENT_TOOL_PRIORITY || DEFAULT_CLIENT_TOOL_PRIORITY)
    .split(",").map(s => s.trim()).filter(Boolean);

// The captured canonical prompt embeds the directory `capture.mjs` ran in. Every
// request therefore told the model it was working in the capture directory,
// while the client's own system block named the real one: two conflicting
// working directories in a single prompt. Rewrite the canonical <env> block
// from the client's environment instead of shipping the stale one.
const repoCache = new Map();

function gitRepoAt(cwd) {
  if (repoCache.has(cwd)) return repoCache.get(cwd);
  let isRepo = false;
  try { isRepo = existsSync(join(cwd, ".git")); } catch {}
  if (repoCache.size > 64) repoCache.clear();
  repoCache.set(cwd, isRepo);
  return isRepo;
}

// Best-effort working-directory discovery from the client's SYSTEM prompt.
// Paths may contain spaces, so take the rest of the line and strip wrapping
// quotes/backticks instead of stopping at the first space. Only system/
// developer blocks are trusted: a user message must not be able to move the
// agent's stated working directory.
function clientEnv(messages) {
  for (const m of messages) {
    if (m?.role !== "system" && m?.role !== "developer") continue;
    const text = typeof m?.content === "string" ? m.content
      : Array.isArray(m?.content) ? m.content.map(p => p?.text || "").join("\n") : "";
    if (!text) continue;
    for (const line of text.split("\n")) {
      const hit = line.match(
        /(?:Primary working directory|Current working directory|Working directory|Workspace root folder)\s*:\s*(.+)$/i);
      if (!hit) continue;
      const cwd = cleanCwd(hit[1]);
      if (cwd) return cwd;
    }
  }
  return "";
}

function cleanCwd(raw) {
  let s = String(raw || "").trim().replace(/^["'`]+|["'`]+$/g, "").trim();
  if (!s.startsWith("/")) return "";
  // A control character would break the single-line <env> substitution.
  if (s.length > 4096 || /[\u0000-\u001f\u007f]/.test(s)) return "";
  return s.replace(/\/+$/, "") || "/";
}

function applyEnv(system, cwd) {
  if (!REWRITE_ENV || !cwd || typeof system !== "string" || !system.includes("<env>")) return system;
  const repo = gitRepoAt(cwd) ? "yes" : "no";
  // Function replacers: a `$` in the path must never be read as a
  // replacement pattern ($&, $1, ...) and corrupt the prompt.
  return system.replace(/<env>[\s\S]*?<\/env>/, block => block
    .replace(/^(\s*Working directory:\s*).*$/m, (_, prefix) => prefix + cwd)
    .replace(/^(\s*Workspace root folder:\s*).*$/m, (_, prefix) => prefix + cwd)
    .replace(/^(\s*Is directory a git repo:\s*).*$/m, (_, prefix) => prefix + repo));
}

function normalize(raw) {
  const parsed = JSON.parse(raw);
  if (!Array.isArray(parsed.messages) || parsed.messages.length === 0)
    throw new Error("messages must be a non-empty array");
  if (!identity) throw new Error("no opencode identity captured yet");

  const messages = parsed.messages;
  const modelIdent = getModelIdentity(parsed.model) || identity;
  const modelClean = cleanModelName(parsed.model);
  if (modelClean) requestedModels.add(modelClean);
  else if (parsed.model) log({ event: "model_name_rejected", model: String(parsed.model).slice(0, 120) });
  // A model first used after an opencode upgrade still carries the prompt from
  // the old build; refresh in the background while this request uses the last
  // known good capture.
  const installed = installedVersionCached();
  if (modelIdent?.version && installed !== "unknown" && modelIdent.version !== installed) {
    refreshIdentity("requested_model_stale", modelClean ? [modelClean] : undefined);
  }
  let baseSystem = modelIdent.system;
  if (modelClean && !hasSpecificModelIdentity(parsed.model)) {
    // No capture exists for this model yet. The primary capture names the
    // primary model, and the upstream free tier fingerprints the prompt, so
    // at minimum rename the Model ID and kick off a targeted capture in the
    // background instead of serving a prompt that claims to be another model.
    baseSystem = baseSystem.replace(
      /^(\s*-\s*Model ID:\s*).*$/m, (_, prefix) => prefix + modelClean);
    refreshIdentity("new_model", [modelClean]);
  }
  const cwd = clientEnv(messages);
  const canonical = applyEnv(baseSystem.replace(
    /^Today's date: .*$/m, `Today's date: ${new Date().toDateString()}`), cwd);
  const clientSystem = messages.filter(m => m.role === "system");
  const rest = messages.filter(m => m.role !== "system");
  const isCanonical = m => typeof m.content === "string" &&
    (m.content === modelIdent.system || m.content === canonical || (identity && (m.content === identity.system)));
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
    if (!parsed.prompt_cache_options && !cacheOptionsRejected.has(modelClean)) {
      parsed.prompt_cache_options = { mode: "implicit" };
    }
  }

  // Canonical tools only, by default. Their names are part of the cached
  // prefix, and the free tier treats a different tool set as an unknown client
  // and answers FreeUsageLimitError. Sending a client's extras therefore costs
  // an extra upstream request per turn (the retry at the stock shape) and made
  // every turn 2-3x as expensive. Translation keeps the capability: a canonical
  // shell/read/skill call is rewritten to the client's Bash/Read/Skill.
  const canonicalTools = modelIdent.tools || identity.tools || [];
  const canonicalNames = new Set(canonicalTools.map(toolName));
  // A canonical tool the client cannot run is a trap: the model calls it, the
  // Go layer has no declared target, and the turn ends with a dropped-call
  // notice. Drop every dead entry, not just the trailing suffix: keeping a
  // dead tool in the middle (experimental_image_generate) leaves it callable
  // while the note below says it is unavailable. BRIDGE_KEEP_DEAD_TOOLS=1
  // restores the stock list if a gate check ever proves the pruned shape is
  // rejected.
  const clientToolNames = new Set(
    (parsed.tools || []).map(toolName).map(shortToolName).filter(Boolean));
  const runnableFlags = canonicalTools.map(t => clientCanRun(toolName(t), clientToolNames));
  const runnableCanonical = KEEP_DEAD_TOOLS ? canonicalTools
    : canonicalTools.filter((t, i) => runnableFlags[i]);
  // The note describes what the *client* cannot run, independent of the
  // kill switch. Computing it from the pruned list made BRIDGE_KEEP_DEAD_TOOLS
  // send the dead tools with no note at all, which is the one case the note
  // exists for.
  const unavailableCanonical = canonicalTools.filter((t, i) => !runnableFlags[i]);
  const prunedCanonical = KEEP_DEAD_TOOLS ? [] : unavailableCanonical;
  const runnableNames = new Set(runnableCanonical.map(toolName));
  if (unavailableCanonical.length) {
    log({
      event: "tools_unavailable_pruned", model: parsed.model,
      kept: runnableCanonical.length, pruned: prunedCanonical.map(toolName).slice(0, 8),
      unavailable: unavailableCanonical.map(toolName).slice(0, 8),
    });
    // The canonical prompt is the free-tier fingerprint and must stay intact,
    // but it advertises tools this client does not have (notably Code Mode's
    // `execute`). Say so once, immediately after it, so a model that reads the
    // prompt before the tool list does not call a tool that will be dropped.
    parsed.messages.splice(1, 0, {
      role: "system",
      content: "[opencode-cc] These tools are described above but are NOT available " +
        `in this client: ${unavailableCanonical.map(toolName).join(", ")}. ` +
        "Do not call them; use only the tools present in this request's tool list.",
    });
  }
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
    const keep = Math.max(0, MAX_TOOLS - runnableCanonical.length);
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
        keptSample: extraTools.map(toolName).slice(0, 8),
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
  // An explicit empty `tools: []` is rejected by several OpenAI-compatible
  // upstreams and advertises nothing anyway. A client with no runnable tools
  // (no tools at all, or only MCP tools this lane cannot forward) must send no
  // tools field, not an empty array.
  const finalTools = [...runnableCanonical, ...extraTools];
  if (finalTools.length) parsed.tools = finalTools;
  else delete parsed.tools;

  // A pinned tool_choice names a client tool ("Bash"), but the wire now carries
  // only canonical names ("shell"): the upstream sees a choice for a tool that
  // is not in its tool list. Rewrite it when the equivalent is on the wire.
  // When the named tool was pruned or suppressed, drop the choice: leaving it
  // on the wire names a function the upstream tool list does not contain.
  if (parsed.tool_choice && typeof parsed.tool_choice === "object") {
    const choice = parsed.tool_choice;
    const wanted = String(choice.function?.name || choice.name || "");
    const canon = canonicalToolKey(wanted);
    const wireNames = new Set(finalTools.map(toolName));
    if (canon && runnableNames.has(canon) && wanted !== canon) {
      if (choice.function) choice.function.name = canon;
      else choice.name = canon;
      log({ event: "tool_choice_rewritten", from: wanted, to: canon, model: parsed.model });
    } else if (!wireNames.has(wanted) && !(canon && wireNames.has(canon))) {
      delete parsed.tool_choice;
      log({ event: "tool_choice_dropped", from: wanted, model: parsed.model });
    }
  }

  // The free tier answers a non-streaming request with the same generic
  // FreeUsageLimitError it uses for unknown clients, even when quota is
  // available. Always ask upstream for SSE and rebuild a non-stream response
  // for callers that requested one.
  const wantsStream = parsed.stream === true;
  parsed.stream = true;
  parsed.stream_options = { ...(parsed.stream_options || {}), include_usage: true };

  // Request-time pruning: keep the shape stable but stop old tool output from
  // consuming the free window's remaining token budget. The client's stored
  // history is unchanged.
  const pruned = pruneOldToolResults(parsed.messages);
  if (pruned.pruned > 0) {
    log({ event: "context_pruned", results: pruned.pruned, chars: pruned.chars, model: parsed.model });
  }

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
  // told exactly what broke instead of getting a quota 429. Apply it to every
  // model this sidecar serves: the previous `/free/i` test missed aliases such
  // as big-pickle, which is exactly how an oversized extras surface bypassed
  // the guard.
  if (shape.toolChars > MAX_TOOL_CHARS) {
    log({ event: "shape_rejected", reason: "tool_surface_too_large", ...shape, maxToolChars: MAX_TOOL_CHARS });
    throw new Error(
      `tool surface too large: ${shape.toolChars} chars across ${shape.tools} tools ` +
      `exceeds the free-tier limit of ${MAX_TOOL_CHARS}; trim tools or set BRIDGE_MAX_TOOL_CHARS`);
  }

  return { body: JSON.stringify(parsed), wantsStream, shape };
}

// ------------------------------------------------------------------- routing
// Mark every sidecar-generated 5xx. The Go layer retries an upstream 5xx once
// for direct Zen traffic, but this sidecar has already applied its own bounded
// retry and deadline; retrying it there doubled the stall and the inflight
// slot. The header lets the Go layer treat it as final.
const json = (body, status = 200, headers = undefined) => {
  const out = new Headers(headers);
  if (status >= 500) out.set("x-oc-native", "1");
  return Response.json(body, { status, headers: out });
};

// relayText rebuilds a response whose body was already read and decoded by
// fetch. Content-Encoding/Content-Length describe the wire bytes, not this
// decoded string, so forwarding them would make the caller decode twice or
// wait for bytes that no longer exist.
function relayText(text, response) {
  const headers = new Headers(response.headers);
  headers.delete("content-encoding");
  headers.delete("content-length");
  return new Response(text, { status: response.status, headers });
}

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
const MAX_AGGREGATE_BYTES = Number(process.env.BRIDGE_MAX_AGGREGATE_BYTES || 16 * 1024 * 1024);
const MAX_TOOL_INDEX = 32;

// A non-streaming response is buffered whole. Cap it: an upstream that streams
// for hours must not grow the single-threaded sidecar's heap without bound.
async function readTextCapped(response, cap) {
  if (!response.body) return { text: await response.text(), tooLarge: false };
  const reader = response.body.getReader();
  const chunks = [];
  let total = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value?.byteLength ?? 0;
      if (total > cap) {
        try { await reader.cancel(); } catch {}
        return { text: "", tooLarge: true };
      }
      chunks.push(Buffer.from(value));
    }
  } finally {
    try { reader.releaseLock(); } catch {}
  }
  return { text: Buffer.concat(chunks).toString("utf8"), tooLarge: false };
}

async function aggregate(response, meta) {
  const buffered = await readTextCapped(response, MAX_AGGREGATE_BYTES);
  if (buffered.tooLarge) {
    log({ event: "upstream_aggregate_too_large", ...meta, cap: MAX_AGGREGATE_BYTES });
    return json({
      error: { type: "upstream_too_large", message: "upstream response exceeded the aggregate cap" },
    }, 502);
  }
  const text = buffered.text;
  let id = null, created = null, model = null, role = "assistant", finish = null, usage = null;
  let content = "", reasoning = "";
  let done = false;
  const calls = new Map();
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
      // The upstream is trusted, but a bad index must not create a sparse
      // array (1e9 entries stalled the event loop) or resolve `__proto__`.
      // An absent/null index means the single call 0; a present but invalid
      // index is a different call we cannot place. Clamping it to 0 merged it
      // into call 0, producing `shellshell` and concatenated JSON arguments.
      const rawIdx = tc?.index;
      if (rawIdx !== undefined && rawIdx !== null &&
          !(Number.isInteger(rawIdx) && rawIdx >= 0 && rawIdx < MAX_TOOL_INDEX)) {
        log({ event: "tool_index_rejected", ...meta, index: String(rawIdx).slice(0, 40) });
        continue;
      }
      const idx = rawIdx ?? 0;
      let cur = calls.get(idx);
      if (!cur) {
        cur = { id: "", type: "function", function: { name: "", arguments: "" } };
        calls.set(idx, cur);
      }
      if (tc.id) cur.id = tc.id;
      if (tc.type) cur.type = tc.type;
      if (tc.function?.name) cur.function.name += tc.function.name;
      if (tc.function?.arguments) cur.function.arguments += tc.function.arguments;
    }
  }
  const message = { role, content: content || null };
  if (reasoning) message.reasoning_content = reasoning;
  const toolCalls = [...calls.entries()].sort((a, b) => a[0] - b[0]).map(([, value]) => value);
  if (toolCalls.length) message.tool_calls = toolCalls;
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
let resetInFlight = null;

function readResetState() {
  try { return JSON.parse(readFileSync(RESET_STATE, "utf8")); } catch { return {}; }
}

// A reset that finished inside the cooldown window still covers this request:
// the engine would only answer "skip: a reset succeeded Ns ago". Retrying the
// upstream call is then the right move and costs no radio cycle.
function recentReset() {
  const at = Number(readResetState().lastSuccessAt || 0);
  if (!at) return null;
  const ageMs = Date.now() - at;
  if (ageMs < 0 || ageMs >= RESET_COOLDOWN_MS) return null;
  return { ok: true, how: "recent-success", ms: 0, ageMs };
}

function resetResult(code, output, started) {
  const text = String(output || "");
  const ms = Date.now() - started;
  if (code === 0) {
    const via = text.match(/\bok via ([^\s:]+)/);
    if (via) return { ok: true, how: via[1], ms };
    if (/skip: a reset succeeded \d+s ago/.test(text)) return { ok: true, how: "recent-success", ms };
    // Older engine revisions exited 0 for these no-op paths; keep reading them.
    if (/dry run:/.test(text)) return { ok: false, how: "dry-run", ms };
    if (/disabled: .* exists/.test(text)) return { ok: false, how: "disabled", ms };
    if (/no usable telegram config/.test(text)) return { ok: false, how: "no-config", ms };
  }
  if (code === 2) return { ok: false, how: "busy", ms };
  if (code === 6) return { ok: false, how: "disabled", ms };
  if (code === 7) return { ok: false, how: "dry-run", ms };
  if (/FAILED:/.test(text)) return { ok: false, how: "reset-failed", ms };
  if (/backing off for/.test(text)) return { ok: false, how: "backoff", ms };
  if (code === 3) return { ok: false, how: "refused", ms };
  if (code === 5) return { ok: false, how: "no-config", ms };
  return { ok: false, how: `exit-${code}`, ms };
}

async function spawnReset(meta) {
  const started = Date.now();
  log({ event: "quota_reset_start", ...meta, source: RESET_SOURCE });
  let proc;
  try {
    proc = Bun.spawn([RESET_NODE, RESET_SCRIPT, `--source=${RESET_SOURCE}`], {
      cwd: HOME,
      env: {
        ...process.env,
        RESET_ALLOW_AUTOMATIC: "1",
        RESET_COOLDOWN_MS: String(RESET_COOLDOWN_MS),
      },
      stdout: "pipe",
      stderr: "pipe",
      // The engine spawns adb children and toggles the radio. A wrapper-only
      // kill can leave an orphan (or data off), so run it in its own group and
      // signal the group on the backstop timeout.
      detached: true,
    });
  } catch (error) {
    log({ event: "quota_reset_spawn_failed", ...meta, error: String(error) });
    return { ok: false, how: "spawn-failed", ms: Date.now() - started };
  }
  const killReset = signal => {
    try { process.kill(-proc.pid, signal); }
    catch { try { proc.kill(signal); } catch {} }
  };
  const timer = setTimeout(() => killReset("SIGKILL"), RESET_TIMEOUT_MS);
  try {
    const [out, err, code] = await Promise.all([
      new Response(proc.stdout).text(),
      new Response(proc.stderr).text(),
      proc.exited,
    ]);
    const text = `${out}${err}`;
    const result = resetResult(code, text, started);
    log({
      event: "quota_reset_done", ...meta, ...result, code,
      tail: text.trim().split("\n").slice(-3).join(" | ").slice(-500),
    });
    return result;
  } finally {
    clearTimeout(timer);
  }
}

function resetLockOwnerAlive() {
  try {
    const pid = Number(readFileSync(RESET_LOCK, "utf8").trim().split(/\s+/)[0]);
    if (!pid) return false;
    process.kill(pid, 0);
    return true;
  } catch (error) {
    // EPERM means the process exists but belongs to another user.
    return error?.code === "EPERM";
  }
}

function resetLockStale() {
  try {
    if (!resetLockOwnerAlive()) return true;
    return Date.now() - statSync(RESET_LOCK).mtimeMs > RESET_LOCK_STALE_MS;
  } catch {
    return false;
  }
}

async function waitForLock() {
  const deadline = Date.now() + RESET_SHARED_WAIT_MS;
  while (Date.now() < deadline) {
    if (!existsSync(RESET_LOCK)) return true;
    if (resetLockStale()) {
      // Do not block recovery for the full wait on a dead engine. The reset
      // script reclaims stale locks itself; returning lets the caller spawn it.
      log({ event: "quota_reset_lock_stale" });
      return true;
    }
    await Bun.sleep(1000);
  }
  return !existsSync(RESET_LOCK);
}

// One reset per incident across all concurrent callers: a sibling request that
// already asked for a reset joins its promise instead of bouncing the radio a
// second time. Waiting for a foreign lock avoids surfacing 429 while the
// OpenCode plugin is still rotating the same egress.
async function recoverQuota(meta) {
  if (resetInFlight) {
    log({ event: "quota_reset_join", ...meta });
    return await resetInFlight;
  }
  const covered = recentReset();
  if (covered) {
    log({ event: "quota_reset_covered", ...meta, ageMs: covered.ageMs });
    return covered;
  }
  // Capture the pre-spawn state: a concurrent reset may publish its success
  // before our own exit-2 child is even reaped, and comparing after-the-fact
  // against its own write would misread that as "nothing changed".
  const beforeSuccess = Number(readResetState().lastSuccessAt || 0);
  const run = (async () => {
    let result = await spawnReset(meta);
    if (result.how !== "busy") return result;
    const cleared = await waitForLock();
    // waitForLock can time out on the same tick the lock becomes stale.
    // Re-check before giving up so recovery does not depend on that boundary.
    const reclaimable = cleared || !existsSync(RESET_LOCK) || resetLockStale();
    if (reclaimable) {
      // The engine's own reclamation should clear the dead lock, but retry
      // once so recovery does not depend on that internal detail.
      result = await spawnReset(meta);
      if (result.how !== "busy") return result;
    }
    const after = Number(readResetState().lastSuccessAt || 0);
    const fresh = after > beforeSuccess || (after > 0 && Date.now() - after < RESET_COOLDOWN_MS);
    result = reclaimable && fresh
      ? { ok: true, how: "shared", ms: 0 }
      : { ok: false, how: reclaimable ? "shared-failed" : "shared-timeout", ms: 0 };
    log({ event: "quota_reset_shared", ...meta, ...result });
    return result;
  })().finally(() => { resetInFlight = null; });
  resetInFlight = run;
  return await run;
}

function passThrough429(response, text, reset) {
  const headers = new Headers({
    "content-type": response.headers.get("content-type") || "application/json",
  });
  // The free tier leases quota to the session id, not to the IP. A 429 from a
  // new conversation usually means that id has no lease yet; the fallback in
  // upstream() retries with the last accepted id before any egress rotation.
  // Do not store or pass through long upstream retry-after timestamps (e.g.
  // 16h/24h) so local clients do not lock out or calculate stale reset times.
  headers.set("x-oc-native", "1");
  headers.set("x-oc-reset", reset);
  headers.set("x-oc-reset-source", RESET_SOURCE);
  return new Response(text, { status: 429, headers });
}

// ---------------------------------------------------------------- upstream
// A non-quota 429 is surfaced untouched. A FreeUsageLimitError from a new
// conversation walks a ladder before giving up:
//   1. other recently accepted ids from the lease pool (least-recently-good
//      first, so new conversations do not all converge on one id);
//   2. the shared egress reset, once;
//   3. one freshly minted id after the reset (a reset that rotated the IP may
//      make a new lease obtainable while the old id is still exhausted);
//   4. surface the 429 with `x-oc-reset: shape`, and remember the request
//      shape briefly so the radio is not cycled again for a shape the gate
//      will always reject.
async function upstream(body, inbound, meta, lease) {
  const credentialOverride = inboundCredential(inbound);
  let transportFailures = 0;
  let resetDone = false;
  let freshAfterResetDone = false;
  let leaseFallbacks = 0;
  let postResetGrace = false;
  let accountRecaptureDone = false;
  let cacheRepairDone = false;
  let last429 = null;
  let sessionId = lease.id;
  const totalDeadline = Number(meta.started || Date.now()) + HEADER_TIMEOUT + TRANSPORT_RETRY_BUDGET_MS;
  for (let attempt = 1; ; attempt++) {
    await paceStart();
    const remaining = totalDeadline - Date.now();
    if (remaining <= 0 && !postResetGrace) {
      // A request that already has a 429 must not turn its expired budget into
      // a 502 after a reset. Surface the last quota response so the caller can
      // retry immediately instead of seeing a fake transport failure.
      if (last429) {
        log({ event: "upstream_deadline", ...meta, attempt, resetDone });
        return passThrough429(last429.response, last429.text, "failed");
      }
      throw new Error("upstream request deadline exceeded");
    }
    const attemptBudget = remaining > 0 ? remaining : POST_RESET_GRACE_MS;
    postResetGrace = false;
    const controller = new AbortController();
    // The first attempt gets the full header deadline; retries share what is
    // left of the budget. A header timeout is never retried (see catch).
    const timer = setTimeout(() => controller.abort(), Math.min(HEADER_TIMEOUT, attemptBudget));
    let response;
    try {
      response = await fetch(identity.endpoint, {
        method: "POST",
        headers: upstreamHeaders(meta?.model, sessionId, credentialOverride),
        body,
        signal: AbortSignal.any([inbound.signal, controller.signal]),
      });
    } catch (error) {
      if (inbound.signal.aborted) throw error;
      // A header timeout is not a reset-induced socket failure. Retrying it
      // tripled the stall and let eight requests pin every slot for ~12 min.
      if (controller.signal.aborted) throw new Error("upstream header timeout");
      // A network reset changes the egress while a request is in flight, so the
      // next attempt can die on a stale socket. Retry instead of surfacing 502.
      if (transportFailures >= 2) throw error;
      transportFailures++;
      log({ event: "transport_retry", ...meta, attempt, error: String(error) });
      await Bun.sleep(transportFailures === 1 ? 750 : 2000);
      continue;
    } finally {
      clearTimeout(timer);
    }
    // Only an SSE body can be rebuilt into a completion. An upstream error
    // body (400/500 JSON) must pass through with its real status, or the
    // caller loses the provider's message behind a fake "truncated stream".
    // A pooled key can belong to a different employer account than the one
    // whose request shape was captured. The bearer key alone selects the
    // workspace; this account's org header and leased session are what trigger
    // "Workspace access denied". Learn the key as foreign, drop those headers,
    // move it to its own lease lane, and retry once.
    if (response.status === 403 && credentialOverride) {
      const fp = credentialFingerprint(false, credentialOverride);
      const text403 = await response.text();
      if (/workspace|access denied|wrong workspace/i.test(text403)) {
        if (!foreignCredentials.has(fp)) {
          foreignCredentials.add(fp);
          sessionId = leaseFor(inbound, credentialOverride).id;
          log({ event: "credential_account_mismatch", ...meta, attempt, body: text403.slice(0, 200) });
          continue;
        }
      }
      return relayText(text403, response);
    }
    // Account mismatch is the one identity failure recovery can repair: the
    // active account changed while the bridge kept running. Recapture once and
    // retry, instead of polling the credential on every request.
    if (response.status === 403 && !accountRecaptureDone) {
      const text = await response.text();
      if (/workspace|access denied|wrong workspace|forbidden/i.test(text)) {
        accountRecaptureDone = true;
        log({ event: "identity_account_mismatch", ...meta, attempt, body: text.slice(0, 200) });
        await refreshIdentity("account_mismatch");
        postResetGrace = true;
        continue;
      }
      return relayText(text, response);
    }
    // A model that rejects the prompt-cache field answers 400 before any
    // generation. Repair the shape once and replay: this is a capability
    // difference, not a bad request from the client.
    if (response.status === 400 && !cacheRepairDone) {
      const text = await response.text();
      const repaired = /prompt_cache_(options|retention|key)/i.test(text)
        ? stripPromptCacheFields(body, meta?.model)
        : null;
      if (repaired) {
        cacheRepairDone = true;
        body = repaired;
        log({ event: "cache_options_unsupported", ...meta, attempt, body: text.slice(0, 200) });
        continue;
      }
      return relayText(text, response);
    }
    if (response.status !== 429) {
      if (response.ok) markGoodSession(lease.scope, sessionId);
      // Logged after the lease is recorded: an accepted fresh id is leased
      // from this moment on, and calling it "new" sent the last diagnosis
      // looking for a rotation that had not happened.
      log({
        event: "inference", ...meta, attempt, status: response.status, ms: Date.now() - meta.started,
        session: sessionId === lastGoodSession ? "leased" : "new",
      });
      if (meta.wantsStream) return tap(response, meta);
      const type = response.headers.get("content-type") || "";
      if (!response.ok || !type.includes("event-stream")) return tap(response, meta);
      return aggregate(response, meta);
    }

    const text = await response.text();
    last429 = { response, text };
    const freeLimit = /free usage limit/i.test(text) || /FreeUsageLimitError/.test(text);
    log({ event: "upstream_429", ...meta, attempt, exhausted: freeLimit, body: text.slice(0, 400) });
    if (!freeLimit || inbound.signal.aborted) {
      return passThrough429(response, text, "manual");
    }

    // The generic error cannot distinguish quota from request shape. Scope a
    // cached failure to the active account and last successful egress reset so
    // an account switch or a new IP does not inherit another lane's failure.
    const shapeKey = leaseShapeKey(meta, credentialOverride);
    const blockedAt = shapeFailureAt(shapeKey);
    if (blockedAt) {
      log({ event: "quota_reset_skipped_shape", ...meta, attempt, ageMs: Date.now() - blockedAt });
      return passThrough429(response, text, "shape");
    }

    // 1. Another established id from the pool.
    if (leaseFallbacks < LEASE_FALLBACK_ATTEMPTS) {
      forgetLease(sessionId);
      const next = nextLeaseCandidate(sessionId);
      if (next) {
        leaseFallbacks++;
        log({
          event: "session_lease_fallback", ...meta, attempt,
          from: sessionId, to: next.id, pool: leasePool.size,
        });
        sessionId = next.id;
        continue;
      }
    }

    // 1b. Go is rotating API keys for this request, so the egress has already
    // been reset (or deliberately skipped) for this attempt. Cycling the radio
    // once per key would leave the phone offline for minutes and still return
    // 429s. Surface now and let the caller try the next key.
    if (headerValue(inbound, "x-bridge-no-reset") === "1") {
      log({ event: "quota_reset_skipped_failover", ...meta, attempt });
      return passThrough429(response, text, "skipped");
    }

    // 2. Rotate the egress once. The reset engine can legitimately run for
    // RESET_TIMEOUT_MS (600s), far longer than this request's budget; wait only
    // for the remaining budget so a hung reset cannot pin every inflight slot.
    if (!resetDone) {
      forgetLease(sessionId);
      resetDone = true;
      const resetRemaining = totalDeadline - Date.now();
      if (resetRemaining <= 0) {
        log({ event: "quota_reset_skipped_deadline", ...meta, attempt });
        return passThrough429(response, text, "failed");
      }
      let resetTimer;
      const deadline = new Promise(resolve => {
        resetTimer = setTimeout(
          () => resolve({ ok: false, how: "deadline" }),
          resetRemaining,
        );
      });
      let outcome;
      try {
        outcome = await Promise.race([recoverQuota({ ...meta, attempt }), deadline]);
      } finally {
        clearTimeout(resetTimer);
      }
      if (inbound.signal.aborted) return new Response(null, { status: 499 });
      if (outcome.ok) {
        log({ event: "quota_retry", ...meta, attempt, how: outcome.how });
        // Give the phone's new route a moment to settle before reusing the pool.
        await Bun.sleep(500);
        // The reset may have finished at the deadline. Allow exactly one
        // immediate attempt; otherwise the next loop check throws and the
        // caller sees a 502 even though the quota was recovered.
        postResetGrace = true;
        continue;
      }
      log({ event: "quota_reset_deadline", ...meta, attempt, how: outcome.how });
      return passThrough429(response, text, "failed");
    }

    // 3. The reset did not revive this id: mint one fresh id once.
    if (!freshAfterResetDone) {
      freshAfterResetDone = true;
      const fresh = newSessionId();
      log({ event: "session_fresh_after_reset", ...meta, attempt, from: sessionId, to: fresh });
      sessionId = fresh;
      continue;
    }

    // 4. Same shape cannot pass; stop cycling the radio for it.
    rememberShapeFailure(shapeKey);
    log({ event: "quota_shape_failed", ...meta, attempt, shape: meta.shape });
    return passThrough429(response, text, "shape");
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
        quota: {
          automaticRecovery: true,
          resetSource: RESET_SOURCE,
          cooldownMs: RESET_COOLDOWN_MS,
          maxRetriesPerRequest: 1,
        },
        identity: identity
          ? { version: identity.version, capturedAt: identity.capturedAt,
              systemLen: identity.system.length, tools: identity.tools.length }
          : null,
        lease: {
          sessions: sessions.size,
          pool: leasePool.size,
          lastGood: Boolean(lastGoodSession),
          lastGoodAgeMs: lastGoodAt ? Date.now() - lastGoodAt : null,
          override: Boolean(SESSION_OVERRIDE),
          store: SESSION_STORE,
          poolTtlMs: LEASE_POOL_TTL_MS,
          fallbackAttempts: LEASE_FALLBACK_ATTEMPTS,
        },
        installed: installedVersionCached(),
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

    // Reserve the slot before the first await. Checking the counter and then
    // incrementing it after `await request.text()` let every request in a burst
    // pass the check before any of them counted, blowing past MAX_INFLIGHT.
    let model = "-";
    const started = Date.now();
    inflight++;
    try {
      await ensureIdentityForCredential(inboundCredential(request));
      let normalized;
      try {
        normalized = normalize(await request.text());
      } catch (error) {
        return json({ error: { type: "invalid_request_error", message: String(error?.message || error) } }, 400);
      }
      const body = normalized.body;
      // Journal lines carry the model; a hostile 32 MiB model string must not
      // become a 32 MiB log line. The body keeps the real value.
      try { model = String(JSON.parse(body).model || "-").slice(0, 200); } catch {}

      if (TRACE_DIR) {
        try {
          await mkdir(TRACE_DIR, { recursive: true, mode: 0o700 });
          // mkdir's mode only applies on creation; repair a pre-existing
          // world-readable directory from an older build.
          try { await chmod(TRACE_DIR, 0o700); } catch {}
          const tracePath = join(TRACE_DIR, `${Date.now()}-${String(model).replace(/[^\w.-]+/g, "_")}.json`);
          // Trace bodies carry prompts, tool output and file contents; never
          // leave them world-readable.
          await writeFile(tracePath, body, { mode: 0o600 });
          try { await chmod(tracePath, 0o600); } catch {}
          log({ event: "trace", model, bytes: body.length, shape: normalized.shape });
        } catch (error) { log({ event: "trace_failed", error: String(error) }); }
      }

      return await upstream(body, request, {
        model, started, bytes: body.length, wantsStream: normalized.wantsStream, shape: normalized.shape,
      }, leaseFor(request, inboundCredential(request)));
    } catch (error) {
      if (request.signal.aborted) return new Response(null, { status: 499 });
      log({ event: "transport_failure", model, ms: Date.now() - started, error: String(error) });
      return json({ error: { type: "server_error", message: "upstream transport failed" } }, 502);
    } finally {
      inflight--;
    }
  },
});

// Load the last known capture before announcing the port: readiness must mean
// "can answer", not "process started". The slow path (running capture.mjs) is
// detached below, because re-capturing every model can take minutes and the
// supervisor kills a sidecar that has not announced its port within 30s.
identity = await loadIdentity();
log({
  event: "listening", port: server.port, host: HOST, identity: identity?.version ?? null,
  systemLen: identity?.system?.length ?? 0, tools: identity?.tools?.length ?? 0,
});
ensureIdentity().catch(error => log({ event: "identity_error", error: String(error) }));

// Re-capture when the client upgrades.
setInterval(() => {
  const installed = installedVersionCached();
  const stale = staleIdentityModels(installed).filter(m => requestedModels.has(m));
  if (!identity || (installed !== "unknown" && installed !== identity.version) || stale.length) {
    log({ event: "identity_watch", installed, have: identity?.version ?? null, staleModels: stale.slice(0, 12) });
    refreshIdentity("watch", stale.length ? stale : undefined);
  }
}, 60 * 60 * 1000);
