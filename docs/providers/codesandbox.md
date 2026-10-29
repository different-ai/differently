# CodeSandbox

| | |
|---|---|
| Status | **Documented** (2026-10-28): no adapter; needs a `remote` plugin |
| Adapter | none (a `remote` pool backed by a plugin that wraps `@codesandbox/sdk`) |
| Trust | external (provider-managed) |
| Isolation | Firecracker microVM |
| API checked | The CodeSandbox SDK at [v2.4.2](https://github.com/codesandbox/codesandbox-sdk/tree/10f82664b5d0869e6d67e9610ae48b40cb560548) (its `openapi.json` and `src/`), the SDK docs source ([codesandbox/docs](https://github.com/codesandbox/docs/tree/1f85ffe79cf1134fcdfe74e6ebc6d844bc3045f8/packages/projects-docs/pages/sdk)), read 2026-10-28 |

## Why there is no built-in adapter

CodeSandbox's REST API (`api.codesandbox.io`) manages sandboxes: fork from a
template, start, hibernate, shut down, delete, list by tag, change tier. But
every operation inside a VM (commands, files, ports, keep-alive) goes through
"Pitcher": a WebSocket carrying msgpack messages, with a pinned protocol
version and a `client/join` handshake. The protocol is defined only in the
SDK's TypeScript sources, not in the docs, and Go's standard library has
neither a WebSocket client nor msgpack. A built-in adapter would depend on an
undocumented protocol that can change with any SDK release.

CodeSandbox is building a REST agent ("Pint": `/api/v1/execs` with SSE,
`/api/v1/files`). It ships only in the SDK's `pint` beta tag, is not in the
docs, and is selected per VM by the API. Revisit when standard templates
report `vm_agent_type: "pint"` and the default SDK uses it.

## Capabilities

What a plugin wrapping the official SDK could offer:

| Feature | Available from CodeSandbox | Notes |
|---|---|---|
| exec | yes (SDK) | `sandbox.commands.run`; over Pitcher |
| exec.stream | yes (SDK) | Command output events over Pitcher |
| files | yes (SDK) | `sandbox.fs.*`; over Pitcher |
| stdin | yes (SDK) | Shell input over Pitcher |
| lifecycle.stop-start | yes | `POST /vm/{id}/shutdown`, then `POST /vm/{id}/start` (disk kept) |
| lifecycle.pause-resume | yes | `POST /vm/{id}/hibernate` keeps memory; resume with `/start` (0.5–2 s) |
| snapshots.disk | partly | Hibernation snapshots are kept 2 to 7 days; there is no named snapshot API |
| snapshots.memory | partly | As above |
| fork | yes | `POST /sandbox/{id}/fork` (memory and disk from a hibernated sandbox) |
| ports.expose | yes | `https://<id>-<port>.csb.app`; private sandboxes need a preview token |
| network.egress-control | no | Not documented |
| max size | 64 vCPU, 128 GiB (XLarge tier) | Tiers Pico to XLarge; disk 20–50 GB by tier per the SDK |
| provider TTL backstop | hibernation only | `hibernation_timeout_seconds` (max 86400) hibernates; no delete deadline |

## Configure

A plugin that implements the [HTTP adapter protocol](../adapters/specification.md#7-http-protocol)
with the official SDK, registered as a `remote` pool:

```yaml
pools:
  - name: codesandbox
    adapter: remote
    trust: external          # declare it: the plugin runs your code on CodeSandbox
    maxConcurrency: 10       # the Build plan allows 10 concurrent VMs
    config:
      endpoint: http://codesandbox-plugin:8080
    credentials: {secret: codesandbox-plugin-token}   # the plugin's token, not the CodeSandbox key
```

```sh
differently admin secret put codesandbox-plugin-token --from-file ./plugin-token.txt
differently admin pool add codesandbox --adapter remote --trust external \
  --config endpoint=http://codesandbox-plugin:8080 --secret codesandbox-plugin-token
```

The CodeSandbox API key (`csb_…`) stays with the plugin. `remote` pools
default to `trust: private`, so set `external` explicitly.

## Limits and differences

- No plugin exists yet; this page records what one would need.
- The REST API has no idempotency key and no key/value metadata: a plugin
  must reconcile by tag (`tags[]`, up to ten) and title.
- `GET /sandbox/{id}` has no VM run state; `GET /vm/running` (refreshed about
  every 30 s) does.
- A VM hibernates after `hibernation_timeout_seconds` unless the SDK sends
  keep-alive messages; HTTP traffic to its ports does not extend it, and by
  default a request to a port wakes it.
- No error statuses are documented; the SDK handles 403, 404, 429, 502 and
  503. Rate limits come back in `x-csb-*` headers and from `GET /meta/info`.
- Regions: not documented beyond an `ipcountry` hint at first start.
- **Pricing:** credits per started minute by tier ($0.01486 a credit); plans
  cap concurrent VMs.

## How it was tested

Not tested: there is no adapter. The REST and Pitcher details above were
read from the SDK's source and spec on 2026-10-28; only CodeSandbox's two
unauthenticated endpoints (`GET /` and a missing `/openapi.json`) were
called.
