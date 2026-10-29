# Provider catalog

Differently runs no sandboxes itself. It places them on **providers**:
runtimes you operate and managed services you opt into. Each provider is
reached through an adapter, and each pool you register names one adapter.
This page lists every provider, how well it has been tested, and how to add
a new one.

## Status legend

| Status | Meaning |
|---|---|
| **Verified live** | Ran against the real provider through Differently; the page says when and what ran |
| **Tested with a fake server** | Implemented from the provider's published API and covered by unit tests against a fake of it; not yet run against the provider |
| **Documented** | Configuration described, no built-in adapter yet; reach it with a [remote plugin](remote.md) |
| **Not supported** | The provider's API cannot meet the adapter contract; the page says why |

Only **Verified live** means it has run. Everything else is a claim about
code and published documentation.

## Providers

26 providers are covered: 16 with a built-in adapter, 8 reachable through a
[remote plugin](remote.md), and 2 that cannot be supported. Four of the 16
have run live; the rest are implemented from published APIs and tested
against fakes of them. "Persists" means stop/start keeps the disk or pause
keeps memory; "Snapshots" are disk unless marked memory.

### Built-in adapters

| Provider | Status | Trust |
|---|---|---|
| [OpenSandbox](opensandbox.md) `opensandbox` | **Verified live**: Docker and Kubernetes runtimes, 2026-10-29 | declared |
| [agent-sandbox](agent-sandbox.md) `agent-sandbox` | **Verified live**: kind cluster, runc, 2026-10-28 | declared |
| [Daytona](daytona.md) `daytona` | **Verified live**: container class, 2026-10-29 (VM class not run) | external |
| [Freestyle](freestyle.md) `freestyle` | **Verified live**: default snapshot, 2026-10-29 | external |
| [E2B](e2b.md) `e2b` | Tested with a fake server | declared |
| [Boat](boat.md) `boat` | Tested with a fake server | external |
| [Morph Cloud](morph.md) `morph` | Tested with a fake server | external |
| [Vercel Sandbox](vercel.md) `vercel` | Tested with a fake server | external |
| [Fly.io Machines](fly.md) `fly` | Tested with a fake server | external |
| [Blaxel](blaxel.md) `blaxel` | Tested with a fake server | external |
| [CubeSandbox](cubesandbox.md) `cubesandbox` | Tested with a fake server | declared |
| [AWS AgentCore Code Interpreter](aws-agentcore.md) `aws-agentcore` | Tested with a fake server | declared |
| [Azure Container Apps Sandboxes](azure-sandboxes.md) `azure-sandboxes` | Tested with a fake server | declared |
| [Azure dynamic sessions](azure-sessions.md) `azure-sessions` | Tested with a fake server (Python pools) | declared |
| [Google Cloud Agent Platform sandboxes](gcp-sandboxes.md) `gcp-sandboxes` | Tested with a fake server | declared |
| [Northflank](northflank.md) `northflank` | Tested with a fake server | declared |

**Trust:** *external* pools are always third-party capacity and receive work
only when policy allows it. *Declared* pools must say `private` or
`external`: self-hostable runtimes (OpenSandbox, agent-sandbox, CubeSandbox,
E2B) and cloud services that run in your own account (AWS, Azure, Google
Cloud, Northflank BYOC).

### What each adapter offers

"Keeps state" is stop/start that keeps the disk, or pause that keeps memory.
"Snapshots" is what a snapshot captures; "+ fork" means a new sandbox can
start from one.

| Provider | Isolation | Keeps state | Snapshots | Port URLs |
|---|---|---|---|---|
| OpenSandbox | container or microVM | stop/start or pause, by runtime | disk + fork | signed |
| agent-sandbox | RuntimeClass | stop/start (volume) | — | in-cluster |
| Daytona | container or VM | stop/start; pause on VMs | disk + fork (memory on VMs) | signed |
| Freestyle | microVM | stop/start, pause | memory + fork | public |
| E2B | microVM | pause | memory + fork | public, opt-in |
| Boat | VM | stop/start | disk + fork | token or public |
| Morph Cloud | VM | pause | memory + fork | public |
| Vercel Sandbox | microVM | stop/start | disk + fork | public |
| Fly.io Machines | microVM | stop/start, suspend | — | — |
| Blaxel | microVM | auto standby | — | token |
| CubeSandbox | microVM | pause | memory + fork | opt-in |
| AWS AgentCore | microVM | — | — | — |
| Azure Sandboxes | managed | stop/start | disk + fork | public |
| Azure sessions | Hyper-V | — | — | — |
| Google Cloud sandboxes | container | stop/start | memory + fork | — |
| Northflank | microVM or gVisor | — | — | public |

Every adapter offers exec and files. Streamed exec, stdin, egress control and
size limits differ: each page has the full table.

Two more kinds are not providers: [`remote`](remote.md), for any backend
behind the HTTP adapter protocol (private unless declared), and `devhost`,
which runs commands on the server's own machine with **no isolation**, in
`--dev` mode only.

### Through a remote plugin (documented)

These have no built-in adapter. Each page explains why and what a plugin
would wrap.

