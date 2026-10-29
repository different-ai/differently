# Cloudflare Sandbox SDK and Containers

| | |
|---|---|
| Status | **Documented**: no adapter. Cloudflare has no hosted API that creates a sandbox or runs a command in one; containers are reachable only through a Durable Object in a Worker. Run it as a `remote` plugin: a Worker in the operator's account |
| Adapter | none (a `remote` plugin) |
| Trust | external (provider-managed) |
| Isolation | "Each container instance runs in a Firecracker microVM with its own kernel and network" |
| API checked | [Sandbox SDK](https://developers.cloudflare.com/sandbox/) ([lifetime](https://developers.cloudflare.com/sandbox/concepts/lifetime/), [listing sandboxes](https://developers.cloudflare.com/sandbox/manage/list-sandboxes/), [bridge](https://developers.cloudflare.com/sandbox/sdk/bridge/), [migrating to 1.0](https://developers.cloudflare.com/sandbox/sdk/migrate/api-map/)), [Containers](https://developers.cloudflare.com/containers/) ([architecture](https://developers.cloudflare.com/containers/concepts/architecture/), [limits](https://developers.cloudflare.com/containers/platform/limits/), [pricing](https://developers.cloudflare.com/containers/platform/pricing/)), [Containers REST API](https://developers.cloudflare.com/api/resources/containers/). Read 2026-10-28 |

## Why there is no in-process adapter

- **The Containers REST API is read-only for instances.** It manages
  applications, rollouts, images and registries, and lists instances, but
  cannot create, start, stop or delete one, run a command, or open a shell.
  Cloudflare: "A Container can only be accessed through its Durable Object."
- **The Sandbox SDK is a Workers binding.** `@cloudflare/sandbox` 1.0
  (2026-10-22) has you write the Durable Object and call its container
  directly; 0.x (`getSandbox`) is kept for existing apps.
- **The HTTP bridge does not fit the contract.** 0.x ships a reference
  Worker with an HTTP API (`POST /v1/sandbox`, exec over SSE, files), but it
  assigns sandbox IDs itself (no caller-chosen name, so no idempotent
  create), has no list route, and 1.0 dropped it.

So the operator deploys a Worker, and Differently talks to it as a `remote`
plugin.

## Capabilities

What the SDK documents, and how a plugin Worker maps it:

| Feature | Provider support | Notes |
|---|---|---|
| `exec` | yes | 1.0: `container.exec(argv, {stdin, cwd, env})` with stdout and stderr streams; 0.x: `exec` |
| `exec.stream` | yes | Stream the two outputs as NDJSON lines |
| `files` | yes | 1.0 `Files`: `readFile`, `writeFile`, `mkdir`, `readDirectory`, `remove` |
| stdin | yes | |
| `lifecycle.stop-start` | no | Disk is ephemeral: "uses a fresh disk from the container image" after a stop |
| `lifecycle.pause-resume` | no | |
| `snapshots.disk` | yes, beta | 1.0 `snapshotContainer()` saves the filesystem (not memory or processes); restore with `start({containerSnapshot})`. Public beta, 30-day expiry, needs the `durable_object` scheduling policy, and the Worker API cannot list them |
| `snapshots.memory` | no | |
| `fork` | yes, beta | Start a new instance from a snapshot |
| `ports.expose` | through the Worker | 1.0 serves previews on Worker routes (`getTcpPort`); 0.x preview URLs need a custom domain with wildcard DNS |
| max size | 4 vCPU / 12 GiB / 20 GB | `standard-4`, or a custom size up to the same limits with at least 3 GiB per vCPU; the smallest, `lite`, is 1/16 vCPU and 256 MiB |
| provider TTL backstop | yes, and it is short | Cloudflare stops an instance "shortly after the Durable Object becomes inactive"; 1.0 `setInactivityTimeout` allows up to 6 hours, and activity inside the container does not count. The plugin has to keep each sandbox's object alive (an alarm) until its TTL |

## Configure

Deploy the plugin Worker (with its container image and Durable Object
bindings) in your Cloudflare account on the Workers Paid plan, give it a
bearer token as a Workers secret, and register it:

```sh
differently admin secret put cloudflare-plugin-token --from-file ./plugin-token.txt
differently admin pool add cloudflare --adapter remote --trust external \
  --config endpoint=https://dly-sandbox-adapter.example.workers.dev \
  --max-concurrency 50 --secret cloudflare-plugin-token
```

```yaml
pools:
  - name: cloudflare
    adapter: remote
    trust: external
    maxConcurrency: 50
    config:
      endpoint: https://dly-sandbox-adapter.example.workers.dev
    credentials: {secret: cloudflare-plugin-token}
```

Plugin shape: a Worker that serves the
[adapter protocol](../adapters/specification.md#7-http-protocol) routes under
`/adapter/v1` (option B in the specification) and sets the
`Differently-Adapter-Protocol: 1` header.

- **Auth**: compare `Authorization: Bearer` with the Workers secret, in
  constant time, on every route.
- **Create**: one sandbox Durable Object per sandbox, addressed by name
  (`getByName(sandboxId)`), so a replay reaches the same object and returns
  its instance. Start the container from the template's image (instance
  type from the request's size), or from `fromSnapshot`. Store sandbox ID,
  installation and env in the object's storage, and record the sandbox in a
  registry object for its installation.
- **List**: read the installation's registry object and each sandbox's
  state. Cloudflare's own docs point to a registry you keep for this; the
  REST instance list is meant for administration and cleanup.
- **Exec**, **files**, **snapshots** and **ports** as in the table. Answer
  `unsupported_operation` for stop, start, pause and resume.
- **Delete**: destroy the container and the object's state, remove the
  registry entry; a missing sandbox is success.
- **Lifetime**: an alarm keeps each object active until the sandbox's TTL
  (or a TTL update) passes. If the instance stops anyway, report it
  `stopped`; its files are gone, so Differently marks the sandbox lost.
- **Errors**: an account at its memory, vCPU or disk limit →
  `insufficient_capacity` (503); an image the application does not have →
  `invalid_request`; an instance that does not start → `sandbox_failed`.
- **Info**: declare `exec`, `exec.stream`, `files`, `isolation.microvm`,
  and `snapshots.disk` and `fork` only if you accept the beta.

## Limits and differences

- Workers Paid plan only. Account limits on concurrently running instances:
  6 TiB of memory, 1,500 vCPU and 30 TB of disk, raised on request.
- Placement is near the incoming request by default, and the Durable Object
  and its instance are "not guaranteed to run in the same location"; regions
  (ENAM, WNAM, EEUR, WEUR, APAC, SAM, ME, OC, AFR) or jurisdictions (`eu`,
  `us`, `fedramp`) can be required.
- Cold starts are "often… in the 1-3 second range".
- Pricing: billed per 10 ms of running time: memory $0.0000025 per GiB-second
  and disk $0.00000007 per GB-second on provisioned size, CPU $0.000020 per
  vCPU-second on active use, plus egress and Workers and Durable Objects
  charges.

## How it was tested

Not tested: no adapter or plugin exists. The facts above come from
Cloudflare's Sandbox SDK, Containers and API documentation listed in the
header.
