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

| Provider | Adapter | Status | Trust | Isolation | Notes |
|---|---|---|---|---|---|
| [OpenSandbox](opensandbox.md) | `opensandbox` | **Verified live** (Docker and Kubernetes runtimes, 2026-10-29) | declared | containers, or Firecracker microVMs (Fast Sandbox, not verified) | The default private runtime. Stop/start, snapshots, fork, signed port URLs and egress policy on Kubernetes |
| [Daytona](daytona.md) | `daytona` | **Tested with a fake server** | external | containers, or VMs (`class: linux-vm`) | Pause and memory snapshots on the VM class only; no stdin |
| [Freestyle](freestyle.md) | `freestyle` | **Tested with a fake server** | external | microVMs | Memory snapshots and fork; deny-all egress by default; exposed ports are public |
| [Remote plugin](remote.md) | `remote` | **Tested with a fake server** | private unless declared | the plugin's | Any backend behind Differently's HTTP adapter protocol |
| Dev host | `devhost` | **Verified live** (dev mode only) | private | **none** | Runs commands on the server's own machine; `--dev` only, for trying the API |

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
4. When you have an account or a running install, run the
   [end-to-end suite](../../test/e2e/README.md) against a pool on it and
   record what ran on the page. Until then, the status stays
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
