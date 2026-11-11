# Same-account credential pool

OpenCode API keys can expire independently. OpenCode-CC accepts more than one
key for the **same account** and fails over only when the upstream rejects a
credential. It never rotates keys on `429` / `FreeUsageLimitError`; that is a
quota signal, not an expired key.

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

All entries must point at the same upstream base. Keys from different
accounts should run in separate opencode-cc instances, not one pool.

## Behaviour

- **Selection:** requests are sticky per session, so prompt caches stay warm.
  If the sticky key is known bad, the next healthy key is used.
- **Failover:** a `401`/`403` that looks credential-related marks the key bad
  for five minutes and replays the same request body with the next key. A
  `429` is returned immediately and never triggers failover.
- **Free models:** the Go layer passes the selected key to the Bun free-tier
  sidecar on the authenticated loopback leg; the sidecar uses it instead of
  re-reading the local OpenCode database.
- **Identity:** the captured request shape belongs to the account, not to one
  key. Rotating a key does not force a recapture or discard the account's
  session lease. If the account itself changes, a `403 workspace access
  denied` triggers one recapture and retry.
- **All keys fail:** the last upstream error is returned unchanged. Do not
  configure an expired key ahead of a live one expecting quota sharing; use
  the provider's published limits.

## Verification

```bash
go test ./internal/config ./internal/server -run 'Credential|Failover|Upstream'
```

The tests cover expired-key failover, 429 non-failover, dead-key skipping, and
`env:`/`file:` resolution.
