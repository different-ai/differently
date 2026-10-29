# Blaxel

| | |
|---|---|
| Status | **Tested with a fake server** (implemented from Blaxel's published OpenAPI specs, not run against Blaxel) |
| Adapter | `blaxel` (`internal/adapters/blaxel`) |
| Trust | external (provider-managed; Blaxel is hosted-only) |
| Isolation | microVM per sandbox, Blaxel-managed. The docs say "microVMs" without naming the hypervisor; Blaxel's blog says Firecracker |
| API checked | Control plane: [controlplane.yml](https://raw.githubusercontent.com/blaxel-ai/docs/main/api-reference/controlplane.yml). In-sandbox API: [sandbox-api openapi.yml](https://raw.githubusercontent.com/blaxel-ai/sandbox/refs/heads/main/sandbox-api/docs/openapi.yml). Guides at [docs.blaxel.ai](https://docs.blaxel.ai/Sandboxes/Overview). All read 2026-10-28 |

Blaxel splits its API in two. The control plane (`api.blaxel.ai/v0`)
creates, lists and deletes sandboxes. Each sandbox then serves its own API
at its `metadata.url` (`https://sbx-<name>-<workspace>.<region>.bl.run`),
which runs commands and moves files. The adapter uses both, with one API
key.

## Capabilities

| Capability | Support |
|---|---|
| exec | yes; runs through `sh -c` in the sandbox with `env`, `cwd` and `timeoutSeconds` (Blaxel kills the process at the deadline). The adapter reads Blaxel's NDJSON stream, so commands may outlast the 60 s cap on Blaxel's waiting form |
| exec.stream | yes; chunks as the process writes them. A command that finishes before Blaxel streams anything arrives one line per event without the newline; buffered exec uses the final result and is exact |
| files | yes; read, write (parent directory created first) and one-level list. On mk3.0 sandboxes Blaxel's proxy refuses bodies over 8 MiB, reported as `invalid_request` |
| stdin | yes; staged in a temporary file the command reads, then removed. Not interactive |
| lifecycle.stop-start | no; Blaxel has no stop or start call |
| lifecycle.pause-resume | no explicit call. Blaxel puts a sandbox with no connection into standby on its own after about 15 s, keeping memory and processes, and wakes it on the next request in under 25 ms; standby compute is not billed |
| snapshots.disk | no; Blaxel's snapshots are a private preview, marked WIP in its spec |
| snapshots.memory | no (same private preview) |
| fork | no (same private preview) |
| ports.expose | yes: a private preview URL with a preview token valid for one hour in its query string (`?bl_preview_token=…`); anyone holding the URL can reach the port until the token expires |
| network.egress-control | no; Blaxel's spec has network settings, but the adapter doesn't set them |
| Max size | tier-dependent, shown only in Blaxel's console. CPU follows memory (one core per 2048 MB); the root filesystem lives in memory, so `diskGiB` has no effect |
| Provider TTL backstop | `spec.runtime.ttl`, set to the sandbox's TTL and at least 5 minutes (Blaxel's minimum); not moved when the TTL changes later |

## Configure

| Key | Default | Meaning |
|---|---|---|
| `workspace` | required | The Blaxel workspace name, sent as `X-Blaxel-Workspace` |
| `region` | Blaxel picks the closest | `us-pdx-1`, `us-was-1`, `eu-lon-1` or `eu-fra-1` (what your account can use) |
| `image` | `blaxel/base-image:latest` | A Blaxel hub image or a template image from your workspace. A request's `template` overrides it |
| `apiURL` | `https://api.blaxel.ai/v0` | The control plane |

The credential is a Blaxel API key (Console → Profile → Security, or a
service account's key, which reaches only its own workspace).

```sh
differently admin secret put blaxel-key --from-file ./blaxel-api-key.txt
differently admin pool add blaxel --adapter blaxel --trust external \
  --config workspace=my-workspace --config region=us-pdx-1 \
  --region us-pdx-1 --max-concurrency 10 --secret blaxel-key
```

In the server config file:

```yaml
pools:
  - name: blaxel
    adapter: blaxel
    trust: external          # always external; a private claim is overridden
    region: us-pdx-1
    maxConcurrency: 10
    config:
      workspace: my-workspace
      region: us-pdx-1
    credentials: {secret: blaxel-key}
```

With Helm, the chart's `providers.*` block covers Daytona and Freestyle
only. Either register the pool at runtime as above, or add it under
`config.pools` with `credentials: {env: DIFFERENTLY_BLAXEL_API_KEY}` and set
that variable through `server.extraEnv` from a Kubernetes Secret. In both
cases set `networkPolicy.server.egress.runtimeProviders: true`, so the
server can reach `api.blaxel.ai` and the sandboxes' `*.bl.run` hosts on port
443.

Like every external pool, it receives no work until the global policy
approves it ([keys and providers](../guides/keys-and-providers.md)).

## Limits and differences

- **Idempotent create** uses Blaxel's `createIfNotExist=true` with a name
  derived from the sandbox ID (`sbx_01ABC` → `sbx-01abc`); names are unique
  per workspace.
- **Listing** pages through every sandbox in the workspace and filters on
  the adapter's labels (`dly-sandbox`, `dly-installation`), because Blaxel's
  only server-side search is fuzzy. A large shared workspace makes each
  reconcile pass slower; a workspace per installation keeps it small.
- **Error classification:** 429 and `QUOTA_EXCEEDED` → `insufficient_capacity`;
  400 `INVALID_IMAGE` → `invalid_request`; 408 `CREATION_TIMEOUT` and a
  sandbox Blaxel reports `FAILED` → `sandbox_failed`; 5xx and rejected
  credentials → `provider_unavailable`. A create that times out at Blaxel's
  edge (504) is followed by name until it is ready.
- **Idle timeouts:** Differently's `idleTimeoutSeconds` leaves Blaxel
  sandboxes alone, since the pool can neither pause nor stop them. Blaxel's
  own standby already stops compute billing when a sandbox is idle.
- **Restore from a snapshot** is refused with `unsupported_operation`.
- **Sandbox count** is capped per tier (10 on the free tier, 50 and 200 on
  the next two), and TTLs are capped at 7 days on the free tier.
- **Pricing:** active compute is billed per GB of memory per second, CPU
  included; standby compute is free.
- Sandbox environment variables are stored in the sandbox's spec on Blaxel.

## How it was tested

Unit tests in `internal/adapters/blaxel` run against a fake server built
from the published control-plane and sandbox API shapes: create (including
a replayed create and a 504 at the edge), get, paged list filtered by
installation, a list page that fails, delete twice, exec (exit codes, env,
cwd, timeout, staged stdin, a command that cannot start), the NDJSON
stream, file read, write and list, preview URLs, and error classification
for 401, 408, 429, `QUOTA_EXCEEDED`, `INVALID_IMAGE` and 503. Run on
2026-10-28 with `go test -race`.

Not run: anything against a live Blaxel account. Unconfirmed from the docs:
whether the sandbox API needs `X-Blaxel-Workspace` (the adapter sends it),
and the exact error bodies Blaxel returns at its gateway.
