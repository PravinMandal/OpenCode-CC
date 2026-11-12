# Set up Wraient's relay stack on another laptop

This is the agent handoff and installation guide for the **patched Wraient fork**,
branch `patch/opencode-user-agent`. Read this before executing setup commands.
The target is Linux with systemd user services. Other operating systems need
adapted service/network instructions; they have not been accepted here.

**Optional clients:** see [Claude Code terminal and T3 Code setup](OPTIONAL_CLIENTS.md).
Neither is required for other relay clients. Claude is required only when you
choose the Claude route or run its acceptance tests; T3 is an optional UI.

The exact tested chain is:

```text
Claude Code -- deepseek-v4.1-flash-oc --> CLIProxyAPI :8317 (patched)
  --> opencode-cc :8787 -- free models --> Bun sidecar (ephemeral loopback port)
  --> OpenCode Zen

Other models: opencode-cc Go transport --> its configured HTTPS_PROXY / NO_PROXY
Optional WARP: selected application --> SOCKS5 :40000 --> Cloudflare --> Internet
Phone route: :18889 --> isolated userspace Tailscale --> Android exit node --> cellular
Telegram control: local script --> Bot API --> phone automation --> cellular reset
```

> **Superseded (November 2026):** the separate DS Flash helper service and the
> fixed `:18790` listener are retired in this fork. `opencode-cc` now owns one
> Bun sidecar on an ephemeral loopback port and routes every `-free` model
> through it. New installs do not need `opencode-cc-ds-flash.service` or the
> `ds-flash.conf` drop-in. See
> [Native free-tier transport](NATIVE_FREE_TIER.md). This guide describes the
> current native sidecar; the old helper deployment is not supported.

**Ready for a technical friend's supervised installation, not a turnkey or
unlimited free service.** Use your own account, key, phone, tailnet and bot. A
working key, healthy daemon, changed IP or HTTP 200 headers alone do not prove
successful inference. Free model eligibility, authentication, network conditions
and quota are separate. Provider changes can break this compatibility path.

Latest validation: [2026-10-27 setup evidence](../evidence/2026-10-27-friend-setup.json).
Real saved-key/active-OAuth file tasks, brief outage and sustained outage/recovery
passed on the reference laptop with a freshly built CPA and separate direct-only
helper. Direct and phone HTTPS traces passed; WARP HTTPS timed out, and its CLI
requested Terms acceptance. No new device enrollment, real Telegram reset, full
container chain or second physical laptop installation was tested.

## 1. What is included and what is private

| Component | Included here | Friend must supply |
|---|---|---|
| Go relay, web dashboard, translations, auth | Source and tests | Go 1.24+, Node 20+/npm to build UI |
| Free-tier Bun sidecar | Embedded in `opencode-cc` (`internal/native/`) | Bun on the machine; the old fixed-port helper is retired |
| CPA retry behavior | `contrib/cliproxyapi/` patch bundle + example config | Upstream v8.0.4 source, Go toolchain requested by its go.mod |
| Claude Code (optional client) | Acceptance runner and wrappers | Official CLI 2.1.310 for recorded Claude tests; not required for other clients |
| OpenCode account | Read-only active credential resolver | Own Zen key or logged-in OpenCode v2; native fixture was 2.1.2 |
| WARP | Instructions below | Own installed/registered WARP; optional |
| Phone HTTP proxy | Userspace Tailscale recipe below | Own tailnet and approved Android exit node |
| Telegram reset | `contrib/telegram-ip-reset/` sender and template | Own bot/chat and **phone-side automation**; no phone automation export is included |
| Credentials/state | None | Private env files, SQLite, bot tokens, Tailscale enrollment |

Do not copy Wraient's `~/data`, OpenCode database, WARP registration, Tailscale
state, `~/.local/state/morph-relay`, Telegram env files, CPA auth pool or editor
settings. Historical diagnostics contain machine paths, status/timing evidence
and short credential fingerprints; they are not installation configuration.

Agent rule: inspect existing listeners/services/config before installing. Do not
stop other applications, switch models/accounts, enroll another person's node,
change system-wide VPN routing, or trigger a phone reset as an installation test.
If a required private component is missing, report it and keep its feature off.

