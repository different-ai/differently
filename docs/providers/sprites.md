# Sprites (Fly.io)

| | |
|---|---|
| Status | **Documented**: Sprites has a public API, but no adapter yet. Exec, the one operation every pool needs, is only specified over WebSocket; the HTTP form documents no response body |
| Adapter | none (a `remote` plugin, or a future in-process `sprites` adapter) |
| Trust | external (provider-managed) |
| Isolation | "Hardware-isolated" dedicated microVM per sprite (Fly.io's wording; the hypervisor is not named) |
| API checked | [Sprites API](https://docs.fly.io/sprites/api) with its [OpenAPI document](https://api.sprites.dev/openapi.json) (v0.1.12) and [AsyncAPI document](https://docs.fly.io/sprites/api/asyncapi.json); [HTTP exec](https://docs.fly.io/sprites/api/exec/execute-command); [lifecycle](https://docs.fly.io/sprites/concepts/lifecycle), [networking](https://docs.fly.io/sprites/concepts/networking), [sprites.dev](https://sprites.dev/) for pricing. Read 2026-10-28 |

Sprites are persistent Linux microVMs with a disk that always survives. A
sprite goes *warm* (suspended with its memory) about 30 s after it is idle
and *cold* (memory dropped) later, and wakes on the next request. This fits
Differently's model well; the gap is the exec protocol.

## Why there is no adapter yet

- **Exec.** `POST /v1/sprites/{name}/exec` (non-TTY) documents its query
  parameters and a stdin body up to 10 MiB, but its `200` response has no
  schema: there is no documented way to read the exit code or to tell stdout
  from stderr. It also takes env as `env=KEY=VALUE` query parameters, which
  would put sandbox secrets in URLs. The documented exec is the WebSocket at
  `wss://api.sprites.dev/v1/sprites/{name}/exec`, with binary frames
  prefixed by stream (`0x01` stdout, `0x02` stderr, `0x03` exit code) and
  JSON control messages. The Go standard library has no WebSocket client
  and Differently does not depend on one; this pass does not add one.
- **Idempotent create.** Names are caller-chosen and "unique … within the
  organization", but the API documents no conflict response for a
  duplicate name.
- **Ownership.** `labels` appear in responses, but the create request has no
  labels field and the list has no label filter. A prefix filter exists, so
  ownership would be carried in the name.

## Capabilities

What the API documents, and how an adapter would map it:

| Feature | Provider support | Notes |
|---|---|---|
| `exec` | yes (WebSocket) | Exit code arrives in a `0x03` frame or an `exit` message; commands keep running after a disconnect |
| `exec.stream` | yes (WebSocket) | Frames arrive as produced |
| `files` | yes | `GET fs/read`, `PUT fs/write?mkdir=true` (≤ 16 MiB), `GET fs/list` with name, size, mode, mtime and a directory flag |
| stdin | yes | `0x00` frames and `0x04` for end of input |
| `lifecycle.stop-start` | not as API calls | Sprites sleep and wake on their own; the disk always persists |
| `lifecycle.pause-resume` | implicit | A warm sprite keeps its memory, but the platform decides when it goes cold |
| `snapshots.disk` | yes | Checkpoints: `POST …/checkpoint`, `GET …/checkpoints`, `POST …/checkpoints/{id}/restore` |
| `snapshots.memory` | no | Checkpoints are filesystem snapshots |
| `fork` | not documented | A checkpoint restores into the same sprite; restoring it into a new sprite is not documented |
| `ports.expose` | one URL per sprite | `https://<name>-<org>.sprites.app`, routed to port 8080 (or the first HTTP port opened); auth `sprite` (organization members) or `public`. Other ports only through the `/proxy` WebSocket |
| max size | 8 vCPU, 100 GB storage | Memory is managed by the platform, not requested |
| provider TTL backstop | no | Idle sprites cost only storage, so a lost control plane leaves storage charges, not compute |

## Configure

There is nothing to configure until an adapter or plugin exists. A
`remote` plugin built on the Sprites API would be registered like any
plugin, with the Sprites token held by the plugin and Differently holding
only the plugin's own token:

```yaml
pools:
  - name: sprites
    adapter: remote
    trust: external
    config:
      endpoint: https://sprites-adapter.internal:8443
    credentials: {secret: sprites-adapter-token}
```

```sh
differently admin secret put sprites-adapter-token --from-file ./plugin-token.txt
```

Plugin shape (the [adapter protocol](../adapters/specification.md#7-http-protocol)):

- `POST /instances`: look up the sprite named `dly-<installation>-<sandbox
  id>` (lowercase) with `GET /v1/sprites/{name}` and create it only on 404.
  The API documents no duplicate-name response, so the plugin should
  serialize creates per name. The API does not state a name length limit;
  check it before relying on this naming. Write sandbox env to a 0600 file
  through `fs/write`.
- `GET /instances?installation=`: `GET /v1/sprites?prefix=dly-<installation>-`
  with `continuation_token` paging; recover the sandbox ID from the name.
- `exec` and `exec?stream=1`: the exec WebSocket, mapping frames to
  `{stream, data}` and the exit frame to `{exitCode}`.
- `files`: `fs/read`, `fs/write`, `fs/list`.
- `snapshots`: checkpoints; declare `snapshots.disk` only, not `fork`.
- `ports`: the sprite URL for its HTTP port only.
- `stop`/`start`/`pause`: answer `unsupported_operation`; sprites sleep on
  their own.

## Limits and differences

- Organization limits on running and warm sprites (`running_limit`,
  `warm_limit` in the list response).
- Waking a warm sprite takes 100–500 ms and a cold one 1–2 s; TCP connections
  drop whenever it pauses.
- The 429 bodies (`sprite_creation_rate_limited`,
  `concurrent_sprite_limit_exceeded`) appear only in the Go SDK, not in the
  OpenAPI document, which lists 400/401/403/404/413/422/5xx.
- Pricing: $0.0385 per CPU-hour, $0.021875 per GB-hour of memory and
  $0.000683 per GB-hour of hot storage, metered only while active; an idle
  sprite pays for storage.

## How it was tested

Not tested: no adapter exists. The facts above come from the OpenAPI and
AsyncAPI documents and the guides listed in the header.
