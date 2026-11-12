# Native free-tier transport

This documents the free-model lane in `opencode-cc` as of November 2026. It
replaces the separate DS Flash helper service and the fixed `:18790` listener.

## Why a Bun hop exists at all

Zen's free tier gates requests on client identity, not only on the request body.
Measured on 2026-11-08 with an identical body, key and headers: a request
carrying an established session id returned `200`, while the same request with
a fresh id returned `429 FreeUsageLimitError`. The free tier leases quota to a
session id. The earlier 2026-11-01 measurement (Bun `200`; Go, Node, Python and
curl `429`) was consistent with this: each non-Bun replay used a fresh id, so
it was denied regardless of its TLS stack.

A byte-for-byte replay that Bun completes successfully is rejected from Go when
it carries a fresh id, so the transport cannot be ported into the Go proxy
without also reproducing the session lease. `opencode-cc` therefore keeps
exactly one Bun hop and hides it: it starts the process itself on an ephemeral
loopback port, passes it an admission token, restarts it on exit, and stops it
on shutdown. There is no fixed port and no second service to manage.

## Shape of the deployment

```text
OpenCode CLI / T3 OpenCode ──────────────► https://opencode.ai  (direct, no CPA)

Claude Code / T3 Claude ─┐
Codex ───────────────────┤
Antigravity ─────────────┼──► CLIProxyAPI :8317 ──► opencode-cc :8787 ──┬──► Zen (paid)
Local LFM3 ──────────────┘                                             └──► Bun sidecar (free)
                                                                            ephemeral 127.0.0.1 port
```

- OpenCode never references CPA. Its provider config must stay direct.
- CPA is the single front door for Claude, Codex, Antigravity and local LFM3.
- CPA has one `opencode-cc` Claude provider. Both free models are aliases on
  that provider; do not add a second provider for the same upstream, or CPA
  round-robins between them unpredictably.
- `opencode-cc` decides per request: a Zen chat completion whose model is a
  free model goes to the sidecar, everything else uses the hardened Go
  transport.

## Session affinity, account identity, and recovery

Free-tier behavior is not fully documented by OpenCode. Local probes show that
an accepted session id can matter, while successful phone-IP changes have also
restored access in some incidents. The generic `FreeUsageLimitError` does not
identify whether the cause is exhausted IP/account/session quota or a rejected
request shape. Treat the exact quota scope as empirical, not a contract.

The sidecar therefore:

1. derives the lease scope from the **account identity** (the captured
   workspace/org header) and client session headers for same-account keys, so
   rotating an expired key belonging to the same account does not discard an
   established lease. A key that hits `Workspace access denied` is learned as
   foreign and moved to its own `cred:<fingerprint>` lease lane;
2. captures the stock OpenCode request shape once per account. Captures contain
   workspace/org headers as well as the system prompt and tools; sending a new
   account's bearer with the previous account's captured workspace produces
   `Workspace access denied`. A key change alone does not recapture. On a
   workspace denial the sidecar drops the captured org/workspace headers for
   that key, takes a key-scoped lease, and retries once; it does not recapture
   the stock shape for a key from another account. The first attempt for a
   newly configured foreign key still carries the captured headers, so the
   denial is the discovery step. The bridge never waits on a per-request
   credential poll. A capture that lands while the account changes is only
   adopted if the file it wrote is newer than the capture started;
3. accepts the key selected by the Go credential pool on the authenticated
   loopback leg (`x-bridge-credential`). With no header it falls back to the
   active OpenCode database credential for standalone use. The inbound public
   `Authorization` is stripped before this leg, so a client key never leaks
   into the sidecar;
4. persists `scope -> {id, at, goodAt, cred}` and a small pool of recently
   accepted ids in `~/.local/share/opencode-native/sessions.json` (durable
   write + fsync, atomic rename, `0600`, capped at 512 scopes) so a service
   restart reuses the lease instead of losing it;
5. keeps the id for 24 hours of inactivity by default, refreshing the mapping
   at most every five minutes;
6. tries at most two recently accepted ids before invoking the shared reset
   engine once. The former default walked all 16 ids, producing up to 17 rapid
   identical 429s before egress recovery. After a successful reset, it retries
   once and, if needed, mints one fresh id for one final attempt;