## 2. Ports and isolation

| Port | Meaning on the reference laptop |
|---|---|
| 8787 | Production relay; exactly one process |
| 8317 | Production CPA |
| 18790 | Retired; must not listen (sidecar uses an ephemeral port) |
| 18889 | Phone HTTP CONNECT proxy; loopback only |
| 40000 | Optional WARP SOCKS5 proxy |
| 18789 | Isolated staging relay |
| 18319 | Isolated acceptance CPA |
| 18794 | Acceptance fault injector |
| 18787 / 18788 / 18890 | Morph services on Wraient's machine; leave alone |

```bash
ss -ltnp
systemctl --user status opencode-cc cliproxyapi --no-pager
```

Check ports before starting. Acceptance reserves 18789/18319/18794 and refuses
occupied ports. The relay owns its Bun sidecar; there is no separate helper
config or listener. No public hostname or Cloudflare Worker is required for this
laptop installation.

## 3. Build the fork and install privately

Commands below assume Bash, run from the repository root, and a fresh setup.
Existing installations must use the staging/atomic update steps in section 10.
There is no Makefile. The upstream GHCR image does **not** include this fork's
unpublished/local fixes or the Bun sidecar; build this checkout yourself.

```bash
git clone --branch patch/opencode-user-agent https://github.com/Wraient/opencode-cc.git
cd opencode-cc
(cd web && npm ci --no-audit --no-fund && npm run build)
mkdir -p internal/assets/dist
cp -a web/dist/. internal/assets/dist/
go build ./...
go test ./...
go build -o /tmp/opencode-cc-stage .
install -d -m 700 "$HOME/.config/opencode-cc" "$HOME/data"
install -d "$HOME/.local/bin" "$HOME/.config/systemd/user"
install -m 755 /tmp/opencode-cc-stage "$HOME/.local/bin/opencode-cc.new"
mv -f "$HOME/.local/bin/opencode-cc.new" "$HOME/.local/bin/opencode-cc"
```

Create `~/.config/opencode-cc/env` using a private editor, mode **0600**:

```dotenv
OPENCODE_CC_LISTEN=127.0.0.1:8787
OPENCODE_CC_PANEL_TOKEN=REPLACE_WITH_YOUR_OWN_LONG_RANDOM_PANEL_PASSWORD
OPENCODE_CC_REQUIRE_API_KEY=true
OPENCODE_CC_UPSTREAM=https://opencode.ai/inference
```

Create `~/data/config.json`, mode **0600**, with your own key in place of the
placeholder (or the literal reference `opencode://active`, described below):

```json
{
  "upstreams": [{
    "name": "my-zen",
    "base_url": "https://opencode.ai/inference",
    "api_key": "REPLACE_WITH_YOUR_OWN_ZEN_KEY",
    "enabled": true
  }],
  "default_model": "deepseek-v4.1-flash-free",
  "log_requests": false
}
```

A nonempty enabled `upstreams` pool takes precedence over the legacy
`ZEN_API_KEY`/`OPENCODE_CC_UPSTREAM` pair. Editing only env does not migrate that
pool. UI settings are persisted in this file; process env overrides supported
fields at load. Never commit this directory. Logging can capture prompts, code,
tool arguments and responses; turn it on only when needed and protect the DB.

Install this user unit as `~/.config/systemd/user/opencode-cc.service`:

```ini
[Unit]
Description=Local OpenCode Zen relay
After=network-online.target

[Service]
ExecStart=%h/.local/bin/opencode-cc -data %h/data
EnvironmentFile=%h/.config/opencode-cc/env
UMask=0077
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
```

Use an explicit `-data`: the reference machine historically relied on its service
working directory yielding `~/data`; a different cwd silently selects a different
config/database. Enable boot-before-login only if wanted with
`loginctl enable-linger "$USER"` (may require administrator authorization).

### Key vs active OAuth

`opencode://active` reads exactly one active OpenCode v2 `credential` row for
integration `opencode`, read-only, on each upstream selection. It is accepted
only for HTTPS `opencode.ai`. Default DB:
`${XDG_DATA_HOME:-$HOME/.local/share}/opencode/opencode.db`; override with
`OPENCODE_CC_OPENCODE_DB=/absolute/path/to/opencode.db` in the service env.

