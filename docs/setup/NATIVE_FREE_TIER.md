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

## Session lease and durability

The free tier leases quota to the session id, not to the API key, organization
or egress address. A newly generated id is answered with
`FreeUsageLimitError` even while an established id on the same key and IP
returns `200`. The sidecar therefore:

1. derives one scope from the **live credential fingerprint** and the client
   session headers. The inbound `Authorization` is deleted by the Go leg before
   the sidecar sees the request, so the credential fingerprint (not that
   header) is what separates accounts; a credential/account rotation starts a
   new lease lane instead of pinning the old account's id;
2. persists `scope -> {id, at, goodAt, cred}` and a small pool of recently
   accepted ids in `~/.local/share/opencode-native/sessions.json` (durable
   write + fsync, atomic rename, `0600`, capped at 512 scopes) so a service
   restart reuses the lease instead of losing it — the original incident was a
   restart that forgot every id;
3. keeps the id for 24 hours of inactivity by default, refreshing the mapping
   at most every five minutes;
4. keeps up to 16 accepted ids in the lease pool for 24 hours. When a fresh id
   has no lease (typically after the daily free window closes), the sidecar
   tries pool candidates **least-recently-good first**, up to three per
   request, so new conversations do not all converge on one id and exhaust it
   while the others still have quota. Only when the pool is exhausted does it
   invoke the reset engine; after a successful reset it retries the current id
   once, then mints one fresh id and retries once more. A rejected id is
   removed from the pool and its scope mapping is expired, so it cannot be
   handed back;
5. records a failed request shape after reset + fresh-id retry still returns
   `FreeUsageLimitError`, and skips further radio resets for that shape for
   ten minutes (`x-oc-reset: shape`). The free tier returns the same generic
   error for unknown clients and oversized tool surfaces, so without this a
   shape that can never pass would cycle the phone once per turn.

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
2. removes every canonical tool the client cannot run at all — not just a
   trailing suffix, because a dead tool in the middle stays callable while a
   note claims it is unavailable (today: Code Mode's `execute` for T3's
   Claude Code lane, which declares no execution tool) — and logs
   `tools_unavailable_pruned`; the captured prompt still describes the pruned
   tools, so a system note right after it says they are unavailable. Without
   this, the model calls `execute`, the Go layer has no declared target, and
   the turn ends with a dropped-call notice. The remaining canonical tools
   keep the free-tier gate's accepted shape;
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

`BRIDGE_KEEP_DEAD_TOOLS=1` disables the dead-tool prune and sends the full
captured list; the system note still names the tools the client cannot run, so
the model is told not to call them. Use it only if the free-tier shape gate
starts rejecting the shorter canonical list.

The captured system prompt embeds the capture date, so the sidecar substitutes
today's date at request time; otherwise an identity captured yesterday would be
rejected.

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

A free request must not use the inbound client key upstream. The sidecar
resolves the live OpenCode credential itself and clears the inbound
`Authorization` header.

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
| `BRIDGE_LEASE_FALLBACK_ATTEMPTS` | `3` | pool ids tried before a reset |
| `BRIDGE_SHAPE_FAILURE_TTL_MS` | `600000` | how long an unpassable request shape skips radio resets |
| `BRIDGE_CAPTURE_MODEL_TIMEOUT_MS` | derived | per-model identity capture timeout |
| `BRIDGE_RESET_STATE` / `BRIDGE_RESET_LOCK` | the engine defaults | override only for tests |
| `BRIDGE_NODE` | `node` | interpreter for the reset engine |
| `OPENCODE_CC_BUN` | autodetect | Bun executable |

### Quota handling

A non-quota 429 is returned to the caller as-is. A free-tier
`FreeUsageLimitError` is evaluated against the request's session lease: an
established id can be allowed while a fresh id on the same key, org and IP is
denied. The sidecar walks the lease ladder described above (pool → reset →
fresh id), then calls the shared reset engine
(`request-reset.mjs --source=opencode-cc`, `RESET_ALLOW_AUTOMATIC=1`) at most
once, joins a reset already in flight or waits out a foreign lock. The reset
path remains for a genuinely exhausted IP; it is not a substitute for session
reuse. The sidecar surfaces a 429 only when the reset failed, the retry is
still limited, or the request shape is marked unpassable.

The sidecar adds diagnostic headers to a 429:

- `x-oc-native: 1`
- `x-oc-reset: manual` (non-quota 429), `failed` (reset failed/backoff/disabled),
  `retried` (reset succeeded but the retry is still limited), or `shape`
  (reset + fresh-id retry still limited; resets are skipped for this shape)
- `x-oc-reset-source: opencode-cc`
- `retry-after` is deliberately **not** forwarded: the upstream value can be
  hours long and would lock local clients out. `x-oc-reset` describes the local
  recovery state instead.

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
