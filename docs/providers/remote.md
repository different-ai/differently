# Remote plugins (any backend)

| | |
|---|---|
| Status | **Tested with a fake server**: the HTTP client runs against a fake plugin served by `adapterhttp.Handler` (`adapterhttp_test.go`); no plugin outside the repository has run yet |
| Adapter | `remote` (`internal/wiring`, client in `pkg/adapter/adapterhttp`) |
| Trust | `private` by default; set `trust: external` when the plugin fronts someone else's infrastructure |
| Isolation | whatever the plugin's backend provides; the plugin reports it in `/info` |
| API checked | Differently's own [HTTP adapter protocol](../adapters/specification.md#7-http-protocol), version 1 |

A `remote` pool talks to a plugin you run: a small HTTP service that speaks
Differently's adapter protocol and drives any backend (an SDK-only provider,
an in-house fleet, a lab of machines). Use it for providers that have no
built-in adapter. A plugin can be written in any language; Go plugins can
serve an existing adapter with `adapterhttp.Handler`
([writing a community adapter](../adapters/specification.md#10-writing-a-community-adapter)).

## Capabilities

Whatever the plugin advertises in `/capabilities`. Requests that need a
feature are placed only on pools that advertise it, and an operation the
plugin lacks returns `unsupported_operation`. The protocol carries every
feature: exec, exec.stream, files, stdin, stop/start, pause/resume,
snapshots, fork and ports. `ttlSeconds` is passed on create, so the plugin
can set a provider-side backstop.

## Configure

```sh
differently admin secret put lab-plugin-token --from-file ./lab-plugin-token.txt
differently admin pool add lab-fleet --adapter remote --trust private \
  --config endpoint=https://lab-adapter.internal:8443 \
  --region lab-1 --max-concurrency 20 --secret lab-plugin-token
```

or in the server config:

```yaml
pools:
  - name: lab-fleet
    adapter: remote
    trust: private                 # external if the plugin fronts a third party
    region: lab-1
    maxConcurrency: 20             # important when the plugin reports unbounded capacity
    config:
      endpoint: https://lab-adapter.internal:8443
    credentials:
      secret: lab-plugin-token     # sent as Authorization: Bearer <token>
```

## Limits and differences

- The plugin must make `Create`, `Delete` and `List` safe to repeat, with the
  sandbox ID as the key, or the reconciler cannot clean up after it
  ([reconciliation contract](../adapters/specification.md#6-reconciliation-contract)).
- Request JSON is limited to 4 MiB; file content moves as raw bytes.
- Keep the plugin on a private network or behind TLS; the token is its only
  authentication.

## How it was tested

`adapterhttp_test.go` round-trips every operation through the client and the
handler, keeps error codes (including `sandbox_failed`), checks idempotent
create and delete, rejects a wrong token, and returns
`unsupported_operation` for optional operations the backend lacks. No
plugin outside the repository has been run, and there is no conformance
suite yet.