The official OpenCode CLI owns login/refresh. The relay does not refresh OAuth
and rejects expired or nearly expired access tokens. Use the CLI to restore the
connection. The legacy `~/.local/share/opencode/auth.json` can contain an expired,
different credential: it is not evidence of the active v2 identity. Never copy
refresh tokens or write its credential database. A static Zen key avoids that
refresh dependency but still has the account's real permissions and limits.

## 4. Install Bun for the native free-tier sidecar

The free-tier transport is a Bun sidecar embedded in `opencode-cc`
(`internal/native/transport.mjs` + `capture.mjs`). There is **no separate
service and no fixed port**: the proxy extracts the scripts to a private temp
directory, starts Bun on an ephemeral loopback port, and supervises it. All
`-free` models and `big-pickle` route through it.

Bun is still a runtime dependency. Install it from the
[official instructions](https://bun.sh/docs/installation) and verify the
version. Production here uses **1.4.2**; do not assume another release has the
same behavior. Put the verified executable where the proxy looks first:

```bash
BUN_BIN="$HOME/.bun/bin/bun"
"$BUN_BIN" --version
install -d "$HOME/.local/lib/opencode-native"
install -m 755 "$BUN_BIN" "$HOME/.local/lib/opencode-native/bun"
```

`OPENCODE_CC_BUN=/path/to/bun` overrides discovery; otherwise the proxy checks
`~/.local/lib/opencode-native/bun`, then `PATH`, `/usr/local/bin/bun` and
`/usr/bin/bun`. The old `opencode-cc-ds-flash.service`, `opencode-native.service`
and `:18790` listener are retired; do not install them.

What the sidecar does, and why it must not be "optimized":

- replaces the client's prompt with the exact captured stock OpenCode system
  prompt (only the `Today's date:` line is refreshed);
- always sends the complete captured **13-tool** canonical list — `edit`,
  `glob`, `grep`, `read`, `shell`, `skill`, `subagent`, `webfetch`,
  `websearch`, `write`, `execute`, `question`,
  `experimental_image_generate`;
- sends `User-Agent: opencode/latest/2.2.7/cli` and
  `X-Opencode-Client: cli`;
- maps canonical tool calls back to the client's declared names and argument
  keys (`shell`→`Bash`, `read`→`Read`, `path`→`file_path`, …);
- keeps sessions on a key-scoped lease so one account's session id is never
  replayed with another account's key, and drops
  `x-opencode-org-id`/`x-opencode-workspace-id` when a key belongs to a
  different workspace;
- on a 429, joins or starts one shared egress reset per attempt; the Go layer
  then cycles keys (`key1 → … → key5 → key1`) with exponential 5–30 s backoff
  until the request succeeds, its context is cancelled, a 3-minute wall-clock
  budget expires, or a 40-attempt safety ceiling is reached.

The full contract, env table and diagnostics are in
[`NATIVE_FREE_TIER.md`](NATIVE_FREE_TIER.md). A new install needs only Bun and
the proxy; no drop-in for a separate helper is required.

## 5. CPA: the local patches are required for the exact recovery behavior

Follow [the bundled CPA build/config instructions](../../contrib/cliproxyapi/README.md).
Stock CPA v8.0.4 does not implement `rate-limit-hold-seconds`; copying the YAML
field into stock CPA can fail configuration validation. Do not claim outage
recovery equivalence from a generic CPA release.

The reference patch sets a 180-second hold **only** on the relay's `claude-2`
group. For opted-in credentials it retries 429s with 2→4→8→15-second backoff,
returns the real last error after the hold expires, and caps/restores stale
local 429 cooldown state under that policy. This intentionally changes local
retry scheduling; it does not clear upstream quota or blanket-disable cooling
for other providers. Unrelated auth/quota errors must remain visible.

Use the example YAML in `contrib/cliproxyapi/config.example.yaml`, replacing
both keys: the client→CPA key and the CPA→relay key created in the panel. They
are different from the upstream Zen key. Alias:
`deepseek-v4.1-flash-oc` → `deepseek-v4.1-flash-free`. If other providers share
CPA, do not replace their config; merge only the dedicated relay group. Local
production uses port 8317; staging acceptance uses 18319.

Claude speaks Anthropic to CPA at `http://127.0.0.1:8317` (no `/v1` suffix).
Set `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN` to the private CPA client key,
and launch with `--model deepseek-v4.1-flash-oc`. Keep fast mode off. For this
model set `CLAUDE_CODE_MAX_CONTEXT_TOKENS=260000` and
`CLAUDE_CODE_AUTO_COMPACT_WINDOW=200000`. These are launch-time limits and do
not increase upstream capacity (measured input ceiling was about 261K).
Restart the Claude process when switching between DeepSeek and other models.

The reference T3 setup uses one Claude CPA provider (`claudeAgent_2`), with
shared defaults 460000 / 400000 for other models. This is a local convention,
not a requirement to create or overwrite a friend's T3 configuration.
`scripts/claude-cpa-context` contains Wraient's absolute Claude executable path;
edit a personal installed copy for your machine. Do not point it at itself.
`scripts/claude-cpa` assumes `~/.cli-proxy-api/api-key.txt` and a models.env slot
configuration; use its documented env overrides or configure your harness
directly. Neither wrapper installs CPA or creates credentials.

For step-by-step client installation, terminal launch commands, T3 provider
fields and the optional mixed-model context launcher, continue with
[Optional Claude Code and T3 Code](OPTIONAL_CLIENTS.md). You may skip both.

## 6. WARP: optional application proxy, separate from the phone

WARP encrypts selected traffic to Cloudflare and gives it a Cloudflare Internet
exit. It is not OpenCode authentication, an Android exit, an inbound tunnel, or
a guarantee of free-model eligibility. In **local proxy mode** only applications
explicitly pointed at its listener use it; the rest of the laptop retains its
normal routing. See [Cloudflare's mode documentation](https://developers.cloudflare.com/warp-client/warp-modes/)
and [Linux installation/registration](https://developers.cloudflare.com/warp-client/get-started/linux/).

Reference machine: Arch Linux, reviewed `cloudflare-warp-bin` AUR recipe wrapping
the official binary, version **2026.8.2115.0**, system `warp-svc.service`, consumer
registration, SOCKS5 `127.0.0.1:40000`. Arch packaging is community-maintained;
verify its source/checksum. Other distros should use Cloudflare's package repo.
Register your own client; do not clone Wraient's registration state. The CLI
may require first-run Terms acceptance interactively before status/configuration.

After installing the package, on a new consumer registration:

```bash
sudo systemctl enable --now warp-svc
warp-cli registration new   # only if not already registered
warp-cli mode proxy
warp-cli proxy port 40000
warp-cli tunnel protocol set MASQUE
warp-cli connect
warp-cli status
curl --noproxy '' --max-time 15 --proxy socks5h://127.0.0.1:40000 https://www.cloudflare.com/cdn-cgi/trace
```

Require `warp=on` from that **proxied** trace; status alone is insufficient. The
installed reference version requires MASQUE for local proxy mode. Its automatic
HTTP/3/HTTP/2 fallback is retained; don't force protocol, MTU or endpoint tweaks
from old experiments into persistent configuration. CLI syntax changes across
versions: consult `warp-cli mode --help` / `warp-cli tunnel protocol --help`.
Disconnect with `warp-cli disconnect`; this stops the optional proxy's egress.

To opt **the Go relay** into WARP, put these in its private env and restart:

```dotenv
HTTP_PROXY=socks5h://127.0.0.1:40000
HTTPS_PROXY=socks5h://127.0.0.1:40000
NO_PROXY=127.0.0.1,localhost,::1
```

Check lowercase proxy variables too; avoid conflicting values. `NO_PROXY` can
bypass an explicit proxy: adding `opencode.ai` makes matching Zen requests go
direct. The reference production process had that bypass during investigation;
its proxy env therefore did not establish that Zen used WARP. Inspect the
**running service** environment locally without printing credentials; a shell's
environment or the env file alone may differ from the process until restart.
Go's environment proxy selection is cached; restart after a change.

The native sidecar inherits the relay service environment, including
`HTTPS_PROXY`/`NO_PROXY`; Bun's `fetch` honors them. To route free traffic
through the phone, set `HTTPS_PROXY=http://127.0.0.1:18889` in the relay unit
and restart it; `NO_PROXY` must not include `opencode.ai`. There is no separate
helper route setting, and a shell's environment does not affect the running
service until restart. `socks5h` asks curl to resolve names through SOCKS; do
not use that scheme for the phone's HTTP CONNECT listener.

Observed caveats: WARP sometimes reported connected while HTTPS timed out;
a successful trace did not prove inference. WARP egress was 429-limited while
direct worked in one controlled comparison. Neither observation is universal.
There are no model probes on idle; don't add tight retry/monitor loops.

## 7. Phone egress and Telegram recovery

### Isolated Tailscale HTTP proxy

Use your own Android phone with cellular data and a working Tailscale exit-node
configuration. Enable/approve that exit node and tailnet permissions using
[Tailscale's exit-node instructions](https://tailscale.com/docs/features/exit-nodes).
On Android this is userspace forwarding; power saving, VPN permission, carrier
NAT, IPv4/IPv6 and mobile data coverage affect availability. Keep the phone
powered and allow the relevant apps to run in the background.

The laptop uses an **additional userspace tailscaled daemon**, with its own
state and socket, so choosing the phone exit only affects this proxy. It does
not replace the laptop's ordinary Tailscale daemon or default route. See
[Tailscale userspace proxy documentation](https://tailscale.com/docs/concepts/userspace-networking).

Example user unit `~/.config/systemd/user/phone-egress.service` (install Tailscale
first; adapt binary paths for your distro):

```ini
[Unit]
Description=Isolated phone egress HTTP proxy
After=network-online.target

[Service]
ExecStart=/usr/bin/tailscaled --tun=userspace-networking --statedir=%h/.local/state/phone-egress --socket=%h/.local/state/phone-egress/tailscaled.sock --outbound-http-proxy-listen=127.0.0.1:18889
UMask=0077
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
```

```bash
install -d -m 700 "$HOME/.local/state/phone-egress"
systemctl --user daemon-reload
systemctl --user enable --now phone-egress
# Enroll interactively in YOUR tailnet; approve/login at the private URL shown.
tailscale --socket="$HOME/.local/state/phone-egress/tailscaled.sock" up --accept-dns=false
# Substitute your own approved phone's name or Tailscale address.
tailscale --socket="$HOME/.local/state/phone-egress/tailscaled.sock" set --exit-node=YOUR_PHONE
curl --noproxy '' --max-time 15 --proxy http://127.0.0.1:18889 https://www.cloudflare.com/cdn-cgi/trace
```

Keep enrollment URLs, auth keys and daemon state private. This loopback proxy is
not authenticated: do not bind it to the LAN or publish it through a tunnel.
Verify real HTTPS egress, not just a health port. The trace reveals the phone's
public exit IP; keep personal IP logs out of public reports. A cellular IPv4 can
change between calls even without a reset; IPv6 behavior may differ.

**Reference-only context:** Wraient's proxy is provided by
`morph-tailscale.service` under `~/.local/state/morph-relay`, using the saved
`ts-sidecar` identity while its original AWS instance is off. Running that
identity in both places causes a collision. Friends must enroll a fresh node,
not copy it. Morph's relay/harbor/opencode/tunnel/Worker/VPC integration is a
separate product and is not needed here. The old farm path on port 18888 is
also separate; it is not the sidecar's 18889 phone egress listener.

Once phone egress is verified, add a relay drop-in:

```ini
[Service]
Environment=HTTPS_PROXY=http://127.0.0.1:18889
Environment=NO_PROXY=127.0.0.1,localhost
```

Restart the relay. This sends the Go relay and its embedded sidecar through
cellular; it does not force all laptop traffic through the phone. Remove the
drop-in and restart to return to direct egress.

### Telegram is a control channel, not the data proxy

The laptop sends a fixed message through Telegram Bot API to a private chat.
Automation **on the phone** watches the authorized chat/sender and exact text,
then cycles cellular connectivity. Depending on Android/version this may need
Tasker/MacroDroid plus permitted ADB/root/device-owner actions; a normal app
cannot universally toggle airplane mode. There is no universal Android automation
export in this repository. Configure and validate that piece on your own device.
A bot message being sent successfully does not prove the phone executed it.

Use a dedicated bot/chat and restrict who can trigger the phone action. Decide
which chat the phone watches, its exact trigger, how long the mobile connection
is down, and how its Tailscale connection returns. Keep ordinary notifications
separate: Wraient also has `~/.codex/telegram.env` for blocker notifications;
that is **not** the reset engine's configuration and is not included.

Install [the bundled sender and private config template](../../contrib/telegram-ip-reset/README.md).
The shared reset engine expects exactly:

- executable `~/.config/opencode/skills/telegram-ip-reset/bin/telegram-ip-reset.sh`;
- config `~/.config/telegram-ip-reset.env`, mode 0600;
- `TELEGRAM_IP_RESET_BOT_TOKEN`, `TELEGRAM_IP_RESET_CHAT_ID`, and
  `TELEGRAM_IP_RESET_TEXT` matching the phone rule.

Only after the owner authorizes automatic phone resets and a manual reset has
been verified, install `~/.config/opencode/scripts/request-reset.mjs` and its
Telegram sender. The sidecar invokes that engine with
`RESET_ALLOW_AUTOMATIC=1`; the engine is single-flight and shares its lock and
cooldown with the OpenCode plugin and the `opencode-cc` Go lane. Do not delete
the engine's timestamp to force extra resets. It never intentionally resets
WARP/Morph services.

The reset engine clears inherited proxy variables before invoking the sender;
Bot API control goes direct, while its recovery probe uses phone HTTP proxy
:18889. Sender output is discarded. The engine independently checks HTTPS trace
before and after; where a before-IP exists it requires a changed IP, otherwise
it can only establish restored egress. It polls up to 48 times with 5-second
gaps and 8-second probe deadlines. A reset can interrupt every service sharing
that phone exit. Direct requests and WARP requests are unaffected by the phone
IP change. Never treat resets as account-quota renewal or promise they repair
429s.

## 8. Secure local access before connecting clients

Sign in at `http://127.0.0.1:8787` using the panel password. Create a relay client
API key in the Keys page for CPA; use a separate key for direct clients if wanted.
`OPENCODE_CC_REQUIRE_API_KEY=true` protects `/v1/*`; the panel password protects
administration. These are separate controls. Configure budgets/IP restrictions
as appropriate. Confirm requests without a client key are rejected:

```bash
curl --noproxy '*' -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8787/api/config
curl --noproxy '*' -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8787/v1/models
```

Expect 401 without auth on both, while health endpoints can remain public.
Use localhost/private access for installation. To serve friends remotely, add
TLS, explicit authentication and firewall/reverse-proxy rules; do not expose
the dashboard or phone/helper proxy publicly. This guide installs independent
copies on friends' laptops; it does not publish Wraient's running service.

## 9. Acceptance on the friend's actual machine

Unit tests and historical evidence do not establish a new machine's eligibility.
First run a minimal real message against an exact free model, then the full
Claude/CPA file-tool task. No model/account substitution counts as acceptance.
Avoid dashboard quota-bar polling.

```bash
go build ./... && go test ./...
go test ./internal/native/ ./internal/server/
go build -o /tmp/opencode-cc-stage .
```

Deploy with section 10, then:

```bash
curl --noproxy '*' -fsS http://127.0.0.1:8787/api/health
# Anthropic wire, free model, non-streaming. Use the client key from the panel.
curl --noproxy '*' -sS http://127.0.0.1:8787/v1/messages \
  -H "content-type: application/json" \
  -H "x-api-key: $OPENCODE_CC_CLIENT_KEY" \
  -d '{"model":"deepseek-v4.1-flash-free","max_tokens":64,
       "messages":[{"role":"user","content":"Reply with the single word: ok"}]}'
```

Then run a real Claude Code task that must Read a file, Write a proof file and
run one shell command; check the relay log for resolved tool calls and the
file's contents. Health alone is not acceptance.

The shape gate means a 200 on the minimal message does not prove the full tool
surface: the tool task is the real acceptance. If the minimal message passes but
the tool task returns `FreeUsageLimitError`, check that the sidecar sent the full
13-tool list (`tools_unavailable_pruned` must not appear; only
`tools_unavailable_noted` is normal) and the exact headers
before blaming quota.

Keep any monitoring hourly to conserve usage; this repo installs no infinite
watcher or free-allowance probes.

## 10. Updates, staging, rollback and recovery

Never start a second relay on production :8787. Build/test first, check
listeners, then stage on :18789 with a fresh private data directory and relevant
saved upstream settings. Don't reuse :18787 (Morph). The retired :18790 helper
must not listen.

Back up the current binary and private config/units with restricted permissions.
After staging passes:

```bash
systemctl --user stop opencode-cc
install -m 755 /tmp/opencode-cc-stage "$HOME/.local/bin/opencode-cc.new"
mv -f "$HOME/.local/bin/opencode-cc.new" "$HOME/.local/bin/opencode-cc"
systemctl --user start opencode-cc
curl --noproxy '*' -fsS http://127.0.0.1:8787/api/health
```

Atomic replacement is mandatory: Morph also executes that same binary on the
reference machine; overwriting its running inode fails with `Text file busy`.
Do not stop Morph. The Bun sidecar is embedded and restarts with the relay; there
is no separate helper release to stage.

To disable the native free lane, remove Bun from the discovery paths (or point
`OPENCODE_CC_BUN` at a missing path) and restart the relay; free models then use
the translated paid transport, which the free tier may reject. This does not
guarantee the exact free model remains compatible.

To roll back, atomically restore the previous relay binary and config. Disable
only this optional WARP consumer with `warp-cli disconnect`; stop only the
isolated phone-egress unit to remove that proxy. Keep ordinary Tailscale and
other services intact.

Recovery backups need source + patch bundle + pinned versions/download checksums,
service units, private config and encrypted irreplaceable credentials/state.
Re-download runtimes and reinstall dependencies; don't publish secrets, node
identities, databases, caches or machine-specific binary backups.

## 11. Diagnose the layer that failed

| Symptom | Check / meaning |
|---|---|
| Connection refused | Exact listener, unit status, port conflict |
| Health 200, inference fails | Health proves listener only; test a real exact-model request |
| 401 / expired OAuth | Correct credential identity; official CLI refresh; don't copy legacy tokens |
| 403 FreeTierError | Client/model eligibility; key health alone is insufficient |
| 429 on a tool task | Usually the shape gate, not quota: verify the exact system prompt, all 13 tools, `User-Agent: opencode/latest/2.2.7/cli`, `X-Opencode-Client: cli`; `x-oc-reset: shape` confirms |
| 429 with `x-oc-reset: failed` | Reset failed or the header budget expired; the lane cycles keys/resets, so wait or cancel |
| Immediate repeated CPA cooldown | Correct patched CPA + hold enabled for this group; never clear all providers' cooldowns |
| WARP connected, HTTPS timeout | Proxied trace, tunnel logs and connectivity; no claim of healthy inference |
| Phone proxy responds but Internet fails | Verify HTTPS through :18889, phone data, tailnet/exit permissions |
| Telegram sends but no reset | Exact authorized sender/chat/text, phone automation and OS permissions |
| HTTP 200 then truncated stream | Read stream to completion; explicit error envelope is a failed generation |
| Wrong upstream despite env edit | Enabled saved upstream pool takes precedence |
| Missing UI | Build Vite and copy web/dist to internal/assets/dist **before** go build |
| Tool name mismatch (`shell` vs `Bash`) | Non-streaming mapping or the sidecar's declared-name resolution; look for the `resolved N upstream tool call(s)` log line |

Use `journalctl --user -u opencode-cc --since '10 min ago'` locally; sanitize
before sharing. Diagnostic probes under `scripts/probe_zen_*` are historical
opt-in investigations with local path assumptions. Some replay private fixtures,
consume model allowance or require the legacy auth file; read their `--help` and
source first. They are not the installation procedure. Never feed the whole
historical investigation into configuration as if every old hypothesis were
current: the deployed summary supersedes older failures.
