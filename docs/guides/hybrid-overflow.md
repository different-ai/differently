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

## 1. Store the provider credential

A pool's credential is never returned by the API and never reaches
sandboxes. It can live in one of two places.

**In Differently.** The value is sealed in the database under the server's
secrets key.
This needs no Kubernetes Secret, and replacing the value later takes effect
without a restart. Store the key, then register the pool at runtime with
`credentials: {secret: daytona-key}`:

```bash
differently admin secret put daytona-key --from-file ./daytona-api-key.txt
differently admin pool add daytona-burst --adapter daytona --trust external \
  --config target=us --config snapshot=daytona-small --config class=container \
  --region us --max-concurrency 50 --secret daytona-key
```

The dashboard's "Add provider" does both steps.
[keys-and-providers.md](keys-and-providers.md#add-daytona-or-freestyle-with-the-key-stored-in-differently)
covers the API form, the egress value a runtime-added provider needs
(`networkPolicy.server.egress.runtimeProviders`), and a limitation with
several server replicas. Then go to step 3.

**In your secret store.** The pool holds a reference to an environment
variable or file on the server, and the database holds only that reference:

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

Without Helm, declare the pool yourself as below. A pool in the config file
can also use a stored key: `credentials: {secret: daytona-key}`.

## 2. Declare the pool

```yaml
pools:
  - name: opensandbox         # your private runtime
    adapter: opensandbox
    trust: private            # required for opensandbox pools
    region: dc1
    costModel: { cpuHour: 0.010, memoryGiBHour: 0.002 }
    credentials: { env: DIFFERENTLY_OPENSANDBOX_API_KEY }
    config:
      apiURL: http://opensandbox-server.opensandbox-system.svc/v1
      isolation: container
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

An OpenSandbox server that someone else operates is an external pool too:
declare it with `trust: external`, and it follows the same rules as Daytona
and Freestyle below. With Helm, the chart's `opensandbox` block renders the
private pool for you (see [opensandbox-pools.md](opensandbox-pools.md)).

## 3. Authorize overflow explicitly

```yaml
policy:
  external: overflow
  externalPools: [daytona-burst]
  maxCostPerHour: 0.50
```

`differently admin pool use <pool> --approve-external` also approves a pool,
as a side effect of switching to it. Prefer this explicit policy change,
which is easier to review
([approving external providers safely](keys-and-providers.md#approve-external-providers-safely)).

Then narrow per tenant where needed:

```bash
echo '{"external":"never"}' > never.json
differently admin policy org:$REGULATED_ORG never.json
```

## 4. Verify before trusting it

```bash
differently-server preflight --config config.yaml   # env:/file: references resolve; a secrets key exists for secret: ones
differently admin pool list                         # health of each pool
differently sandbox create --classification internal --wait
differently sandbox placement <id>                  # outcome, pool, every candidate's reasons
differently admin trust-report                      # recentExternalPlacements, endpoints, credentials by reference (env:, file:, secret:)
```

The trust report's `recentExternalPlacements` lists each sandbox that ran
outside your pools, who created it, its classification and the reason it was
placed there.

## Overflow or switching

Overflow keeps your private pools first and sends work to an approved
external pool only when no private pool can take it at that moment.
Sometimes you want all new work on one provider instead: during an outage of
your own runtime, to move to a new provider, or to test one. For that,
switch:

```bash
differently admin pool use daytona-burst    # drains every other pool; running sandboxes stay
differently admin pool use opensandbox      # back
```

Switching uses drain flags, not policy. The external pool must still be
approved, and classifications still apply: `private-only` work is refused
while every private pool is drained. Switching back leaves the policy
approval in place. Details:
[switch new sandboxes between providers](keys-and-providers.md#switch-new-sandboxes-between-providers).

## Rotating the provider key

- **A stored key:** `differently admin secret put daytona-key --from-file
  new.txt`. Every replica uses the new value within seconds, with no
  restart.
- **A `file:` reference:** picked up the same way once the mounted file
  changes.
- **An `env:` reference** (the chart's `providers.*`): update the Secret,
  then restart the server.

See
[rotate a provider key live](keys-and-providers.md#rotate-a-provider-key-live).

## Semantics that differ by provider

| | OpenSandbox (yours) | Daytona | Freestyle |
|---|---|---|---|
| Exec output | separate stdout/stderr, streaming, line-based | combined into stdout | separate, buffered, ≤5 min inline |
| stdin on exec | no (`unsupported_operation`) | no (`unsupported_operation`) | yes |
| Pause | yes; state kept (checked on the Docker runtime) | VM class only | memory kept |
| Snapshots | disk; memory only if the runtime captures it and the pool declares `snapshots.memory` (Fast Sandbox, unverified) | filesystem (memory with VM class) | always memory + disk |
| Stop | no (`unsupported_operation`); pause or delete instead | provider stop | in-guest `poweroff` |
| Port URLs | only when OpenSandbox serves the port directly without request headers | signed preview URL (1 h) | public `*.style.dev` hostname |

For OpenSandbox, exec, pause and resume, disk snapshots and the stop
refusal were checked live against release 1.2.0 with the Docker runtime; port
URLs, restoring or forking from a snapshot, and memory snapshots were not. The Daytona and Freestyle columns
are checked only against fake servers.

Snapshots and forks stay on the pool that took them. A running sandbox is
never moved between providers.

## Costs

`costModel` drives estimates (`GET /v1/usage`) and the `cost` strategy and
`maxCostPerHour` ceilings. Estimates are not reconciled against provider
invoices yet; treat them as budgeting guides.