7. caches a failed shape briefly only for the same model/request dimensions,
   account identity, **credential** and last successful egress reset. The lease
   and captured identity remain account-scoped, but the shape-failure cache is
   key-scoped: a key rotated in after a 429 must make a real attempt instead of
   inheriting the exhausted key's cached 429.

`BRIDGE_SESSION_ID` pins one id for smoke tests or recovery. It must match the
stock shape `^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$` and must not be the all-hex
spoof shape Zen rejects; anything else is logged as
`session_override_rejected` and not sent upstream. `BRIDGE_SESSION_STORE`
overrides the file path.

A fresh install has no leased id and cannot create one until the free window
reopens; `BRIDGE_SESSION_ID` can seed a known-leased id for a smoke test. Do
not delete `sessions.json` during a restart: the file is the lease record, not
a cache. A corrupt store is logged (`session_store_load_failed`) and treated as
empty rather than silently replaced.

The upstream chat API is stateless and the body carries the whole conversation,
so the id is an affinity/quota handle only, not a server-side conversation key.
This is an observed property of the free lane, not a documented guarantee.

### Long sessions and request-time pruning

The free tier also rejects a request whose prompt does not fit the window's
remaining token budget. Long agent runs accumulate tool output faster than
anything else, and a 100k+ token prompt can be rejected even when the model's
native context window is larger.

The sidecar prunes **at request time** in `transport.mjs`:

- the newest 8 tool results stay verbatim;
- older results are replaced with the first 600 characters plus a marker;
- if the newest 8 still exceed 64k characters, only the newest 3 stay verbatim;
- the client's stored conversation is untouched, and the model can re-run a
  tool when it needs the full output.

This keeps the full native window available instead of advertising a smaller
one. Kill switch: `BRIDGE_PRUNE_TOOL_RESULTS=0`. Tuning:
`BRIDGE_PRUNE_KEEP_RESULTS`, `BRIDGE_PRUNE_PREVIEW_CHARS`. Pruning is logged as
`context_pruned` with the result count and characters removed.

## Free-model detection

`internal/proxy.IsFreeModel` is intentionally generic:

- any model whose name contains `-free` (namespace stripped first), or
- `big-pickle`.

No model list is pinned. A new free model works as soon as Zen serves it,
without an `opencode-cc` release.

## Tool-surface gate

Zen also gates the free lane on the tool set. Measured on 2026-11-02:

- the stock OpenCode 13 tools are the canonical prefix and are accepted;
- 19 total tools (13 canonical + 6 client tools) fail with
  `FreeUsageLimitError`, and the same turn at 13 tools returns 200;
- 20 total tools also fail. This is not a simple count: an unfamiliar tool name
  makes the request look like an unknown client.

The sidecar therefore:

1. replaces the client's tool list with the captured canonical tools;
2. **always sends the full captured 13-tool list**, even when the client
   cannot run a tool (today: Code Mode's `execute` for T3's Claude Code lane,
   which declares no execution tool). Pruning even one entry was measured to
   break the shape fingerprint: the request still returned
   `FreeUsageLimitError` while quota was available. A system note right after
   the captured prompt names the tools the client cannot run. If the model
   still calls one, the Go layer drops the call rather than sending an
   undeclared tool name to the client; the streaming Anthropic lane also emits
   a visible notice so the turn does not end silently;
3. suppresses client-only tools and logs `tools_suppressed` once per distinct
   tool-name set;
4. keeps the client's tools executable by translating the canonical call back
   to the declared tool name and argument keys (`shell`→`Bash`, `read`→`Read`,
   `skill`→`Skill`, `path`→`file_path`, …);
5. falls back to `Bash` + `rg` when the client does not declare `Grep`/`Glob`;
6. emits a visible text notice when a call cannot be resolved, instead of
   ending the turn silently.

`BRIDGE_CLIENT_TOOL_EXTRAS=1` appends client tools for deliberate experiments,
but that shape is known to be rejected by the free tier and is not the default.

`BRIDGE_KEEP_DEAD_TOOLS=0` is a debugging escape hatch that restores the old
pruned shape. Do not set it in production: the free-tier gate fingerprints the
complete stock tool set, and the pruned shape produced fake quota 429s. With the
default keep-all shape the sidecar logs `tools_unavailable_noted` (the client
cannot run a tool, but the tool is still sent); `tools_unavailable_pruned`
appears only when the kill switch actually removed one and should never be seen
in production.

