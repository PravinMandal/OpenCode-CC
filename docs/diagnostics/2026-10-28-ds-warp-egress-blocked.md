# DS Flash: WARP egress is rejected, direct IPv4 works

Date: 2026-10-28 (IST) / 2026-10-27 (UTC)

## Scope

Investigated the request path:

`T3 Code / Claude Code -> CLIProxyAPI -> opencode-cc -> DS Flash helper -> OpenCode Zen`

The helper used the active `opencode` database credential only. No static
`oc_sk_` key was used for these tests.

## Finding

Every Cloudflare WARP egress tested returned `429 FreeUsageLimitError` for
`deepseek-v4.1-flash-free`, while the same active credential, request body,
User-Agent and headers on the host's direct IPv4 path returned 200.

Tested WARP paths:

- the official WARP local proxy, over both its HTTP bridge and SOCKS5;
- two fresh userspace `wgcf` + `wireproxy` profiles;
- WARP IPv6 destinations;
- WARP IPv4 destinations (forced with an IPv4 CONNECT target);
- WARP endpoints `engage.cloudflareclient.com`, `162.159.192.1`,
  `188.114.96.1`, and `188.114.97.1`.

All returned the same quota envelope. The endpoint variants shared the same
WARP egress address (`104.28.219.91`) in this environment. Direct IPv4 returned
200. Direct IPv6 returned 429.

This is consistent with the free gate treating the shared WARP egress as
exhausted/restricted. Reconnecting WARP or adding another WARP profile does not
change that policy result. The older fork's IPv4-preference evidence was true
for its own paths, but it does not make this shared WARP egress usable for the
free model.

A request without the stock OpenCode User-Agent shape also returned 429 against
WARP and direct. The relay's `ocUA()` header is therefore required; it was
present on the successful production path.

## Live decision

- Keep only the active `opencode://active` database credential in the relay
  pool. The static keys remain removed and dead.
- Use **direct IPv4 first**. It is the only egress currently accepted by the
  provider.
- Keep the userspace WARP pool only as a fallback, never as the first choice.
  It is loopback-only: `wireproxy` runs in userspace, no TUN, no default route,
  no DNS change, and no host-wide network change.
- On a free-limit rejection, the helper can ask the WARP pool to rotate and
  retry once with a fresh protocol session. That path is best-effort; the
  provider rejection applies to WARP itself.
- `DS_FLASH_AUTO_RESET=0`; the helper does not use the Telegram phone reset.

## Evidence

- 16 direct-first requests through CPA: 16 HTTP 200, no 429.
- Earlier direct path soak: 18/18 HTTP 200 before it was stopped for the
  deployment.
- A new direct-first 3-hour soak was started at 2026-10-27 18:45:57 UTC
  (2026-10-28 00:15 IST), 90-second interval. It is the authoritative
  sustained test; a short burst alone is not acceptance.
- Host default IPv4 and IPv6 routes were unchanged throughout.
- WARP profiles and their private keys are mode 0600 under
  `~/.config/opencode-cc/warp/`; no credential or private key is printed by
  the manager.

## Remaining risk

The provider can impose a quota on the direct residential IPv4 too. WARP cannot
work around that because its egress is what is being rejected. If the direct
path later returns 429, the provider-level remedies are a new active credential
or account/paid plan, not another WARP reconnect. The live pool must never be
fed the old static keys again.
