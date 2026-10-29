# Daytona

| | |
|---|---|
| Status | **Tested with a fake server**: implemented from Daytona's published OpenAPI specs; not yet run against a live account |
| Adapter | `daytona` (`internal/adapters/daytona`) |
| Trust | external (provider-managed) |
| Isolation | Daytona-managed containers (`class: container`, the default) or VMs (`class: linux-vm`) |
| API checked | Daytona's published OpenAPI specs for the control plane and the toolbox, 2026-10-28 |

## Capabilities

| Capability | Support |
|---|---|
| exec | yes; stdout and stderr come back combined |
| exec.stream | no (buffered fallback) |
| files | yes |
| stdin | no (`unsupported_operation`) |
| lifecycle.stop-start | yes |
| lifecycle.pause-resume | `class: linux-vm` only |
| snapshots.disk | yes |
| snapshots.memory | `class: linux-vm` only |
| fork | no |
| ports.expose | yes (signed preview URL, valid 1 h) |
| network.egress-control | no |
| Max size | 4 vCPU / 8 GiB / 10 GiB (Daytona's default maximum) |
| Provider TTL backstop | `ttlMinutes`; Daytona's own auto-stop is off, so Differently owns stop |

## Configure

```sh
differently admin secret put daytona-key --from-file ./daytona-api-key.txt
differently admin pool add daytona --adapter daytona --trust external \
  --config target=us --config class=container \
  --region us --max-concurrency 20 --secret daytona-key
```

or in the server config:

```yaml
pools:
  - name: daytona
    adapter: daytona
    trust: external                # always external
    region: us
    maxConcurrency: 20
    config:
      apiURL: https://app.daytona.io/api   # default
      target: us                   # Daytona region; also the pool's region
      class: container             # or linux-vm
      # snapshot: my-snapshot      # base snapshot for new sandboxes
    credentials:
      secret: daytona-key
```

With Helm, set `providers.daytona.enabled: true` and its `pool.config`. An
external pool receives work only when policy allows external placement and
lists it in `externalPools`
([approve external providers](../guides/keys-and-providers.md#approve-external-providers-safely)).

## Limits and differences

- Requested CPU, memory and disk are not sent: a sandbox gets its
  snapshot's size. Quota and cost still use the requested size.
- Exec has no stdin and no streaming, and timeouts default to 600 s.
- Billed by Daytona under your account's plan.

## How it was tested

Unit tests against a fake server built from the published specs
(`daytona_test.go`): create and idempotent create, `List` for the
reconciler, exec (stdin refused), files, delete, health, capabilities per
class, and error classification (429 as `insufficient_capacity`, outages as
`provider_unavailable`). Stop/start, pause/resume, snapshots and ports are
implemented but not covered by a test. Nothing has run against a live
Daytona account.