The sidecar prepends the captured stock system prompt and substitutes today's
date at request time (the capture embeds its own date, so an identity captured
yesterday would otherwise be rejected). Non-canonical client system messages
are kept after the captured prompt; the stock block stays first.

## GLM compatibility

Zen replaced the legacy `prompt_cache_retention` field with
`prompt_cache_options`. DeepSeek tolerates the old name, but GLM rejects the
whole request with `unsupported_parameter`. The sidecar normalises the old
field to `prompt_cache_options: { mode: "implicit" }` before forwarding.

## Operations

| Item | Value |
|---|---|
| Owner | `opencode-cc.service` (systemd user unit) |
| Public port | `127.0.0.1:8787` |
| Sidecar port | ephemeral, discover with `journalctl --user -u opencode-cc` or `ss -ltnp` |
| Sidecar auth | random per-process token in `x-bridge-token`; loopback alone is not trusted |
| Data staged at | `<data-dir>/native/transport.mjs` and `capture.mjs` |
| Identity | `~/.local/share/opencode-native/identity.json` (shared with older installs) |
| Logs | `journalctl --user -u opencode-cc -f` |
| Retired | `opencode-native.service`, `opencode-cc-ds-flash.service`, `:18790`, `:18791` |

Useful checks:

```bash
systemctl --user is-active opencode-cc
ss -ltnp | grep -E ':(8787|18790|18791)\b'
curl --noproxy '*' -sS http://127.0.0.1:8787/api/health
journalctl --user -u opencode-cc --since '5 min ago' | grep native
```

The inbound public client key never reaches the sidecar. The Go layer
authenticates the client at the public listener, then sets
`x-bridge-credential` to the selected pool key on the loopback leg. In
standalone mode with no header, the sidecar resolves the live OpenCode
credential itself. The public `Authorization` is not forwarded.

## Configuration

All knobs are optional environment variables read by the sidecar:

| Variable | Default | Purpose |
|---|---|---|
| `BRIDGE_CLIENT_TOOL_EXTRAS` | unset | `1` = append client tools (not recommended; the free tier rejects the shape) |
| `BRIDGE_MAX_TOOLS` | `19` | total tool cap only when extras are enabled |
| `BRIDGE_CLIENT_TOOL_PRIORITY` | `ToolSearch,DeferredToolPlaceholder,ReportFindings,ScheduleWakeup,Workflow,ListAgents` | extras preference order |
| `BRIDGE_MAX_TOOL_CHARS` | `38000` | hard character cap for the tool surface |
| `BRIDGE_MIN_START_GAP_MS` | `400` | spacing between request starts |
| `BRIDGE_HEADER_TIMEOUT_MS` | `240000` | first-attempt upstream header deadline |
| `BRIDGE_TRANSPORT_RETRY_BUDGET_MS` | `30000` | extra budget shared by transport retries (a header timeout is never retried) |
| `BRIDGE_MAX_AGGREGATE_BYTES` | `16777216` | cap for a buffered non-stream response |
| `BRIDGE_MAX_INFLIGHT` | `8` | concurrent requests |
| `BRIDGE_TRACE_DIR` | unset | dump normalized request bodies for diagnosis |
| `BRIDGE_RESET_SCRIPT` | `~/.config/opencode/scripts/request-reset.mjs` | shared egress reset engine |
| `BRIDGE_RESET_SOURCE` | `opencode-cc` | reset-engine source name |
| `BRIDGE_RESET_COOLDOWN_MS` | `60000` | minimum gap between automatic resets |
| `BRIDGE_RESET_TIMEOUT_MS` | `600000` | hard limit for one reset-engine run |
| `BRIDGE_RESET_SHARED_WAIT_MS` | `660000` | wait for a foreign reset lock (above the 600s stale threshold) |
| `BRIDGE_RESET_LOCK_STALE_MS` | `600000` | age at which a live-but-hung reset lock is reclaimed |
| `BRIDGE_SESSION_TTL_MS` | `86400000` | inactivity before a session id (and its lease) is rotated |
| `BRIDGE_SESSION_STORE` | `~/.local/share/opencode-native/sessions.json` | durable session-id store |
| `BRIDGE_SESSION_ID` | unset | pin one upstream session id (smoke tests, recovery) |
| `BRIDGE_LEASE_POOL_TTL_MS` | `86400000` | age after which an accepted id leaves the fallback pool |
| `BRIDGE_LEASE_POOL_LIMIT` | `16` | maximum accepted ids kept in the fallback pool |
| `BRIDGE_LEASE_FALLBACK_ATTEMPTS` | `2` | pool ids tried before a reset |
| `BRIDGE_SHAPE_FAILURE_TTL_MS` | `600000` | how long an unpassable request shape skips radio resets |
| `BRIDGE_CAPTURE_MODEL_TIMEOUT_MS` | derived | per-model identity capture timeout |
| `BRIDGE_RESET_STATE` / `BRIDGE_RESET_LOCK` | the engine defaults | override only for tests |
| `BRIDGE_NODE` | `node` | interpreter for the reset engine |
| `OPENCODE_CC_BUN` | autodetect | Bun executable |

