# E2B

| | |
|---|---|
| Status | **Tested with a fake server** (2026-10-28): implemented from E2B's published API specs, not run against E2B |
| Adapter | `e2b` (`internal/adapters/e2b`) |
| Trust | declared (self-hostable): `external` for E2B's hosted service; `private` only for an E2B you operate |
| Isolation | Firecracker microVM per sandbox |
| API checked | Control plane: [spec/openapi.yml](https://github.com/e2b-dev/E2B/blob/3b0f0fedde75acb1e6c62f35ff93fe9b812ed2d4/spec/openapi.yml); in-sandbox daemon: [spec/envd](https://github.com/e2b-dev/E2B/tree/3b0f0fedde75acb1e6c62f35ff93fe9b812ed2d4/spec/envd); [docs.e2b.dev](https://docs.e2b.dev/), read 2026-10-28 |

## Capabilities

| Feature | Supported | Notes |
|---|---|---|
| exec | yes | envd `process.Process/Start` (Connect, JSON) running `bash -l -c`; separate stdout and stderr; env and cwd native; timeout kills the process (exit 124) |
| exec.stream | yes | The same call, relayed as it arrives |
| files | yes | envd `GET`/`POST /files` (parent folders created) and `ListDir` |
| stdin | yes | Sent with `SendInput` once the process starts, then closed |
| lifecycle.stop-start | no | E2B has no stop that keeps the sandbox |
| lifecycle.pause-resume | yes | Pause keeps memory and disk; a paused sandbox is not billed |
| snapshots.disk | yes | Captured with memory |
| snapshots.memory | yes | `POST /sandboxes/{id}/snapshots`; a snapshot is a template, restored by creating from it |
| fork | yes | Through a snapshot and a new sandbox from it |
| ports.expose | with `ports: public` | `https://<port>-<id>.<domain>`; every listening port is public |
| network.egress-control | no | `egress: none` turns internet access off for the whole pool; E2B's per-host allow and deny lists are not wired up |
| max size | 8 vCPU, 8 GiB, 20 GiB (plan) | Sizes come from the template; the request's resources are not applied |
| provider TTL backstop | yes | `timeout` (deletes), capped at `maxLifetimeSeconds`; moved with `/timeout` |

## Configure

```yaml
pools:
  - name: e2b
    adapter: e2b
    trust: external          # required: external for E2B's hosted service
    region: us-west-1
    maxConcurrency: 20       # Hobby allows 20 concurrent sandboxes
    config:
      apiURL: https://api.e2b.app   # default: https://api.<domain>
      domain: e2b.app        # default; other regions have their own domain and key
      template: base         # default template (its size is the sandbox's size)
      maxLifetimeSeconds: "3600"    # the plan's maximum runtime: 3600 Hobby, 86400 Pro
      ports: private         # private (default) or public
      egress: public         # public (default) or none
    credentials: {secret: e2b-key}
```

```sh
differently admin secret put e2b-key --from-file ./e2b-api-key.txt
differently admin pool add e2b --adapter e2b --trust external --region us-west-1 \
  --config maxLifetimeSeconds=3600 --max-concurrency 20 --secret e2b-key
```

A pool without `trust` is refused. The Helm chart's pool schema takes
`credentials: {env: E2B_API_KEY}` with the key in `server.extraEnv`, or
register the pool at runtime as above. A `trust: external` pool is not
placed until policy approves external pools
([keys-and-providers.md](../guides/keys-and-providers.md#approve-external-providers-safely)).
The server needs egress to `api.<domain>` and `sandbox.<domain>` on 443.

## Limits and differences

- **Lifetime.** E2B deletes a sandbox when its timeout ends, and the plan
  caps it from each start or resume (1 h Hobby, 24 h Pro). The adapter sends
  the sandbox's TTL, never more than `maxLifetimeSeconds`; a sandbox with no
  TTL, or a longer one, is deleted at that cap and Differently then reports
  it gone. Set `maxLifetimeSeconds` to the plan's limit: a larger value is
  refused by E2B at create.
- **Sizes.** CPU, memory and disk are fixed by the template at build time
  (the `base` template has 2 vCPU and 512 MiB). The request's resources are
  used for quota and cost only. Build one template per size with E2B's tools
  and point pools or templates at them.
- **Ports.** E2B has no per-port expose. With `ports: private` (default) the
  sandbox refuses public traffic and `ports.expose` is not offered; with
  `ports: public` every listening port is reachable by anyone with the URL.
  This is set at create and cannot change later.
- **Snapshots** pause the source briefly and drop its open connections,
  including running commands' streams. They always include memory.
- **Templates and errors.** An unknown template is `invalid_request`, as is
  one the key may not use (403). 429 and 503 (including
  `sandbox_capacity_unavailable`) requeue as `insufficient_capacity`;
  `sandbox_create_failed` is `sandbox_failed`; 401 and other 5xx are
  `provider_unavailable`. Exec against a paused or unreachable sandbox (502
  from E2B's edge) is `invalid_state`.
- **Regions** are separate deployments with their own domain and API key
  (`e2b.app` is us-west-1). Register one pool per region.
- **Quotas:** Hobby 20 concurrent sandboxes and 1 create a second; Pro 100
  or more and 5 a second. Paused sandboxes do not count.
- **Pricing:** per second while running, per vCPU and per GiB of RAM; disk
  is included; paused sandboxes are free.
- E2B's infrastructure is open source and can be self-hosted. Point
  `apiURL` and `domain` at your install and declare `trust: private` only if
  you operate it. The limits above describe E2B's hosted service; a
  self-hosted install has not been tried.

## How it was tested

`go test ./internal/adapters/e2b/` against a fake control plane and a fake
envd speaking Connect's streaming envelopes (2026-10-28): create with
template, timeout cap, metadata, env, network and egress settings;
idempotent create; exec and streamed exec (cwd, env, exit codes, Connect
errors, timeout, stdin through `SendInput` and `CloseStdin`); files (write
as multipart, read, list, missing paths); pause (twice) and resume with the
refreshed envd token; a second adapter fetching the token itself;
`SetExpiry`; snapshot, restore and delete; port URLs and their absence with
private ports; paged `List` with E2B's metadata filter encoding; idempotent
delete; error classification for 401, 403, 404, 429, 500, 503 and E2B's
error codes. Nothing ran against E2B itself: no account or key was
available.
