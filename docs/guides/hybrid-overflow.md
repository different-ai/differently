# Hybrid overflow to external providers

Differently can place eligible sandboxes on Daytona or Freestyle when private
capacity runs out, for special hardware, or for regions you do not operate.
This is off by default and stays off until you change three things on purpose.

> The Daytona and Freestyle adapters are implemented against the providers'
> published API specifications (2026-10-28) and tested with fake servers.
> They have not been run against live accounts yet; verify in a staging
> installation first.

## What has to be true for work to leave your pools

All of these, every time:

1. The pool exists and is enabled (registering it authorizes nothing).
2. The **global policy** sets `external` to `overflow` or `allowed` **and**
   names the pool in `externalPools`.
3. No organization or project policy narrows `external` to `never`, denies the
   pool, or excludes its region.
4. The sandbox's **classification** allows it (`private-only` never does).
5. The request does not opt out (`placement.external: never`).
6. With `overflow`, no private pool can take the sandbox right now.

Security and residency constraints are evaluated before cost or speed, and the
decision — every pool considered, with reasons — is stored on the sandbox.

## 1. Store the provider credential as a secret

Credentials are references to environment variables or files on the server;
they never enter the database or any API response, and never reach sandboxes.

```bash
kubectl -n differently create secret generic provider-daytona --from-literal=api-key="$DAYTONA_API_KEY"
```

With Helm, point the chart at it; the chart exposes it to the server as
`DIFFERENTLY_DAYTONA_API_KEY` and registers the pool:

```yaml
providers:
  daytona:
    enabled: true
    existingSecret: provider-daytona
    existingSecretKey: api-key
    pool:
      name: daytona-burst
      maxConcurrency: 50
      config: { target: us, snapshot: daytona-small, class: container }
```

Without Helm, declare the pool yourself as below.

## 2. Declare the pool

```yaml
pools:
  - name: fc-private
    adapter: firecracker
    region: dc1
    costModel: { cpuHour: 0.010, memoryGiBHour: 0.002 }
  - name: daytona-burst
    adapter: daytona          # always trust: external
    region: us
    maxConcurrency: 50        # also caps provider spend exposure
    costModel: { cpuHour: 0.050, memoryGiBHour: 0.016 }
    credentials: { env: DIFFERENTLY_DAYTONA_API_KEY }
    config:
      target: us
      snapshot: daytona-small
      class: container        # or linux-vm for pause/memory snapshots
```

Freestyle is similar (`adapter: freestyle`, `config.snapshot`,
`config.egress: public` to allow outbound internet; Freestyle VMs are
deny-all by default).

## 3. Authorize overflow explicitly

```yaml
policy:
  external: overflow
  externalPools: [daytona-burst]
  maxCostPerHour: 0.50
```

Then narrow per tenant where needed:

```bash
echo '{"external":"never"}' > never.json
differently admin policy org:$REGULATED_ORG never.json
```

## 4. Verify before trusting it

```bash
differently-server preflight --config config.yaml   # credential references resolve
differently admin pool list                         # health of each pool
differently sandbox create --classification internal --wait
differently sandbox placement <id>                  # outcome, pool, every candidate's reasons
differently admin trust-report                      # recentExternalPlacements, endpoints, credentials by reference
```

The trust report's `recentExternalPlacements` lists each sandbox that ran
outside your pools, who created it, its classification and the reason it was
placed there.

## Semantics that differ by provider

| | Firecracker (yours) | Daytona | Freestyle |
|---|---|---|---|
| Exec output | separate stdout/stderr, streaming | combined into stdout | separate, buffered, ≤5 min inline |
| stdin on exec | yes | no (`unsupported_operation`) | yes |
| Pause | memory kept | VM class only | memory kept |
| Snapshots | disk | filesystem (memory with VM class) | always memory + disk |
| Stop | guest shutdown, disk kept | provider stop | in-guest `poweroff` |
| Port URLs | not yet | signed preview URL (1 h) | public `*.style.dev` hostname |

Snapshots and forks stay on the pool that took them. A running sandbox is
never moved between providers.

## Costs

`costModel` drives estimates (`GET /v1/usage`) and the `cost` strategy and
`maxCostPerHour` ceilings. Estimates are not reconciled against provider
invoices yet; treat them as budgeting guides.