### Quota handling

A non-quota 429 is returned to the caller as-is. A free-tier
`FreeUsageLimitError` is ambiguous: it can reflect quota state, account/session
affinity, or a request rejected by the free-tier gate. The sidecar joins a
reset already in flight or waits out a foreign lock. The bridge does not claim
that every successful IP change guarantees quota; the provider does not publish
enough detail to distinguish these causes from the error body alone.

The Go free lane treats a free-limit 429 as **limited, never dead**, and cycles
the configured keys:

1. the request starts on the current key; the sidecar attempts its own
   recovery (lease fallbacks and one egress reset) for that key;
2. if the attempt still returns a labeled free-limit 429, the Go layer marks
   that key limited for 60 seconds and retries with the next key in cyclic
   order (`key1 → key2 → … → key5 → key1`). A 429 without a free-limit label
   (for example `x-oc-reset: manual`) is surfaced, because rotating keys would
   only hide the real error;
3. every key attempt asks the sidecar to recover, but the shared reset engine
   is single-flight: requests inside its cooldown join or wait for one radio
   cycle instead of starting another. "A reset per key" means each key is tried
   with a reset request, not that five radios cycle for one request;
4. after a full pass over the pool, the lane backs off exponentially (5 s,
   10 s, 20 s, capped at 30 s) and starts the next pass. The cycle repeats
   until the request succeeds, its context is cancelled, a 3-minute wall-clock
   budget expires, or the 40-attempt ceiling is reached. The budget and ceiling
   keep a permanently limited IP from pinning an in-flight slot forever; in
   normal operation a pass is long enough to clear the reset cooldown.

Keys from different accounts do not permanently inherit the captured
account's `x-opencode-org-id` / `x-opencode-workspace-id`. The first attempt for
a newly configured key still carries the captured headers (that is how the
mismatch is discovered); a `403 Workspace access denied` then drops those
headers for that credential and moves it to a key-scoped lease lane. Leases are
key-scoped, so one account's session id is never replayed with another
account's key.

The sidecar adds diagnostic headers to a 429:

- `x-oc-native: 1`
- `x-oc-reset: manual` (non-quota 429), `failed` (reset failed, budget
  expired, or disabled), `shape` (reset + fresh-id retry still limited; resets
  are skipped for this shape), or `skipped` (legacy: the caller sent
  `x-bridge-no-reset`; the current Go lane never does)
- `x-oc-reset-source: opencode-cc`
- `retry-after` is deliberately **not** forwarded: the upstream value can be
  hours long and would lock local clients out. `x-oc-reset` describes the local
  recovery state instead.

The shape-failure cache is scoped by credential, account, egress and the last
successful reset time, so a rotated key makes a real attempt and a reset that
actually changed the egress is retried. The failure is recorded under the
post-recovery key, so a later pass hits the cache instead of cycling the radio
again. If every key reports `shape` in one pass, the request itself is
unpassable; the Go lane surfaces the real 429 immediately.

`/health` reports `quota.automaticRecovery: true`, the reset source, cooldown,
and `lease.pool`, `lease.lastGoodAgeMs`, `lease.fallbackAttempts` for diagnosis.
The engine still owns the radio, the lock, the cooldown and the reset state; the
sidecar only asks it to act.

## Migration from the fixed-port helper

1. Stop and disable `opencode-native.service`.
2. Remove `~/.config/systemd/user/opencode-cc.service.d/ds-flash.conf` (the
   `OPENCODE_CC_DS_FLASH_FETCH_URL` drop-in).
