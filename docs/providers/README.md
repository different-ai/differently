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

26 providers are covered: 16 with an adapter in this repository, 8 reachable
through a [remote plugin](remote.md), and 2 that cannot be supported.

### What the server contains

The published image, the release binaries and `make build` contain three
provider adapters, each verified against a real backend: `opensandbox`,
`daytona` and `freestyle`. They also contain [`remote`](remote.md), the
plugin protocol, and the dev-mode `devhost`.

The other 13 adapters are **experimental**. Their code and tests are in the
repository and CI builds and tests them, but they are compiled into the
server only with the Go build tag `experimental_adapters`. Twelve have been
tested only against fake servers; `agent-sandbox` also ran live once, on a
kind cluster. A server built without the tag refuses a pool of one of these
kinds, in the config file and through the API, with an error that names the
tag.

To use an experimental adapter, build the server yourself:

```sh
go build -tags experimental_adapters ./cmd/differently-server
# or the image:
docker build -f build/server.Dockerfile --build-arg GO_TAGS=experimental_adapters \
  -t registry.example.com/differently-server:experimental .
```

With the Helm chart, point `server.image` at that image and set
`server.experimentalAdapters=true`; otherwise the chart refuses pools of
these kinds. The dashboard and the console offer only the default kinds, so
add an experimental pool in the config file, with
`differently admin pool add` or with `POST /v1/admin/pools`.

### In the default build

| Provider | Status | Trust |
|---|---|---|
| [OpenSandbox](opensandbox.md) `opensandbox` | **Verified live**: Docker and Kubernetes runtimes, 2026-10-29 | declared |
| [Daytona](daytona.md) `daytona` | **Verified live**: container class, 2026-10-29 (VM class not run) | external |
| [Freestyle](freestyle.md) `freestyle` | **Verified live**: default snapshot, 2026-10-29 | external |

### Experimental: build tag `experimental_adapters`

| Provider | Status | Trust |
|---|---|---|
| [agent-sandbox](agent-sandbox.md) `agent-sandbox` | **Verified live**: kind cluster, runc, 2026-10-28 | declared |
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
start from one. "Build" is `default` for the adapters in every server and
`tag` for the experimental ones.

| Provider | Build | Isolation | Keeps state | Snapshots | Port URLs |
|---|---|---|---|---|---|
| OpenSandbox | default | container or microVM | stop/start or pause, by runtime | disk + fork | signed |
| agent-sandbox | tag | RuntimeClass | stop/start (volume) | — | in-cluster |
| Daytona | default | container or VM | stop/start; pause on VMs | disk + fork (memory on VMs) | signed |
| Freestyle | default | microVM | stop/start, pause | memory + fork | public |
| E2B | tag | microVM | pause | memory + fork | public, opt-in |
| Boat | tag | VM | stop/start | disk + fork | token or public |
| Morph Cloud | tag | VM | pause | memory + fork | public |
| Vercel Sandbox | tag | microVM | stop/start | disk + fork | public |
| Fly.io Machines | tag | microVM | stop/start, suspend | — | — |
| Blaxel | tag | microVM | auto standby | — | token |
| CubeSandbox | tag | microVM | pause | memory + fork | opt-in |
| AWS AgentCore | tag | microVM | — | — | — |
| Azure Sandboxes | tag | managed | stop/start | disk + fork | public |
| Azure sessions | tag | Hyper-V | — | — | — |
| Google Cloud sandboxes | tag | container | stop/start | memory + fork | — |
| Northflank | tag | microVM or gVisor | — | — | public |

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
2. Register it. A new adapter starts as experimental, behind the build tag:
   - `//go:build experimental_adapters` as the first line of every file in
     the package, tests included;
   - the factory in `internal/wiring/wiring_experimental.go`;
   - `ExperimentalAdapters` in `internal/config/config.go`, and **exactly
     one** trust list there: `externalKinds` for a managed provider (its
     pools are always `external`), or `declaredTrustKinds` for a
     self-hostable runtime (its pools must say who operates it).
     `TestEveryAdapterKindHasATrustRule` fails until you choose, so a new
     kind can never be treated as private by omission, and
     `TestRegisteredKindsMatchConfig` fails if the factory and the list
     disagree;
   - the adapter enum in `deploy/helm/differently/values.schema.json` and
     the experimental list in the chart's `differently.validate` helper
     (`templates/_helpers.tpl`).

   `make experimental` builds, vets and tests with the tag; `make verify`
   and CI run it. Adapters written in parallel all edit these lists, so
   their PRs conflict with each other. The resolution is always the union
   of both sides.

   Moving an adapter into the default build is a separate change and a
   maintainers' decision; it needs **Verified live** status first. Drop the
   build tag, move the factory to `internal/wiring/wiring.go` and the kind to
   `DefaultAdapters`, and add it to the `--adapter` help in
   `cmd/differently/main.go`, the `kinds` table in
   `internal/dashboard/static/app.js` and `console/src/lib/providers.ts`.
3. Write `docs/providers/<kind>.md` from the template below, and add a row
   here.
4. When you have an account or a running install, run the adapter's live
   test (`go test -tags live,experimental_adapters`; `daytona` and
   `freestyle` have one to follow) and drive a pool on it through a Differently server, or run the
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
| Adapter | `<kind>` (`internal/adapters/<kind>`), or none; say if it is experimental (build tag) |
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
