# Koyeb

| | |
|---|---|
| Status | **Documented**: no built-in adapter. Koyeb Sandboxes (public preview) can be driven through a `remote` plugin built on Koyeb's SDK |
| Adapter | none (use `remote`) |
| Trust | external (provider-managed) |
| Isolation | microVM per instance (Cloud Hypervisor on bare metal, per Koyeb's FAQ) |
| API checked | [API reference](https://www.koyeb.com/docs/reference/api) ([public.swagger.json](https://api.prod.koyeb.com/public.swagger.json)), [sandbox docs](https://www.koyeb.com/docs/sandboxes) and the [sandbox-container executor docs](https://github.com/koyeb/sandbox-container/blob/main/docs/sandbox_executor.md), read 2026-10-28 |

## Why there is no built-in adapter

Koyeb's control plane is fully documented: a sandbox is a `SANDBOX`-type
service, created, listed, paused, resumed and deleted through the public
API. The work inside a sandbox is the gap:

- Files, background processes and exposed ports go through an executor
  running inside the sandbox. Its HTTP API is documented in Koyeb's
  `sandbox-container` repository, but how to reach it on Koyeb (a route on
  the sandbox's public URL, a routing-key header, and a secret injected as
  an environment variable) appears only in the SDK's source.
- The documented exec endpoint, `GET /v1/streams/instances/exec`, is a
  WebSocket, and how it authenticates isn't documented either.
- Pausing a service stops its deployments and resuming redeploys it, so
  memory is not kept.

Building on the SDK's private details would break silently when Koyeb
changes them, so Differently doesn't.

## Capabilities

What Koyeb documents for sandboxes:

| Capability | Koyeb |
|---|---|
| exec | yes; SDK `exec()`, or the API's WebSocket exec |
| exec.stream | yes; SDK callbacks, or WebSocket frames |
| files | yes; SDK file operations, through the in-sandbox executor |
| stdin | yes; WebSocket exec carries stdin frames |
| lifecycle.stop-start | service pause and resume; whether the disk survives the redeploy isn't documented |
| lifecycle.pause-resume | no; pause does not keep memory |
| snapshots.disk | yes; instance snapshots of type `FILESYSTEM`, and a new service starts from one |
| snapshots.memory | unclear; a `FULL` snapshot type exists, and the docs don't say it includes memory |
| fork | through snapshots: create services from one snapshot |
| ports.expose | yes; SDK `expose_port()`, one port at a time on the sandbox's public URL |
| network.egress-control | yes; `network_policy.egress` with a mode and an allow list |
| Max size | up to 40 vCPU / 128 GB (`5xlarge`); GPU types exist, and which types sandboxes may use isn't stated |
| Provider TTL backstop | `life_cycle.delete_after_create` and `delete_after_sleep` |

## Configure

Run a [remote plugin](remote.md) that speaks Differently's adapter protocol
([specification §7](../adapters/specification.md#7-http-protocol))
and calls Koyeb's SDK, then register it as a `remote` pool:

```sh
differently admin secret put koyeb-plugin-token --from-file ./plugin-token.txt
differently admin pool add koyeb --adapter remote --trust external \
  --config endpoint=https://koyeb-plugin.internal:8443 --max-concurrency 10 \
  --secret koyeb-plugin-token
```

```yaml
pools:
  - name: koyeb
    adapter: remote
    trust: external
    config: {endpoint: "https://koyeb-plugin.internal:8443"}
    credentials: {secret: koyeb-plugin-token}
```

The plugin holds the Koyeb API token; the pool's credential is the
plugin's own bearer token. Differently doesn't ship this plugin.

## Limits and differences

- Sandboxes are a public preview, available on the Starter, Pro and Scale
  plans.
- An app holds at most 16 services.
- Create has no idempotency key, but claims from a prewarmed pool
  (`POST /v1/claim`) take one (`request_id`).
- Listing filters by name, IDs, type, status or region; there are no tags.
- Scale-to-zero after idle: Deep Sleep starts a new VM in 1 to 5 s; Light
  Sleep (preview) wakes from a snapshot in about 200 ms.
- Regions: Frankfurt, Washington, Singapore, Tokyo, Paris and AWS
  us-east-1; San Francisco in preview.
- Pricing: per second by instance size, plus a plan fee.
- Koyeb's docs announce that Koyeb is joining Mistral AI.

## How it was tested

Nothing ran: there is no adapter or plugin. Read from Koyeb's docs and
published spec on 2026-10-28.