| Provider | Why there is no adapter |
|---|---|
| [Modal](modal.md) | Sandboxes have no public REST API; the SDKs speak a gRPC API Modal says is not public |
| [Cloudflare Sandbox SDK and Containers](cloudflare.md) | Containers are reachable only through a Durable Object in a Worker |
| [CodeSandbox](codesandbox.md) | The supported interface is the `@codesandbox/sdk` |
| [Beam](beam.md) | Driven through Beam's SDK; its open-source engine (beta9) can be self-hosted |
| [Koyeb](koyeb.md) | Sandboxes (public preview) are driven through Koyeb's SDK |
| [Sprites](sprites.md) (Fly.io) | Exec is specified only over WebSocket; a built-in adapter is possible later |
| [Amazon Bedrock AgentCore Runtime](aws-agentcore-runtime.md) | Command execution needs a plugin or a future adapter |
| [GKE Agent Sandbox](gke-agent-sandbox.md) | A GKE add-on with no Google HTTP API of its own |

### Not supported

| Provider | Why |
|---|---|
| [Runloop](runloop.md) | The service is winding down; transition support ends 2026-11-13 |
| [microsandbox](microsandbox.md) | Since its 2026 rewrite it has no server API: the runtime is embedded in each SDK process |

## Choose a route for a new provider

1. **Built-in adapter** (`internal/adapters/<kind>`) when the provider has a
   documented HTTP API that fits the [adapter contract](../adapters/specification.md):
   create, get, list by label, delete, exec and files at minimum. Follow
   `daytona` and `freestyle`, which use `internal/adapters/httpx`.
2. **Remote plugin** when the provider is reachable only through an SDK, or
   when you want to keep provider code out of the server. The plugin speaks
   the [HTTP adapter protocol](../adapters/specification.md#7-http-protocol)
   in any language ([remote plugins](remote.md)).
3. **Not supported** when the provider cannot list its sandboxes by a label
   or metadata Differently sets, or cannot make create and delete safe to
   repeat: the reconciler depends on both.

## Add a built-in adapter

1. Write the adapter with unit tests against a fake server built from the
   provider's published API. Cover every feature you advertise, idempotent
   create, `List` for the reconciler, and error classification:
   `sandbox_failed` for one sandbox's failure, `invalid_request` for a bad
   image or request, `insufficient_capacity` for 429 and capacity errors, and
   `provider_unavailable` for outages and rejected credentials. Advertise
   only features that work.
2. Register it:
   - the factory in `internal/wiring/wiring.go`;
   - `KnownAdapters` in `internal/config/config.go`, and **exactly one**
     trust list there: `externalKinds` for a managed provider (its pools are
     always `external`), or `declaredTrustKinds` for a self-hostable runtime
     (its pools must say who operates it). `TestEveryAdapterKindHasATrustRule`
     fails until you choose, so a new kind can never be treated as private
     by omission;
   - the adapter enum in `deploy/helm/differently/values.schema.json`;
   - the `--adapter` help in `cmd/differently/main.go` and the `kinds`
     table in `internal/dashboard/static/app.js`, so operators can add it
     without a config file.
3. Write `docs/providers/<kind>.md` from the template below, and add a row
   here.
4. When you have an account or a running install, run the adapter's live
   test (`go test -tags live`, as `agent-sandbox`, `daytona` and `freestyle`
   have) and drive a pool on it through a Differently server, or run the
   [end-to-end suite](../../test/e2e/README.md) against it where it fits.
   Record what ran on the page. Until then, the status stays
   **Tested with a fake server**.

## Configure any provider

Every pool has the same shape, in the server config or through
`differently admin pool add`, `POST /v1/admin/pools` or the dashboard:

```yaml
pools:
  - name: <pool>
    adapter: <kind>
    trust: external              # or private; required for self-hostable kinds
    region: <region>
    maxConcurrency: 20
    config: {<key>: "<value>"}   # the adapter's keys, all strings
    credentials:
      secret: <pool>-key         # stored sealed in Differently; or {env: VAR} or {file: /path}
```

Store a key with `differently admin secret put <pool>-key --from-file key.txt`.
An `external` pool receives work only when the policy allows external
placement and names it in `externalPools`
([keys and providers](../guides/keys-and-providers.md)). Switch where new
sandboxes go with `differently admin pool use <pool>`.

## Page template

````markdown
# <Provider>

| | |
|---|---|
| Status | one of the four statuses, with the date and what ran |
| Adapter | `<kind>` (`internal/adapters/<kind>`), or none |
| Trust | external (provider-managed) or declared (self-hostable) |
| Isolation | what the provider documents (microVM, gVisor, container…) |
| API checked | docs or OpenAPI URL, and the date read |

## Capabilities
exec · exec.stream · files · stdin · lifecycle.stop-start ·
lifecycle.pause-resume · snapshots.disk · snapshots.memory · fork ·
ports.expose · network.egress-control · max size · provider TTL backstop:
each yes, no, or a note.

## Configure
The pool, its config keys with defaults, and how to store the key.

## Limits and differences
What doesn't map, quotas, regions, pricing model in one line.

## How it was tested
What ran, where and when, and what didn't.
````
