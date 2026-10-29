# Beam

| | |
|---|---|
| Status | **Documented**: no built-in adapter. Beam's sandboxes can be driven through a `remote` plugin built on Beam's own SDK |
| Adapter | none (use `remote`) |
| Trust | external for Beam Cloud. Beam's engine, [beta9](https://github.com/beam-cloud/beta9), is open source (AGPL-3.0); a plugin in front of your own beta9 cluster is declared trust |
| Isolation | Containers by default ("isolated containers"; the docs don't say whether runc or gVisor), or a microVM with `use_vm=True` |
| API checked | [API reference](https://docs.beam.cloud/v2/reference/api), [pods.swagger.json](https://docs.beam.cloud/v2/reference/api-docs/pods.swagger.json), [gateway.swagger.json](https://docs.beam.cloud/v2/reference/api-docs/gateway.swagger.json) and the [sandbox guides](https://docs.beam.cloud/v2/sandbox/overview), read 2026-10-28 |

## Why there is no built-in adapter

Beam publishes an HTTP spec for its gateway that covers running commands,
files and exposing ports inside an existing sandbox. Creating one over HTTP
is the gap:

- A sandbox is a pod started from a "stub". The published spec lists a
  free-form `stubType` field but never names the sandbox value; only the
  SDK's source does.
- A stub needs an image or object ID. The spec doesn't say how to get one
  without the image-build API, and that API isn't in the hosted spec.
- Every sandbox guide uses the Python or TypeScript SDK. Only three gateway
  operations have reference pages.
- The hosted spec lags the engine: TTL updates, memory snapshots and
  filesystem images have HTTP routes in beta9's source but not in the
  published spec.
- The gateway can answer HTTP 200 with `ok: false`, so failures need
  reading from the body.

Building on the SDK's private details would break silently when Beam
changes them, so Differently doesn't.

## Capabilities

What Beam documents for sandboxes, through its SDK:

| Capability | Beam (through its SDK) |
|---|---|
| exec | yes; `process.run_code()` and `process.exec()` |
| exec.stream | SDK only; the HTTP spec only polls stdout and stderr |
| files | yes; read, write, upload, download, list, stat, find and replace |
| stdin | partly: "Input is uploaded before execution, followed by EOF"; not interactive |
| lifecycle.stop-start | no; sandboxes only terminate |
| lifecycle.pause-resume | VMs only; Beam's VMs pause with memory but have no documented HTTP API |
| snapshots.disk | yes; `create_image_from_filesystem()` |
| snapshots.memory | yes; `snapshot_memory()`, "on supported runtimes" |
| fork | through snapshots |
| ports.expose | yes; `expose_port()`, and the URLs are **public** |
| network.egress-control | yes; `block_network`, or an allow list of up to 64 entries |
| Max size | not published; defaults are 1 CPU and 128 MiB, GPUs available |
| Provider TTL backstop | `keep_warm_seconds` is the sandbox's TTL (Python default 600 s; `-1` means terminate manually) |

## Configure

Run a [remote plugin](remote.md) that speaks Differently's adapter protocol
([specification §7](../adapters/specification.md#7-http-protocol))
and calls Beam's SDK, then register it as a `remote` pool:

```sh
differently admin secret put beam-plugin-token --from-file ./plugin-token.txt
differently admin pool add beam --adapter remote --trust external \
  --config endpoint=https://beam-plugin.internal:8443 --max-concurrency 10 \
  --secret beam-plugin-token
```

```yaml
pools:
  - name: beam
    adapter: remote
    trust: external
    config: {endpoint: "https://beam-plugin.internal:8443"}
    credentials: {secret: beam-plugin-token}
```

The plugin holds the Beam token (`BEAM_TOKEN`); the pool's credential is
the plugin's own bearer token. Differently doesn't ship this plugin.

## Limits and differences

- Concurrency is capped per plan: 30, 1,000 or unlimited CPU containers,
  and 5, 50 or 1,000+ GPUs, on the Developer, Team and Growth plans.
- Egress can be blocked or limited to an allow list of up to 64 entries.
- Self-hosting beta9 doesn't provide every Beam Cloud feature.
- Pricing: per second per physical core and per GiB of memory, plus a plan
  fee.

## How it was tested

Nothing ran: there is no adapter or plugin. Read from Beam's docs and
published specs on 2026-10-28.