3. `systemctl --user daemon-reload`.
4. Install the new `opencode-cc` binary and restart only `opencode-cc.service`.
5. Confirm `ss -ltnp` shows `:8787` and no `:18790`/`:18791`.
6. Confirm the journal says `native free-tier transport on 127.0.0.1:<port>`.

`OPENCODE_CC_DS_FLASH_FETCH_URL` is no longer read; a stale drop-in is inert
but should be removed so the next operator is not misled.

## Rollback

Back up the binary, units and config before deploying. To roll back:

```bash
systemctl --user stop opencode-cc.service
install -m 0755 <backup>/opencode-cc ~/.local/bin/opencode-cc
# Restore units/config from the backup, then:
systemctl --user daemon-reload
systemctl --user start opencode-cc.service
```

The old code path required `OPENCODE_CC_DS_FLASH_FETCH_URL` plus a running
Bun helper. If that is the rollback target, restore the helper unit and drop-in
together; starting either half alone leaves free models broken.

## Verification

Run one request per client shape. A health response proves nothing.

```bash
KEY=<cpa-client-key>
for model in oc/deepseek-v4.1-flash-free oc/glm-5.3-flash-free; do
  for shape in messages chat/completions responses; do
    body='{"model":"'$model'","max_tokens":16,"messages":[{"role":"user","content":"Reply with exactly OK"}]}'
    [ "$shape" = responses ] && body='{"model":"'$model'","input":"Reply with exactly OK","max_output_tokens":16}'
    curl --noproxy '*' -sS -o /dev/null -w "$model $shape %{http_code}\n" \
      "http://127.0.0.1:8317/v1/$shape" \
      -H "authorization: Bearer $KEY" -H 'content-type: application/json' -d "$body"
  done
done

codex exec --profile cliproxyapi-glm-free --skip-git-repo-check "Reply with exactly GLM-OK"
codex exec --profile cliproxyapi-deepseek-free --skip-git-repo-check "Reply with exactly DS-OK"
```

For T3 Claude, the configured binary is `~/.local/bin/claude-t3`
(`install -m 0755 scripts/claude-t3 ~/.local/bin/claude-t3`). It rewrites
`--tools default` to a lean set and adds `--strict-mcp-config` for free models;
local models get a four-tool set, a short system prompt, low effort and a 16k
context hint. Paid models pass through untouched, and it never routes traffic.

## Model metadata

Verified 2026-11-01 against `https://opencode.ai/inference/v1/models`, the live
CPA catalog and each upstream profile. Client metadata should match this table,
not old guesses:

| Model | Context | Max output | Input | Notes |
|---|---|---|---|---|
| `oc/deepseek-v4.1-flash-free` | 200,000 | 32,000 | text, image | effort low/high/max |
| `oc/glm-5.3-flash-free` | 200,000 | 32,000 | text, image | Zen also advertises video |
| `ag/gemini-3.8-flash-high` | 1,048,576 | 65,536 | text, image | effort high baked into the ID |
| `cx/gpt-6-luna` | 500,000 curated (1,000,000 model max) | 128,000 | text, image | Codex profile uses the 500k middle |
| `local/lfm3-omni-2b` | 16,384 | 4,096 | text, image, audio | llama-swap `--ctx-size 16384` |

Both free models accept images even though the paid-model catalog used to
advertise text only; the Codex catalog now lists `image` for them.

Claude Code does not read this table. It derives its assumed window from the
model ID or from `modelPicker[].behavesAs`. The curated rows currently behave
as `claude-opus-4-8`, so Claude Code assumes a 1M window for every row while
Zen rejects free-model requests above 200k. Removing `behavesAs` from the free
rows makes Claude Code assume 200k, at the cost of an "unrecognized model"
notice and different effort defaults. `CLAUDE_CODE_MAX_CONTEXT_TOKENS` does
not override a model ID that Claude Code resolves to a known model.

## Known limits

- The sidecar needs a recent `opencode` on `PATH` to recap its identity when
  the installed version changes.
- A very large client tool surface is trimmed, not rejected. The client may
  lose access to tools it declared; the canonical six client slots are kept in
  client order, so important tools belong first.
- Free-tier eligibility is an upstream policy, not a property of this proxy.
  A 200 today does not promise a 200 tomorrow.
