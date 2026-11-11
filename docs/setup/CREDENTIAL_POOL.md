# Credential pool

OpenCode API keys can expire independently, and a key that hits its free-tier
limit stops serving until its window refills. OpenCode-CC accepts more than one
key, round-robins them, and fails over when a credential is rejected or a key
runs out.

A pool may hold several keys of one employer account, or keys from several
accounts that the same employer issued. Never mix unrelated third-party
accounts in one pool.

## Configuration

Keep the keys out of JSON. Put them in the service environment file
(`~/.config/opencode-cc/env`, mode `0600`) or one file per key:

```json
{
  "upstreams": [
    {
      "base_url": "https://opencode.ai/inference",
      "api_key": "env:OPENCODE_KEY_1",
      "enabled": true
    },
    {
      "base_url": "https://opencode.ai/inference",
      "api_key": "env:OPENCODE_KEY_2",
      "enabled": true
    },
    {
      "base_url": "https://opencode.ai/inference",
      "api_key": "file:/home/pravin/.config/opencode-cc/keys/key-3",
      "enabled": true
    }
  ]
}
```

`api_key` supports three forms:

| Form | Meaning |
| --- | --- |
| `env:NAME` | Read `NAME` from the service environment. Empty/unset fails closed. |
| `file:/absolute/path` | Read a single key from a private file. Empty/missing fails closed. |
| `opencode://active` | Read the credential currently active in the OpenCode CLI database. |
| any other string | A literal key (supported, but prefer `env:` or `file:`). |

All entries must point at the same upstream base. Keys from several employer
accounts may share one pool: the free-tier sidecar detects a workspace mismatch
from the first `403 Workspace access denied`, drops the foreign account's
org/workspace headers for that key, and gives it its own session-lease lane.
Leases and captured identity stay account-scoped, so keys of one account keep
sharing the good lane.

## Behaviour

- **Selection:** `NextUpstream` advances through configured entries, so
  successive requests and sessions spread over all keys instead of pinning one.
  If the chosen key is known dead or limited, the next usable key is selected.
- **Credential failover:** a `401`/`403` that looks credential-related marks the
  key bad for five minutes and replays the same request body with the next key.
  Quota, billing, rate-limit and permission 403s are **not** credential
  failures: rotating every key of the same account would hit the same wall and
  hide the real error.
- **Quota failover:** a `429` marks the key *limited* for 60 seconds (never
  dead) and retries with the next key. This is the last resort, used only after
  the free-tier sidecar has already attempted its egress reset for that
  request. Free-lane retries carry `x-bridge-no-reset: 1`, so rotating through
  the pool does not cycle the phone's radio once per key.
- **Bounded:** failover skips every key already dead or limited, so a request
  makes at most one attempt per configured key and always returns the real
  upstream error. A limited key becomes usable again automatically when its
  60-second window passes.
- **Free models:** the Go layer passes the selected key to the Bun free-tier
  sidecar on the authenticated loopback leg and scopes selection to the Zen
  base, so a key belonging to another upstream can never be sent to Zen.
- **Identity:** the captured request shape belongs to the account, not to one
  key. Rotating a key does not force a recapture or discard the account's
  session lease. The shape-failure cache **is** key-scoped: a rotated key must
  make a real attempt instead of inheriting the exhausted key's cached `429`.
  If the account itself changes, a `403 workspace access denied` triggers one
  recapture and retry.
- **All keys fail:** the last upstream error is returned unchanged. The pool
  never loops forever and never substitutes the local LFM3 stack.

## Verification

```bash
go test ./internal/config ./internal/server -run 'Credential|Failover|Upstream'
```

The tests cover expired-key failover, 429 rotation and its bound, quota-403
non-rotation, limited-key recovery, large error-body restoration, probe
isolation, and `env:`/`file:` resolution.
