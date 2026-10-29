# Kubernetes: install with Helm

This guide takes an existing cluster to a first running sandbox. Differently's
chart deploys the control plane only; sandboxes run on an
[OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) server that
you install first.
Cloud-specific notes are in [aws.md](aws.md), [gcp.md](gcp.md) and
[azure.md](azure.md), and self-managed distributions are in
[bare-metal.md](bare-metal.md).

Status: **documented**. These steps have not been run on a real cluster or
against a live OpenSandbox server. The chart (version 0.2.0) passes `helm lint`
and `helm template` locally for the value sets in `deploy/helm/differently/ci/`.

The examples use namespace `differently`, release `dly` and the chart from
this repository (`deploy/helm/differently`). The resources are named
`dly-differently-*`.

## 1. Check prerequisites

Work through [prerequisites.md](prerequisites.md). You need a cluster for
the control plane, PostgreSQL for anything beyond evaluation, and a place to
run OpenSandbox. KVM nodes are needed only if you choose OpenSandbox's Fast
Sandbox runtime.

## 2. Install OpenSandbox and choose its runtime

Install OpenSandbox with its own Helm chart or manifests, following its
documentation. This chart does not do it for you. Decide:

- **Runtime.** Docker or Kubernetes for containers (they share the host
  kernel), or Fast Sandbox for Firecracker microVMs on KVM nodes. The Docker
  runtime and the Kubernetes runtime (single-node k3s, from OpenSandbox's own
  charts) have run with Differently; Fast Sandbox is **unverified**
  ([POC 12](../proof-of-concepts.md#poc-12-opensandbox-kubernetes-runtime),
  [POC 11](../proof-of-concepts.md#poc-11-fast-sandbox-through-the-adapter)).
- **OpenSandbox's charts.** Install `base` (the CRDs) first and let it create
  its namespaces (`opensandbox-system`, `opensandbox-dataplane`); creating
  them yourself makes `base` refuse to install. The charts default to images
  in a registry in mainland China; Docker Hub has the same images as
  `docker.io/opensandbox/{server,controller,execd,egress,image-committer}:release-1.2.0`.
- **Pause and snapshots on the Kubernetes runtime** need the controller's
  snapshot registry (`controller.snapshot.registry`). Without one, set
  `opensandbox.features: [exec, exec.stream, files]`.
- **API key.** Set the server's `api_key`. Without one, OpenSandbox refuses to
  start unless insecure mode is acknowledged; do not run it that way.
- **Versions.** Pin the server and `execd` images. The adapter was checked
  against server `release-1.2.0` with `execd` `release-1.2.0` (set
  `opensandbox.lineEndings: kept`) and `v1.1.0`.
- **Location.** The chart's default `apiURL` assumes a Service
  `opensandbox-server` in namespace `opensandbox-system` on port 80, which is
  where OpenSandbox's own chart puts it. Note the real URL if yours differs.
- **Sandbox egress.** Differently does not configure OpenSandbox's network
  policy. Restrict sandbox egress in OpenSandbox or with NetworkPolicy in its
  namespace, and block cloud metadata endpoints.

## 3. Create the namespace

Differently's pods do not need privilege:

```sh
kubectl create namespace differently
```

## 4. Create secrets

**OpenSandbox API key.** Store the server's `api_key` in a Secret. Either
name it yourself and set `opensandbox.existingSecret` (this guide's
examples), or create the Secret the chart reads by default when neither
`existingSecret` nor `apiKey` is set, `<fullname>-opensandbox` with key
`api-key`. The chart's NOTES print the exact command, and server
pods wait until the Secret exists.

```sh
kubectl -n differently create secret generic opensandbox-api-key \
  --from-literal=api-key='<the OpenSandbox server api_key>'
# or, for the chart's default name with release dly:
# kubectl -n differently create secret generic dly-differently-opensandbox --from-literal=api-key='<...>'
```

**Database (production).** Use managed PostgreSQL or a
[CloudNativePG](https://cloudnative-pg.io/) cluster. Create a database and user
for Differently, then store the DSN:

```sh
kubectl -n differently create secret generic differently-database \
  --from-literal=database-url='postgres://differently:PASSWORD@db.internal.example.com:5432/differently?sslmode=verify-full'
```

With CloudNativePG, the operator creates an `<cluster>-app` Secret. Its `uri`
key holds a DSN, so you can point the chart at it directly:
`postgresql.external.existingSecret=<cluster>-app` and
`postgresql.external.existingSecretKey=uri`. Check that the key name matches
your CNPG version.

**Bootstrap token (recommended for GitOps, optional otherwise).** Without
this, the chart generates the token once and keeps it across upgrades with
`lookup`. Tools that render with `helm template` (Argo CD, some Flux setups)
cannot `lookup`, so they must use an existing Secret:

```sh
SECRET=$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom | head -c 40)
kubectl -n differently create secret generic differently-tokens \
  --from-literal=bootstrap-token="dly_bootop1_${SECRET}"
```

**Secrets key (generated, or yours for GitOps).** This key seals provider
credentials that operators store in Differently from the dashboard, CLI or
API.
Without your own Secret, the chart does three things:

- it generates 32 random bytes once, into `dly-differently-secrets-key`
  (key `secrets-key`);
- it keeps them across upgrades with `lookup`;
- it leaves the Secret in place on `helm uninstall`.

Renderers that cannot `lookup` would generate a new key on every render,
and a new key makes every stored credential unreadable. So with them, create
the Secret yourself:

```sh
kubectl -n differently create secret generic differently-secrets-key \
  --from-literal=secrets-key="$(differently-server secrets-key)"   # or: openssl rand -base64 32
```

Either way, back the key up
([keys-and-providers.md](../guides/keys-and-providers.md#back-up-the-secrets-key)).

**External providers (optional, off by default).** Skip this unless you want
Daytona or Freestyle overflow. You can also add them later from the dashboard
or CLI with the key stored in Differently instead
([keys-and-providers.md](../guides/keys-and-providers.md#add-daytona-or-freestyle-with-the-key-stored-in-differently)).

```sh
kubectl -n differently create secret generic daytona-credentials --from-literal=api-key='dtn_...'
```

## 5. Write values and install

The chart's `opensandbox:` block is enabled by default and renders the
default pool, first in the pool list (`config.pools` defaults to empty):

| Value | Default | Purpose |
|---|---|---|
| `opensandbox.enabled` | `true` | Register the OpenSandbox server as a pool |
| `opensandbox.apiURL` | `http://opensandbox-server.opensandbox-system.svc/v1` | Lifecycle API base URL, ending in `/v1` |
| `opensandbox.trust` | `private` | `private` if you operate the server and its runtime, `external` if someone else does |
| `opensandbox.pool.name` | `opensandbox` | Pool name; also `pool.region`, `pool.labels`, `pool.maxConcurrency`, `pool.costModel` |
| `opensandbox.image` | empty (`ubuntu:24.04`) | Default image URI; a request's template overrides it |
| `opensandbox.templateId` | empty | A Fast Sandbox template to create sandboxes from instead of an image |
| `opensandbox.isolation` | `container` | What the runtime provides, reported in the trust report: `container` (Docker or Kubernetes runtime) or `microvm` (Fast Sandbox). Differently does not enforce it |
| `opensandbox.features` | empty (adapter defaults) | Capabilities to advertise, as a list; add `snapshots.memory` only when the runtime captures memory |
| `opensandbox.useServerProxy` | `true` | Reach exec and files through the OpenSandbox server's proxy, so only its port is needed |
| `opensandbox.existingSecret` / `existingSecretKey` | empty / `api-key` | A Secret you created holding the server's API key |
| `opensandbox.apiKey` | empty | Inline API key, stored by the chart in a Secret. Prefer `existingSecret` |
| `opensandbox.env` | `DIFFERENTLY_OPENSANDBOX_API_KEY` | Environment variable the server reads the key from; the pool refers to it by name |

Quick evaluation, with the in-memory store and one replica. State is lost
when the server restarts:

```sh
helm install dly deploy/helm/differently -n differently \
  --set opensandbox.existingSecret=opensandbox-api-key
```

Production-shaped install (`values-prod.yaml`):

```yaml
config:
  store:
    driver: postgres
  # policy stays external: never unless you opt in (see "Hybrid" below)

opensandbox:
  apiURL: http://opensandbox-server.opensandbox-system.svc/v1
  trust: private
  isolation: container          # microvm only if OpenSandbox runs Fast Sandbox
  existingSecret: opensandbox-api-key

postgresql:
  external:
    existingSecret: differently-database
    existingSecretKey: database-url

auth:
  existingSecret: differently-tokens

secrets:
  existingSecret: differently-secrets-key   # omit to let the chart generate the key

server:
  replicas: 3

ingress:
  enabled: true
  className: <your-ingress-class>
  hosts: [differently.example.com]
  tls:
    enabled: true
    certManager:
      clusterIssuer: letsencrypt-prod

networkPolicy:
  server:
    egress:
      databaseCIDRs: ["10.20.0.0/24"]     # where PostgreSQL lives
```

```sh
helm install dly deploy/helm/differently -n differently -f values-prod.yaml
```

What the install does:

- **Values check.** `values.schema.json` mirrors the server's strict config
  schema, so a typo such as `config.policy.extrnal` fails here, not at runtime.
- **Migrations.** With `postgres`, a pre-install and pre-upgrade hook Job
  (`dly-differently-migrate`) runs `differently-server migrate`. It runs as a
  server init container instead when you use `postgresql.embedded`.
- **Preflight.** Each server pod first runs `differently-server preflight`
  (init container `preflight`) and refuses to start on configuration errors,
  including a pool credential reference that does not resolve.
- **Secrets key.** With `secrets.enabled` (the default), the server gets
  `DIFFERENTLY_SECRETS_KEY` from `secrets.existingSecret`, or from the
  generated `dly-differently-secrets-key`. Preflight checks that it is
  base64 of 32 bytes.
- **NetworkPolicy.** Server egress is limited to DNS, the database,
  OpenSandbox and enabled providers. Providers added later through the
  dashboard or API need `networkPolicy.server.egress.runtimeProviders: true`,
  which allows `providerCIDRs` on port 443. The OpenSandbox rule is
  `networkPolicy.server.egress.opensandbox`: by default pods in namespace
  `opensandbox-system` (`namespaceSelector`) on port 80. Change `namespaceSelector`
  or `podSelector` if OpenSandbox runs elsewhere in the cluster, or set
  `cidrs` for a server outside it. With `opensandbox.useServerProxy: false`,
  add the per-sandbox endpoints through `networkPolicy.server.egress.extra`.

Hybrid (private first, Daytona overflow), as extra values:

```yaml
providers:
  daytona:
    enabled: true
    existingSecret: daytona-credentials
config:
  policy:
    external: overflow          # never | overflow | allowed
    externalPools: [daytona]
```

Enabling the provider alone changes nothing about placement. Policy must also
allow it, and so must the request's classification: `private-only` never
leaves private pools, `internal` may overflow, and `public` is allowed.
A global policy set through `PUT /v1/policies/global` replaces
`config.policy` at runtime, and so does approving a pool with
`differently admin pool use <pool> --approve-external`. See
[hybrid-overflow.md](../guides/hybrid-overflow.md) and
[keys-and-providers.md](../guides/keys-and-providers.md).

## 6. Create an organization, a project and a project key

The bootstrap token is an operator key (`admin` scope). Use it to set up
tenancy, then use narrower keys day to day. To create a named operator key
and retire the bootstrap key, see
[the bootstrap operator token](../guides/keys-and-providers.md#the-bootstrap-operator-token).

```sh
export DIFFERENTLY_API_KEY=$(kubectl -n differently get secret dly-differently-tokens \
  -o jsonpath='{.data.bootstrap-token}' | base64 -d)        # or your differently-tokens Secret
kubectl -n differently port-forward svc/dly-differently-server 8080:8080 &
export DIFFERENTLY_URL=http://127.0.0.1:8080                  # or https://differently.example.com

ORG=$(curl -fsS -X POST "$DIFFERENTLY_URL/v1/admin/organizations" \
  -H "Authorization: Bearer $DIFFERENTLY_API_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"acme","defaultProject":"default"}')
ORG_ID=$(echo "$ORG" | jq -r .organization.id)
PROJECT_ID=$(echo "$ORG" | jq -r .project.id)

PROJECT_KEY=$(curl -fsS -X POST "$DIFFERENTLY_URL/v1/api-keys" \
  -H "Authorization: Bearer $DIFFERENTLY_API_KEY" -H 'Content-Type: application/json' \
  -d "{\"name\":\"first-sandbox\",\"orgId\":\"$ORG_ID\",\"projectId\":\"$PROJECT_ID\",
       \"scopes\":[\"sandboxes:read\",\"sandboxes:write\",\"sandboxes:exec\"]}" | jq -r .token)
echo "$PROJECT_ID"
```

The token is shown only once, so store `PROJECT_KEY` in your secret manager.
To replace it later without downtime, rotate it with a grace period
([rotate Differently API keys](../guides/keys-and-providers.md#rotate-differently-api-keys-without-downtime)).
You can also open `$DIFFERENTLY_URL/dashboard/` and sign in with a key.

## 7. Create the first sandbox

With the CLI (`differently`, configured by `DIFFERENTLY_URL` and `DIFFERENTLY_API_KEY`):

```sh
export DIFFERENTLY_API_KEY=$PROJECT_KEY
differently sandbox create --profile small --wait        # prints the sbx_... ID
differently sandbox exec <sbx_id> -- echo ok
```

Or with the HTTP API directly:

```sh
SBX=$(curl -fsS -X POST "$DIFFERENTLY_URL/v1/sandboxes" \
  -H "Authorization: Bearer $PROJECT_KEY" -H 'Content-Type: application/json' \
  -d '{"profile":"small"}' | jq -r .id)
curl -fsS "$DIFFERENTLY_URL/v1/sandboxes/$SBX/wait?status=running&timeoutSeconds=120" \
  -H "Authorization: Bearer $PROJECT_KEY" | jq '{status, pool, statusReason}'
curl -fsS -X POST "$DIFFERENTLY_URL/v1/sandboxes/$SBX/exec" \
  -H "Authorization: Bearer $PROJECT_KEY" -H 'Content-Type: application/json' \
  -d '{"args":["echo","ok"]}' | jq '{exitCode, stdout}'
curl -fsS "$DIFFERENTLY_URL/v1/sandboxes/$SBX/placement" -H "Authorization: Bearer $PROJECT_KEY" | jq .
curl -fsS -X DELETE "$DIFFERENTLY_URL/v1/sandboxes/$SBX" -H "Authorization: Bearer $PROJECT_KEY"
```

If the sandbox stays `queued` or fails, the placement explanation and
`statusReason` say why. Usual causes:

- the `opensandbox` pool is unhealthy: wrong `apiURL`, wrong API key, or the
  NetworkPolicy blocks the server's port (80 with OpenSandbox's chart);
- OpenSandbox cannot pull the image or start the sandbox (its own logs say
  why);
- the request asks for a capability the pool does not offer (for example
  `lifecycle.stop-start`);
- policy excludes every pool.

## 8. Run the smoke test

The test Pod creates a sandbox, runs `echo ok` and deletes it. With the
bootstrap key it needs the project ID from step 6. Alternatively, put a
project-scoped key in a Secret and set `test.existingSecret`.

```sh
helm upgrade dly deploy/helm/differently -n differently --reuse-values --set test.projectId=$PROJECT_ID
helm test dly -n differently --logs
```

## Configuration reference

All settings are in [`values.yaml`](../../deploy/helm/differently/values.yaml),
and the comments explain each one. Useful ones:

| Value | Default | Purpose |
|---|---|---|
| `config.*` | server defaults | Rendered into `/etc/differently/config.yaml`: profiles, classifications, pools, policy, quotas, scheduler, audit |
| `config.store.driver` | `memory` | `postgres` for anything beyond evaluation |
| `server.replicas` | `1` | Must stay 1 with `memory` |
| `auth.existingSecret` | chart-managed | Bootstrap token Secret (key `bootstrap-token`) |
| `secrets.enabled` | `true` | Give the server a secrets key, so provider credentials can be stored in Differently; `false` means none can be stored |
| `secrets.existingSecret` / `existingSecretKey` | empty (chart generates `<fullname>-secrets-key`) / `secrets-key` | A Secret you manage holding the base64 32-byte key; required for renderers that cannot `lookup` |
| `opensandbox.*` | enabled | The default pool: see the table in step 5 |
| `networkPolicy.server.egress.opensandbox` | namespace `opensandbox-system`, port 80 | Where the server may connect to reach OpenSandbox |
| `providers.daytona/freestyle.enabled` | `false` | Register an external pool (still gated by `config.policy`) |
| `networkPolicy.server.egress.restricted` | `true` | Server egress limited to DNS, the database, OpenSandbox and enabled providers |
| `networkPolicy.server.egress.runtimeProviders` | `false` | Also allow `providerCIDRs` on 443 for providers added at runtime (dashboard or API); `providers.*` already open it |
| `metrics.serviceMonitor.enabled` | `false` | Prometheus Operator scrape of `/metrics` |
| `global.imageRegistry` | empty | Mirror registry for every image |

Other pools (a second OpenSandbox server, a `remote` plugin) go under
`config.pools`. Their credentials are referenced by environment variable,
as the comments in `values.yaml` describe, or name a key stored in
Differently (`credentials: {secret: name}`). See
[opensandbox-pools.md](../guides/opensandbox-pools.md) and the
[adapter specification](../adapters/specification.md). Values from the
node-agent releases (`agent.*`, `auth.agentToken*`,
`config.scheduler.agentTimeoutSeconds`, `adapter: firecracker`) now fail the
schema check.

Exposure notes:

- The ingress publishes the API and the dashboard on one host. `/metrics` is
  on `metrics.port` (default 9091), which the ingress never routes to; set
  `metrics.port: 0` only if you want it back on the API port.
- The OpenSandbox server does not need to be reachable from outside the
  cluster. Clients never talk to it: exec, files and lifecycle go through
  Differently.
- The upstream ingress-nginx project has been retired. Prefer a maintained
  controller or Gateway API. An HTTPRoute pointing at
  `dly-differently-server:8080` works the same way.

## Verify

```sh
kubectl -n differently get pods -o wide
#   dly-differently-server-*   Running, READY 1/1 (per replica)
kubectl -n differently logs deploy/dly-differently-server -c preflight
curl -fsS "$DIFFERENTLY_URL/readyz"                                    # {"status":"ready"}
curl -fsS "$DIFFERENTLY_URL/version"
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/pools" \
  | jq '.items[] | {name, adapter, trust, health: .health.status}'      # opensandbox: healthy
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/trust-report" \
  | jq '{telemetry, globalPolicy: .globalPolicy.external,
         pools: [.pools[] | {name, trust, isolation, endpoint, credentialRef, approvedForExternalPlacement}]}'
helm test dly -n differently --logs                                    # PASS: sandbox sbx_... ran 'echo ok'
```

The install worked when all of these hold:

- the `opensandbox` pool is healthy;
- the trust report shows the OpenSandbox endpoint, the credential only as a
  reference (`env:...`, or `secret:...` for a key stored in Differently), the isolation you actually deployed, and
  `external: never` or your deliberate hybrid policy;
- the smoke test passes.
