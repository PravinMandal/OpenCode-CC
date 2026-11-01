# Native free-tier transport

This documents the free-model lane in `opencode-cc` as of November 2026. It
replaces the separate DS Flash helper service and the fixed `:18790` listener.

## Why a Bun hop exists at all

Zen's free tier is gated on the TLS/HTTP2 stack, not only on the request body.
Measured on 2026-11-01 with an identical body, headers, key and session:

| Client | Result |
|---|---|
| Bun (`fetch`, HTTP/2) | `200` |
| Go `net/http` | `429 FreeUsageLimitError` |
| Node `fetch` | `429` |
| Python `requests` | `429` |
| `curl` | `429` |

A byte-for-byte replay that Bun completes successfully is rejected from Go, so
the transport cannot be ported into the Go proxy. `opencode-cc` therefore keeps
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

## Free-model detection

`internal/proxy.IsFreeModel` is intentionally generic:

- any model whose name contains `-free` (namespace stripped first), or
- `big-pickle`.

No model list is pinned. A new free model works as soon as Zen serves it,
without an `opencode-cc` release.

## Tool-surface gate

Zen also gates the free lane on the tool set. Measured on 2026-11-01:

- the stock OpenCode 13 tools are the canonical prefix and are accepted;
- 19 total tools pass;
- 20 total tools fail with `FreeUsageLimitError`, even when the same `/64`
  passes a stock probe;
- some 17/18-tool subsets fail, so it is not only a count.

The sidecar therefore:

1. prepends the captured canonical system prompt and the canonical 13 tools;
2. appends at most 6 client tools (`BRIDGE_MAX_TOOLS`, default 19 total),
   preserving client order and dropping the rest;
3. logs `tools_trimmed` with counts and an 8-name deduped sample;
4. if the request still gets a shape `429`, retries once with the canonical 13
   tools only (`shape_retry`);
5. only rotates the network when a stock-shaped probe also fails. A shape
   rejection is never "fixed" by cycling the hotspot.

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
| `BRIDGE_MAX_TOOLS` | `19` | total tool cap (canonical + client extras) |
| `BRIDGE_MAX_TOOL_CHARS` | `38000` | hard character cap for the tool surface |
| `BRIDGE_MAX_RECOVERY` | `2` | maximum egress rotations per request |
| `BRIDGE_RECOVERY_INTERVAL_MS` | `60000` | minimum gap between rotations |
| `BRIDGE_MIN_START_GAP_MS` | `400` | spacing between request starts |
| `BRIDGE_HEADER_TIMEOUT_MS` | `240000` | upstream header deadline |
| `BRIDGE_MAX_INFLIGHT` | `8` | concurrent requests |
| `BRIDGE_TRACE_DIR` | unset | dump normalized request bodies for diagnosis |
| `BRIDGE_RESET_ENGINE` | `~/.config/opencode/scripts/request-reset.mjs` | optional egress reset command |
| `OPENCODE_CC_BUN` | autodetect | Bun executable |

### Quota reset is optional

When a 429 is followed by a failing stock probe, the sidecar runs the configured
reset engine once with `--source=opencode-cc`. On this machine that engine
notifies the phone and verifies the public IPv6 prefix changed; it is
user-specific and is not shipped in this repository. If the script is absent,
the sidecar logs `recovery_unavailable` and returns the upstream 429 — paid
models are unaffected.

The sidecar adds diagnostic headers to a 429:

- `x-oc-native: 1`
- `x-oc-reset: needed` (budget wall) or `shape` (request-shape rejection)
- `x-oc-reset-last: <timestamp>` when a rotation has run
- `x-oc-reset-throttled: 1` when the minimum interval blocked another rotation

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
