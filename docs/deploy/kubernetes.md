# Kubernetes: install with Helm

This guide takes an existing cluster to a first running sandbox. Cloud-specific
node setup is in [aws.md](aws.md), [gcp.md](gcp.md) and [azure.md](azure.md),
and self-managed distributions are in [bare-metal.md](bare-metal.md).

Status: **documented**. These steps have not been run on a real cluster.
The chart passes `helm lint` and `helm template` locally.

The examples use namespace `differently`, release `dly` and the chart from
this repository (`deploy/helm/differently`). The resources are named
`dly-differently-*`.

## 1. Check prerequisites

Work through [prerequisites.md](prerequisites.md). You need at least one node
where `/dev/kvm` and `/dev/net/tun` exist, cgroup v2 is mounted and the host
kernel is 5.10, 6.1 or 6.18.

## 2. Label and taint the KVM nodes

```sh
kubectl label node <node> differently.dev/kvm=true
kubectl taint node <node> differently.dev/kvm=true:NoSchedule
```

## 3. Stage the guest kernel and root filesystem

Build them with [`deploy/images/`](../../deploy/images/README.md). Then do one
of the following:

- **Package them as an OCI image** and let the chart copy them onto every KVM
  node. Set these at install time:

  ```sh
  deploy/images/package-oci.sh --tag registry.example.com/differently-guest-images:v1 \
    --kernel out/vmlinux --rootfs out/default.ext4 --push
  # values: agent.guestImages.enabled=true, image registry/repository/tag as pushed
  ```

- **Copy them to every KVM node yourself**, into
  `/var/lib/differently/images/vmlinux` and
  `/var/lib/differently/images/default.ext4`.

## 4. Create the namespace

The agent is privileged, so the namespace must allow privileged pods:

```sh
kubectl create namespace differently
kubectl label namespace differently pod-security.kubernetes.io/enforce=privileged
```

## 5. Create secrets

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

**Tokens (recommended for GitOps, optional otherwise).** Without this, the
chart generates both tokens once and keeps them across upgrades with
`lookup`. Tools that render with `helm template` (Argo CD, some Flux setups)
cannot `lookup`, so they must use an existing Secret:

```sh
SECRET=$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom | head -c 40)
AGENT=$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom | head -c 48)
kubectl -n differently create secret generic differently-tokens \
  --from-literal=bootstrap-token="dly_bootop1_${SECRET}" \
  --from-literal=agent-token="${AGENT}"
```

**External providers (optional, off by default).** Skip this unless you want
Daytona or Freestyle overflow:

```sh
kubectl -n differently create secret generic daytona-credentials --from-literal=api-key='dtn_...'
```

## 6. Write values and install

Quick evaluation, with the in-memory store and one replica. State is lost
when the server restarts:

```sh
helm install dly deploy/helm/differently -n differently
```

Production-shaped install (`values-prod.yaml`):

```yaml
config:
  store:
    driver: postgres
  # policy stays external: never unless you opt in (see "Hybrid" below)

postgresql:
  external:
    existingSecret: differently-database
    existingSecretKey: database-url

auth:
  existingSecret: differently-tokens

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

agent:
  jailer:
    enabled: true
  priorityClassName: system-node-critical
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
  (init container `preflight`) and refuses to start on configuration errors.
- **Agents.** Each KVM node runs an agent pod. Its `doctor` init container
  blocks the agent until KVM, tun, Firecracker, the kernel and images check out.

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
`config.policy` at runtime.

## 7. Create an organization, a project and a project key

The bootstrap token is an operator key (`admin` scope). Use it to set up
tenancy, then use narrower keys day to day.

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
You can also open `$DIFFERENTLY_URL/dashboard/` and sign in with a key.

## 8. Create the first sandbox

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

If the sandbox stays `queued`, the placement explanation says why. Usual
causes:

- no healthy agent;
- no agent has enough free memory;
- the template is missing on every agent;
- policy excludes every pool.

## 9. Run the smoke test

The test Pod creates a sandbox, runs `echo ok` and deletes it. With the
bootstrap key it needs the project ID from step 7. Alternatively, put a
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
| `auth.existingSecret` | chart-managed | Tokens Secret (keys `bootstrap-token`, `agent-token`) |
| `providers.daytona/freestyle.enabled` | `false` | Register an external pool (still gated by `config.policy`) |
| `agent.updateStrategy` | `OnDelete` | Agent restarts stop that node's microVMs (see [day-2.md](day-2.md)) |
| `agent.privileged` | `true` | `false` needs a device plugin for `/dev/kvm` and `/dev/net/tun` (**unverified**) |
| `agent.reserveCPU` / `reserveMemoryMiB` | `1` / `2048` | Host headroom on KVM nodes |
| `networkPolicy.server.egress.restricted` | `true` | Server egress limited to DNS, agents, database and enabled providers |
| `metrics.serviceMonitor.enabled` | `false` | Prometheus Operator scrape of `/metrics` |
| `global.imageRegistry` | empty | Mirror registry for every image |

Exposure notes:

- The ingress publishes the API, the dashboard, the agent heartbeat endpoint
  (token-protected) and `/metrics` (unauthenticated) on one host. Block
  `/metrics` at the ingress controller if it must stay internal.
- The upstream ingress-nginx project has been retired. Prefer a maintained
  controller or Gateway API. An HTTPRoute pointing at
  `dly-differently-server:8080` works the same way.

## Verify

```sh
kubectl -n differently get pods -o wide
#   dly-differently-server-*   Running, READY 1/1 (per replica)
#   dly-differently-agent-*    Running on each differently.dev/kvm=true node
kubectl -n differently logs deploy/dly-differently-server -c preflight
kubectl -n differently logs ds/dly-differently-agent -c doctor
curl -fsS "$DIFFERENTLY_URL/readyz"                                    # {"status":"ready"}
curl -fsS "$DIFFERENTLY_URL/version"
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/agents" \
  | jq '.items[] | {id, pool, node, healthy, freeMemoryMiB}'           # one healthy agent per KVM node
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/trust-report" \
  | jq '{telemetry, globalPolicy: .globalPolicy.external, pools: [.pools[] | {name, trust, approvedForExternalPlacement}]}'
helm test dly -n differently --logs                                    # PASS: sandbox sbx_... ran 'echo ok'
```

The install worked when all of these hold:

- the agents are healthy;
- the trust report shows `external: never`, or your deliberate hybrid policy;
- the smoke test passes.
