# Azure Container Apps dynamic sessions

| | |
|---|---|
| Status | **Tested with a fake server** for Python code-interpreter pools (implemented from the published API, not run against Azure). Custom container and shell pools: **Not supported** by this adapter (see [below](#custom-container-and-shell-pools)) |
| Adapter | `azure-sessions` (`internal/adapters/azuresessions`) |
| Trust | declared: every pool must state it. Microsoft manages the sessions (`external`), but they run in your own subscription, so declare `private` if your policy treats that subscription as private |
| Isolation | Hyper-V isolation per session |
| API checked | Data-plane spec `2025-10-02-preview` in [azure-rest-api-specs](https://github.com/Azure/azure-rest-api-specs) (`specification/app/data-plane/DynamicSessions`), [sessions](https://learn.microsoft.com/en-us/azure/container-apps/sessions), [code interpreter sessions](https://learn.microsoft.com/en-us/azure/container-apps/sessions-code-interpreter) and [session pools](https://learn.microsoft.com/en-us/azure/container-apps/session-pool), read 2026-10-28 |

Each Differently sandbox is one session in a Python code-interpreter session
pool. Azure allocates a session the first time an identifier is used. For
general-purpose sandboxes on Azure, prefer
[Azure Container Apps Sandboxes](azure-sandboxes.md).

## Capabilities

| Feature | Supported | Notes |
|---|---|---|
| exec | yes | The pool runs Python, so the command runs through Python's `subprocess` (`/bin/sh -c`), and the exit code comes back on the first line of stderr. Microsoft does not document whether `subprocess` is allowed; check before relying on it |
| exec.stream | no | Buffered fallback |
| files | yes, under `/mnt/data` only | The session files API (upload up to 128 MB); other paths are `invalid_request` |
| stdin | yes | Passed to `subprocess` |
| lifecycle.stop-start | no | |
| lifecycle.pause-resume | no | |
| snapshots.disk | no | |
| snapshots.memory | no | |
| fork | no | |
| ports.expose | no | |
| network.egress-control | no | Set on the session pool, not per sandbox |
| max size | fixed by the pool (not reported) | Requested resources are not applied |
| provider TTL backstop | the pool's cooldown | A session ends after `cooldownPeriodInSeconds` (300–3600) without requests; there is no per-session TTL |

## Configure

Create a session pool with container type `PythonLTS`. Give a service
principal the **Azure ContainerApps Session Executor** role on the pool. The
credential is the principal as JSON (client secret or federated token file).

```json
{"tenantId": "...", "clientId": "...", "clientSecret": "..."}
```

```sh
differently admin secret put azure-sessions-sp --from-file ./azure-sp.json
differently admin pool add azure-sessions --adapter azure-sessions --trust external \
  --config endpoint=https://eastus.dynamicsessions.io/subscriptions/<sub>/resourceGroups/<rg>/sessionPools/<pool> \
  --max-concurrency 300 --secret azure-sessions-sp
```

```yaml
pools:
  - name: azure-sessions
    adapter: azure-sessions
    trust: external                # required: external, or private if your policy allows
    region: eastus
    maxConcurrency: 300            # keep within one 300-session list page; see below
    config:
      endpoint: https://eastus.dynamicsessions.io/subscriptions/.../sessionPools/differently
      # apiVersion: 2025-10-02-preview
      # outputLimit: "1048576"     # characters of stdout and of stderr per command
    credentials: {secret: azure-sessions-sp}
```

| Key | Default | Meaning |
|---|---|---|
| `endpoint` | required | The pool's `poolManagementEndpoint` |
| `apiVersion` | `2025-10-02-preview` | Data-plane API version (there is no GA version) |
| `outputLimit` | `1048576` | `outputStreamsMaxLength`; the service default is 4096 characters. Output at the limit is reported as truncated |

## Limits and differences

- **Preview only.** No data-plane version is GA, and the Learn pages and the
  spec disagree in places: `listSessions` against `.management/listSessions`,
  `expireAt` against `expiresAt`, lowercase or PascalCase enum values. The
  adapter follows the spec.
- **Commands run at most 210 seconds.** One code execution is limited to
  220 seconds; the command is stopped at 210 s (exit 124) so its result
  still comes back.
- **The session identifier** is `dly-<installation>--<sandbox>`, with each
  ID's underscore written as a hyphen. It relies on Differently's
  `prefix_<ULID>` IDs.
- **List** pages through `listSessions` and keeps this installation's prefix.
  The pages are skip-based over a set that changes as sessions end, so with
  more than one page (300 sessions) a session ending during the read can
  hide another. The reconciler would then fail that sandbox. Use a pool
  dedicated to Differently and keep `maxConcurrency` at 300 or less (a pool
  allows up to 600 sessions).
- **Sessions end after the pool's cooldown** without requests, and the
  reconciler then marks the sandbox failed. Set the cooldown at least as long
  as your sandboxes may sit idle.
- The sandbox environment is kept in `/tmp/.differently/env.json` in the
  session and merged into every command.
- **Errors**: `SessionWithIdentifierNotFound` (sent as 400) is `not_found`; 429
  is `insufficient_capacity`; a failed execution or a program the
  interpreter killed is `sandbox_failed`. Microsoft does not document the
  throttling or capacity responses.
- **Pricing**: per allocated session, in one-hour increments.

## Custom container and shell pools

**Not supported by this adapter.**

- **Custom container pools** forward every request to your container's
  port. There is no exec, files or exit-code API of Azure's own, so
  Differently would need a protocol your image implements. The fitting route
  is a `remote` plugin that runs inside the image and speaks Differently's
  adapter HTTP protocol, or a sandbox in
  [Azure Container Apps Sandboxes](azure-sandboxes.md) instead.
- **Shell pools** (`containerType: Shell`) are preview-only in the ARM API,
  and the execution result has no exit-code field (the spec's example puts
  `"0"` in `status`).

## How it was tested

`go test ./internal/adapters/azuresessions/` runs the adapter against a fake
pool built from the spec. The fake includes an Entra ID token endpoint. It
allocates sessions on first use and runs each execution's Python program with
the local `python3`, so the exec wrapper really runs. The tests cover:

- create and a replayed create, Get;
- exec with env, stdin and a non-zero exit, and the execution limits sent;
- files under `/mnt/data`, refusal of other paths;
- paged List per installation, idempotent Delete, `SessionWithIdentifierNotFound`;
- 429 and a killed program.

`go test ./internal/adapters/pyshell/` runs the wrapper with timeouts and
missing binaries. Nothing has run against a real session pool.
