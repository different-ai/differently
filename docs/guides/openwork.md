# OpenWork on Differently

OpenWork's sandbox jobs are written against one provider contract,
[`@openwork/sandbox`](https://github.com/different-ai/openwork/tree/dev/packages/sandbox).
The Differently provider (`@openwork/sandbox-differently`) implements that
contract against this API. OpenWork then holds a single Differently key.
Differently holds the OpenSandbox, Daytona and Freestyle credentials, and
decides by policy where each computer runs.

Status: **documented and unit-tested on both sides; not run end to end**.
Differently's side is covered by its tests and the e2e suite. The OpenWork
adapter passes OpenWork's conformance cases against a fake Differently
server. Nobody has yet driven a Workbot runner through a real Differently
installation.

## What OpenWork needs, and where Differently provides it

| OpenWork contract | Differently | Notes |
|---|---|---|
| `idempotencyKey` names one live instance | `Idempotency-Key` header, plus the label `openwork.sandbox.key` | A key whose sandbox has ended creates a new one. A replay of a live key is reported to OpenWork as `conflict`. |
| `labels`, `find`, `list` | `labels`; `GET /v1/sandboxes?label=k=v` | |
| `image` | a [template](#templates-route-one-image-name-to-every-provider), or a snapshot ID to boot from | |
| `resources` | `resources` (`cpu`, `memoryMiB`, `diskGiB`) | |
| create-time `env` | `env` | |
| `lifecycle.autoDeleteMinutes` | `ttlSeconds`, extended each time the computer resumes | `PATCH ttlSeconds` also moves OpenSandbox's own expiry. |
| `lifecycle.autoStopMinutes`, `autoPauseMinutes` | not provided | Workbot pauses idle computers itself. |
| `run` (exact stdout, stderr, exit code) | `POST /v1/sandboxes/{id}/exec` | Exact on OpenSandbox with execd release-1.2.0 and `lineEndings: kept`. |
| `files` read, write, stat | `files/content` GET and PUT; directory listing | The adapter derives `stat` from the parent listing and applies `mode` with `chmod`. |
| `pause`, `start` | `/pause`, `/resume` (or `/stop`, `/start`) | |
| `snapshots` | `POST /v1/sandboxes/{id}/snapshots`, `DELETE /v1/snapshots/{id}` | A snapshot boots only on the pool that took it. |
| `endpoint(port)` | `POST /v1/sandboxes/{id}/ports` | Depends on the backend (see the [backend matrix](../feature-parity.md#3-backend-capability-matrix)). |
| storage volumes, detached exec | **not yet** | OpenWork Cloud workers (Den) need both, so they stay on their Daytona provider. Workbot computers do not. |

Errors map onto OpenWork's taxonomy by code, never by message:

| Differently code | OpenWork code |
|---|---|
| `not_found` | `not_found` |
| `conflict` | `conflict` |
| `invalid_state`, `unsupported_operation`, `unsupported_capability`, `invalid_request` | `invalid_state` (not retried) |
| `quota_exceeded`, `no_eligible_pool` | `capacity` |
| `rate_limited` | `rate_limited` |
| `unauthenticated`, `permission_denied`, `suspended` | `auth` |
| `provider_unavailable`, HTTP 5xx | `transient` |

## 1. Deploy Differently with the pools OpenWork may use

Follow [deploy/kubernetes.md](../deploy/kubernetes.md). For OpenWork:

- **OpenSandbox (private).** Run its server with `execd_image =
  "opensandbox/execd:release-1.2.0"` or later, and set
  `opensandbox.lineEndings: kept` in the chart. With execd 1.1, the output
  of commands that end without a newline gains one, and OpenWork's exact
  `run` contract fails.
- **Daytona or Freestyle (external, optional).** Enable
  `providers.daytona` or `providers.freestyle` with `existingSecret`. Their
  API keys live in Kubernetes Secrets that only the Differently server reads,
  and never reach OpenWork. See [hybrid-overflow.md](hybrid-overflow.md).
- **Policy.** Registering a provider authorizes nothing. To route work there,
  set `config.policy.external` to `overflow` (only when private pools are
  full) or `allowed`, and list the pools in `config.policy.externalPools`.
  A narrower org or project policy can only tighten this.

### On OpenSandbox's Kubernetes runtime (AKS, k3s)

Observed on a single-node k3s install on Azure (OpenSandbox server,
controller and execd release-1.2.0, BatchSandbox): the end-to-end suite
passes 8 of 8 with pause and snapshots left out. What that means for
OpenWork computers:

- **Exact output.** Its Kubernetes runtime already uses `execd`
  release-1.2.0, so set `opensandbox.lineEndings: kept`.
- **Pause and snapshots need a registry.** On this runtime, OpenSandbox
  pauses by committing the sandbox's filesystem to an OCI image and resumes
  by re-creating it from that image (`controller.snapshot.registry` plus
  push and pull secrets). That is slower than Docker's memory freeze, and
  processes don't survive it. Without a registry, leave
  `lifecycle.pause-resume` and `snapshots.disk` out of `opensandbox.features`.
  Workbot then can't pause idle computers: the pause fails quietly and the
  computer runs until its TTL.
- **Port URLs need OpenSandbox's ingress gateway** (`server.gateway.enabled`),
  so a desktop viewer needs it, or a Daytona or Freestyle pool.
- **First image pull.** Pulling a new image can take longer than
  OpenSandbox's 60-second pod-ready timeout. OpenSandbox then answers the
  create with a 504 but keeps creating the sandbox, and the adapter adopts it
  and keeps waiting. The whole create still has to finish within the
  scheduler's `provisionTimeoutSeconds` (120 s by default), so pre-pull large
  images such as Workbot's computer image on the nodes, or raise that
  setting.

## Templates: route one image name to every provider

Workbot boots a prepared image (`HEADLESS_COMPUTER_SNAPSHOT`), and each
provider names its own copy differently. A Differently template maps one
name to each pool's image, so the same computer can be placed on any of them:

```yaml
config:
  templates:
    - name: workbot-computer
      description: Workbot's Linux computer
      images:
        opensandbox: ghcr.io/example/workbot-computer:2026-10-28 # OpenSandbox image (or a Fast Sandbox template ID)
        daytona: workbot-computer-v12                          # Daytona snapshot name
        freestyle: workbot-computer-v12                         # Freestyle snapshot ID or slug
```

A template with `images` runs only on the pools it lists, and placement
explanations say so. A template name with no configured entry is passed to
the backend unchanged, as before.

## 2. Give OpenWork a project and a key

```bash
differently admin org create openwork --default-project workbot
differently key create workbot-runner --org <org-id> --key-project <project-id>
```

The key holds the default `sandboxes:read`, `sandboxes:write` and
`sandboxes:exec` scopes and works only in that project. Set project quotas
(`maxActiveSandboxes`, CPU, memory) to cap what the runner can hold.

## 3. Turn it on in OpenWork

The Workbot runner selects its computer provider with `HEADLESS_COMPUTER`,
which is off by default. To test Differently on one runner:

```bash
HEADLESS_COMPUTER=differently
DIFFERENTLY_URL=https://differently.example.com
DIFFERENTLY_API_KEY=<the project key>
HEADLESS_COMPUTER_SNAPSHOT=workbot-computer   # the Differently template
```

Switching back is the same variable. Computers already created on the
previous provider are not moved; each runner finds only its own provider's
computers.

## Limits to know

- **Idle deletion** is built from TTL extensions: a computer expires
  `HEADLESS_COMPUTER_KEEP_DAYS` after it was last resumed, not after its last
  command. Differently has no idle-based lifecycle of its own yet.
- **Backstops.** On Daytona and Freestyle, a TTL change does not yet move
  their own deletion deadline ([backlog E0.20](../backlog.md)). Keep their
  TTLs generous or unset there.
- **Desktop viewers** need a pool whose backend exposes ports without the
  backend's API key: Freestyle (public TLS host) or Daytona (signed preview
  URL). OpenSandbox's proxied endpoints are not handed out.
- **Den's Cloud workers** need detached processes and persistent volumes;
  both are in the [backlog](../backlog.md) and Den stays on Daytona until
  then.
