# Freestyle

| | |
|---|---|
| Status | **Tested with a fake server**: implemented from Freestyle's published OpenAPI spec (`/v5`); not yet run against a live account |
| Adapter | `freestyle` (`internal/adapters/freestyle`) |
| Trust | external (provider-managed; Freestyle is hosted only) |
| Isolation | hardware-virtualized microVMs (Freestyle-managed) |
| API checked | Freestyle's published OpenAPI spec, 2026-10-28 |

## Capabilities

| Capability | Support |
|---|---|
| exec | yes; separate stdout and stderr; at most 5 minutes inline |
| exec.stream | no (buffered fallback) |
| files | yes |
| stdin | yes |
| lifecycle.stop-start | yes (stop runs `poweroff` in the guest; Freestyle has no stop endpoint) |
| lifecycle.pause-resume | yes |
| snapshots.disk | yes (always memory and disk) |
| snapshots.memory | yes |
| fork | yes (memory) |
| ports.expose | yes: a **public** TLS host `<port>-<slug>.<domain>`; anyone with the URL can connect |
| network.egress-control | yes: deny-all unless `egress: public` |
| Max size | 32 vCPU / 64 GiB / 256 GiB (plan-dependent) |
| Provider TTL backstop | `ttlSeconds` |

## Configure

```sh
differently admin secret put freestyle-key --from-file ./freestyle-api-key.txt
differently admin pool add freestyle --adapter freestyle --trust external \
  --config egress=none --max-concurrency 20 --secret freestyle-key
```

or in the server config:

```yaml
pools:
  - name: freestyle
    adapter: freestyle
    trust: external                # always external
    maxConcurrency: 20
    config:
      apiURL: https://api.freestyle.sh   # default
      egress: none                 # none (default, deny-all) or public
      # snapshot: my-snapshot      # base snapshot for new VMs
      # domain: style.dev          # default domain for exposed ports
    credentials:
      secret: freestyle-key
```

With Helm, set `providers.freestyle.enabled: true` and its `pool.config`.
An external pool receives work only when policy allows it
([approve external providers](../guides/keys-and-providers.md#approve-external-providers-safely)).

## Limits and differences

- Create takes no size or env: a VM boots at its snapshot's size and is grown
  with `/resize` (never shrunk). Sandbox env is written to a 0600 file in the
  VM and sourced by every exec.
- Snapshots always capture memory and disk, and report `kind: memory`.
- Exposed ports are public; use them only for content that may be public.
- Billed by Freestyle under your account's plan.

## How it was tested

Unit tests against a fake server built from the published spec
(`freestyle_test.go`): create (with resize and the env file) and idempotent
create, exec, `List` for the reconciler, and repeatable delete. Stdin, files,
stop/start, pause/resume, snapshots, fork, ports and egress are implemented
but not covered by a test. Nothing has run against a live Freestyle
account.
