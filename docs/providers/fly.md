# Fly.io Machines

| | |
|---|---|
| Status | **Tested with a fake server**: implemented from the Machines API OpenAPI document, not run against a Fly.io account |
| Adapter | `fly` (`internal/adapters/fly`) |
| Trust | external (provider-managed; forced by the server) |
| Isolation | Firecracker microVM per machine, operated by Fly.io |
| API checked | [Machines API OpenAPI](https://docs.machines.dev/openapi.json) (the old `/spec/openapi3.json` path is gone); [Machines resource](https://docs.fly.io/machines/api/machines-resource), [working with the API](https://docs.fly.io/machines/api/working-with-machines-api) (rate limits), [suspend and resume](https://docs.fly.io/reference/suspend-resume), [machine sizing](https://docs.fly.io/machines/guides-examples/machine-sizing), [`persist_rootfs`](https://docs.fly.io/reference/configuration#persist_rootfs), [pricing](https://docs.fly.io/about/pricing). Read 2026-10-28 |

Each sandbox is one machine in a Fly app the operator creates for the pool.
The machine is named after the sandbox ID (lowercase, `_` → `-`) and carries
`dly-sandbox` and `dly-installation` in its metadata. Sprites, Fly.io's
separate sandbox product, has [its own page](sprites.md).

## Capabilities

| Feature | Supported | Notes |
|---|---|---|
| `exec` | yes | `POST /v1/apps/{app}/machines/{id}/exec` through `sh -c`; separate stdout and stderr; a signal is reported as exit code 128 + signal. The timeout is sent in seconds, as Fly's own Go client does; the API reference does not state the unit or a maximum |
| `exec.stream` | no | The API has no streamed exec; streamed requests get one buffered event per stream |
| `files` | yes, through exec | No file API exists. Read runs `base64`, write pipes base64 through stdin, list runs `stat -c` (GNU and BusyBox both work). The image needs `sh`, `base64` and `stat` |
| stdin | yes | Sent in the exec body as a string |
| `lifecycle.stop-start` | yes | `stop` / `start`, polled until settled. Machines are created with `rootfs.persist: always`, so the disk survives |
| `lifecycle.pause-resume` | only with `config.suspend: "true"` | `suspend` keeps memory, but Fly.io does not guarantee the resume: it may cold-boot (the disk is kept either way). Suspend wants ≤ 2 GB of memory and no swap |
| `snapshots.disk` | no | Fly snapshots volumes, not a machine's root filesystem |
| `snapshots.memory` | no | The suspend snapshot cannot be named or restored elsewhere |
| `fork` | no | |
| `ports.expose` | no | A Fly app has one public hostname (`<app>.fly.dev`) shared by all its machines; reaching one machine needs the `fly-force-instance-id` request header, which a URL cannot carry, so every sandbox would share every port |
| max size | 8 vCPU / 16 GiB (`shared`), 16 vCPU / 128 GiB (`performance`) | Requests are rounded up to Fly's sizes: 1, 2, 4, 6 or 8 shared CPUs with 256 MB to 2 GB each; 1, 2, 4, 8 or 16 performance CPUs with 2 GB to 8 GB each; memory in 256 MB steps. `diskGiB` sets `rootfs.size_gb` |
| provider TTL backstop | no | Machines have no deadline. If the control plane is lost, machines keep running (and billing) until deleted |

## Configure

Create the app once (`fly apps create dly-sandboxes`, or
`POST /v1/apps`), then store an app-scoped deploy token
(`fly tokens create deploy -a dly-sandboxes`) and register the pool:

```sh
differently admin secret put fly-token --from-file ./fly-token.txt
differently admin pool add fly --adapter fly --trust external \
  --config app=dly-sandboxes --config region=iad --config image=ubuntu:24.04 \
  --region iad --max-concurrency 20 --secret fly-token
```

Or in the server config file:

```yaml
pools:
  - name: fly
    adapter: fly
    trust: external
    region: iad
    maxConcurrency: 20
    config:
      app: dly-sandboxes        # required: the Fly app that holds the machines
      region: iad               # default: Fly picks one near the caller
      image: ubuntu:24.04       # required unless every request names a template (an image)
      cpuKind: shared           # shared (default) or performance
      command: ""               # replaces the default idle process
      suspend: "false"          # "true" advertises pause and resume
      apiURL: https://api.machines.dev
    credentials: {secret: fly-token}
```

The Helm chart's `config.pools` accepts `credentials: {env: FLY_API_TOKEN}`
with the variable supplied through `server.extraEnv` from a Kubernetes
Secret; stored `secret:` credentials are registered with the CLI or API as
above.

## Limits and differences

- The machine runs an idle loop (`trap 'exit 0' TERM INT; while :; do sleep
  3600 & wait $!; done` under `/bin/sh -c`) instead of the image's command,
  so it stays up for exec. Set
  `config.command` to run something else; if that process exits, the machine
  stops (restart policy `no`).
- Sandbox env is set as the machine's env and also written to
  `/etc/differently/env` (mode 0600) when the machine is created; every exec
  sources it, because exec has no env field.
- Files travel as base64 inside exec requests and responses, so very large
  files are bounded by limits Fly.io does not document.
- Rate limits: one request per second per action per machine (burst 3), five
  per second for Get Machine. The adapter polls with Get Machine once a
  second.
- Capacity: a full region answers `status: insufficient_capacity`, and a
  full host answers 409 with "insufficient … available to fulfill request".
  Both become `insufficient_capacity`. A bad image (422), or a machine name
  already held by a machine Differently did not create, is
  `invalid_request`; a machine that ends `failed` is `sandbox_failed`; 401,
  403 and 5xx are `provider_unavailable`.
- Machines are not restored from snapshots, so `fromSnapshot` is refused
  with `invalid_request`.
- Pricing: started machines are billed per second by CPU kind and memory
  (from $2.19 per 30 days for `shared-cpu-1x` with 256 MB in `iad`); stopped
  and suspended machines pay $0.15 per GB-month of root filesystem.

## How it was tested

`go test ./internal/adapters/fly/` runs the adapter against a fake server
built from the OpenAPI document: create (name, region, guest sizing, rootfs,
init, env file); idempotent create, a replay after the machine stopped, and a
lost name race; exec with cwd, env, stdin, timeout and a signal; files
(binary round trip, missing file, listing, missing directory); stop and
start; suspend and resume with `config.suspend`; metadata-filtered `List`
across short and empty pages; idempotent forced delete; and error
classification (no image, bad image, full region, full host, failed
machine, 5xx, rejected token). Nothing has run against `api.machines.dev`:
no Fly.io token was available.
