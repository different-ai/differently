# Northflank

| | |
|---|---|
| Status | **Tested with a fake server** (implemented from the published API, not run against Northflank) |
| Adapter | `northflank` (`internal/adapters/northflank`) |
| Trust | declared: every pool must state it. Northflank manages the workload (`external`); with BYOC it runs in a cluster in your own cloud account, so declare `private` if your policy treats that as private |
| Isolation | microVMs for CPU workloads (Cloud Hypervisor, per the product page) and gVisor for GPU workloads; on BYOC, the cluster's runtime class (`kata-clh`, `kata-qemu`, `gvisor` or none) |
| API checked | [Northflank API](https://northflank.com/docs/v1/api/use-the-api), [sandboxes quickstart](https://northflank.com/docs/v1/application/sandboxes/quickstart), [execute command](https://northflank.com/docs/v1/api/execute-command), the OpenAPI document at `api.northflank.com/v1/swagger-json`, and `@northflank/js-client` 0.12.0, read 2026-10-28 |

Northflank has no separate sandbox resource: a sandbox is a deployment
service that keeps a long-running process. Each Differently sandbox is one
service whose container runs `sleep infinity`.

## Capabilities

| Feature | Supported | Notes |
|---|---|---|
| exec | yes | Over Northflank's command-exec WebSocket, the only exec API. The working directory, stdin and timeout are added to the command line; the timeout uses coreutils `timeout` (exit 124) |
| exec.stream | yes | `stdOut` and `stdErr` messages are passed on as they arrive |
| files | yes | Through commands (`base64` and `find`), in 64 KiB pieces. Northflank has no file API either |
| stdin | yes | Piped in through the command line |
| lifecycle.stop-start | no | Northflank's pause scales to zero and wipes ephemeral storage |
| lifecycle.pause-resume | no | |
| snapshots.disk / snapshots.memory | no | Container snapshots exist but are new, Kata-only and barely documented |
| fork | no | |
| ports.expose | yes | A public HTTP port on `<port name>--<service>--<random>.code.run`: **the URL is public**, so anyone who has it can connect |
| network.egress-control | no | Not offered by Northflank's API |
| max size | the pool's plan (default `nf-compute-200`: 2 vCPU, 4 GiB) | Requested resources are not applied; the default quota is 8 vCPU and 16 GiB per instance |
| provider TTL backstop | yes | `createOptions.expiryTime`: the TTL plus 5 minutes (at least 5 minutes). It is set only when that fits in 7 days, and cannot be moved later |

## Configure

The credential is a Northflank API token with permission to manage services
in the project. Organization-scoped tokens also need `teamId`.

```sh
differently admin secret put northflank-token --from-file ./northflank-token.txt
differently admin pool add northflank --adapter northflank --trust external \
  --config projectId=differently-sandboxes --config image=ubuntu:24.04 \
  --max-concurrency 20 --secret northflank-token
```

```yaml
pools:
  - name: northflank
    adapter: northflank
    trust: external                # required: external, or private for a BYOC cluster your policy allows
    maxConcurrency: 20
    config:
      projectId: differently-sandboxes
      # teamId: my-team            # for organization-scoped tokens
      image: ubuntu:24.04          # a request's template overrides it
      plan: nf-compute-200
      # apiURL: https://api.northflank.com
    credentials: {secret: northflank-token}
```

| Key | Default | Meaning |
|---|---|---|
| `projectId` | required | The project the services are created in (its region is fixed) |
| `teamId` | none | Needed with organization-scoped tokens |
| `image` | `ubuntu:24.04` | Container image; it needs `sh`, `sleep infinity`, `base64` and coreutils |
| `plan` | `nf-compute-200` | Deployment plan for every sandbox |
| `apiURL` | `https://api.northflank.com` | API and command-exec host |

## Limits and differences

- **Rate limit: 1,000 requests an hour by default.** Health probes run at most
  every 30 seconds per server replica, and readiness is polled every 2
  seconds. The reconciler lists every 30 seconds. With several replicas or
  many sandboxes, ask Northflank to raise the limit.
- **Service names** are `dly-<8-character installation hash>-<sandbox ULID>`,
  within Northflank's 54 characters. A create that conflicts (409) reads the
  existing service, so replays are safe. List keeps this prefix; the
  description also records both IDs.
- **List** pages through every service of the project (100 a page). Use a
  project dedicated to Differently.
- The sandbox environment is set as the service's runtime environment.
- **Errors**: 429 is `insufficient_capacity`; 409 on create means the service
  exists; 400 (for example an image Northflank cannot verify) is
  `invalid_request`; 401, 403 and 5xx, and a refused exec token, are
  `provider_unavailable`; a failed deployment is `sandbox_failed`. Delete
  treats 404 and 410 as already gone.
- **Exit codes** arrive as a number or a string ("42"); both are read.
- **Ports**: Northflank replaces the whole port list on update, so the
  adapter re-sends the existing ports. The `dns` field it reads is in the
  ports guide but not in the get-service schema.
- **Pricing**: per second, $0.01667 per vCPU-hour and $0.00833 per GB-hour
  on Northflank's cloud; BYOC is billed per node vCPU-hour.

## How it was tested

`go test ./internal/adapters/northflank/` runs the adapter against a fake
server built from the API reference and the command-exec protocol. The fake
implements the WebSocket server side: masked client frames, a fragmented
message, a ping, and an exit code sent as a string. It runs commands with a
local `/bin/sh`. The tests cover:

- create, a replayed create (409), the expiry, Get;
- exec with env and stdin, streamed exec, file write and read;
- adding two ports;
- paged List per installation, idempotent Delete;
- a rejected token (REST and WebSocket), the cached health probe, and 429.

Nothing has run against a real Northflank account.
