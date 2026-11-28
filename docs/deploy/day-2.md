# Day-2 operations

Status: **documented**. None of these procedures has been rehearsed on a live
installation. The examples use namespace `differently`, release `dly`, and
`DIFFERENTLY_URL` and `DIFFERENTLY_API_KEY` set to an operator key (see
[kubernetes.md](kubernetes.md)).

Differently and OpenSandbox are installed, upgraded and removed separately.
This page covers Differently's control plane and how it relates to the
backends of its pools; OpenSandbox's own operations follow its documentation.

## Upgrade

### Differently

```sh
helm upgrade dly deploy/helm/differently -n differently -f values.yaml
```

The upgrade runs in this order:

1. **Schema check.** `values.schema.json` rejects invalid values before
   anything changes.
2. **Migrations.** The `pre-upgrade` hook Job runs
   `differently-server migrate` (idempotent). If it fails, the upgrade stops
   and the old pods keep serving. Read the logs with
   `kubectl -n differently logs job/dly-differently-migrate`. A failed Job is
   kept until the next attempt. With `postgresql.embedded`, migrations run
   instead as the `migrate` init container of each new server pod.
3. **Server rollout.** Server pods roll with `maxUnavailable: 0`. Each new pod
   must pass `preflight` and `/readyz` before an old one stops. With
   `config.store.driver=memory`, the strategy is `Recreate`, and all state is
   lost.

Sandboxes keep running during a control-plane upgrade: they live in
OpenSandbox (or another backend), not in the server pods. Requests for exec,
files and lifecycle fail while no server replica is ready.

Changing `config.*` or the `opensandbox` block restarts the server pods,
through a config checksum annotation. Changing `config.policy` in values does
not override a global policy stored through the API (`PUT /v1/policies/global`).
Check `GET /v1/policies` after policy changes.

Upgrading from a release that had node agents (chart 0.1.x): migration
`0002_drop_agents` drops the `agents` table, and the `firecracker` adapter no
longer exists, so a config that still declares a `firecracker` pool fails
validation. In values, `agent.*`, `auth.agentToken*` and
`config.scheduler.agentTimeoutSeconds` now fail the schema check. Releases
with node agents had no `opensandbox` adapter, so work cannot be moved
across beforehand: drain the `firecracker` pool, let its sandboxes end (or
delete them), remove the pool and those values, and upgrade with an
`opensandbox` block. The upgrade removes the agent DaemonSet; afterwards,
remove any systemd agents by hand and wipe `/var/lib/differently` on the
former KVM nodes (it held guest images and sandbox disks).

### OpenSandbox

Upgrade OpenSandbox with its own tooling, on its own schedule. Its published
API spec has run ahead of the images its server deploys by default (the
[spike](../research/opensandbox-spike.md) found four differences, all handled
by the adapter), so treat every upgrade as a contract change:

1. Keep the server and `execd` image versions pinned. The adapter was checked
   against server `release-1.2.0` with `execd` `release-1.2.0`
   (`lineEndings: kept`) and, in its first runs, `execd` `v1.1.0`.
2. Before upgrading production, run the adapter's live test against a staging
   server on the new versions. It creates and deletes real sandboxes:

   ```sh
   OPENSANDBOX_URL=https://opensandbox.staging.example.com/v1 OPENSANDBOX_API_KEY="$OSB_KEY" \
     go test -tags live -run TestLiveOpenSandbox ./internal/adapters/opensandbox/
   ```

3. Drain the pool if OpenSandbox's release notes say running sandboxes do
   not survive the upgrade (whether they do is **unverified** for any
   version).
4. After the upgrade, check the pool's health and run `helm test`.

Version pinning and upgrade testing are tracked in
[backlog E21](../backlog.md#e21-opensandbox-version-pinning-and-upgrade-testing).

## Replacing a backend

To move work from one backend to another, for example from an OpenSandbox
server with the Docker runtime to a new one with Fast Sandbox:

1. **Add the new pool** (a separate OpenSandbox server, with its own `apiURL`
   and API key) under `config.pools` or through `POST /v1/admin/pools`, and
   confirm it is healthy.
2. **Drain the old pool** so it takes no new placements. Running sandboxes
   continue:

   ```sh
   differently admin pool drain <old-pool>
   ```

3. **Wait for its sandboxes to end**, or delete them:

   ```sh
   differently admin pool get <old-pool> | jq .active     # 0 when nothing is left
   ```

4. **Remove the old pool**: drop it from values and upgrade (pools declared in
   config are written to the database at start-up and stay there), then
   `curl -X DELETE -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/pools/<old-pool>"`.
   The API refuses to delete a pool that is still declared in the config file.

Once the new pool is healthy, `differently admin pool use <new-pool>` can
replace step 2: it undrains the new pool and drains every other one in one
call
([switch new sandboxes between providers](../guides/keys-and-providers.md#switch-new-sandboxes-between-providers)).

Notes:

- With more than one server replica, a drain, undrain, `use` or new pool
  made through the API reaches the other replicas, including the scheduler
  leader, within about 5 seconds.
- Running sandboxes are never moved between pools. Snapshots stay on the pool
  that took them, and a fork from a snapshot is placed on that pool, so keep
  the old pool until you no longer need its snapshots, or delete them.
- Prefer one pool per OpenSandbox server. Two pools on the same server are
  safe: the reconciler leaves each pool's live sandboxes alone and deletes
  only instances left behind by an earlier placement. But they add no
  isolation, because the server has one API key.

## Move an installation to another cloud

To move a running installation to new infrastructure, for example from the
[Azure VMs layout](azure-vms.md) to [Google Cloud VMs](gcp-vms.md), build the
target first, then move the database and the keys in one short maintenance
window. Status: **documented, not run**. Like the rest of this page, it has
not been rehearsed on a live installation, and it has not been run from one
cloud to another.

### What moves and what does not

| | Moves with the database | Moves by hand | Does not move |
|---|---|---|---|
| Installation ID, organizations, projects, API keys (hashes), policies, pools, usage, audit chain | ✓ | | |
| Sandbox records | ✓ | | |
| Provider credentials stored in Differently (`secret:`) | ✓, sealed | The **secrets key** that opens them, byte for byte | |
| Bootstrap token | Its operator key's hash | The token itself, so the key stays the same ([rotation](#token-and-key-rotation)) | |
| Sandboxes on external providers (Freestyle, Daytona, ...) | | | Nothing to move: they keep running, and the new control plane manages them through the same records |
| Sandboxes on your own backends (OpenSandbox servers), their snapshots and templates | | | They stay on the old backend. Templates are built again on the new one |
| OpenSandbox API keys and `file:` credentials | | New servers, new keys: store them on the target | |

API keys keep working, so clients need only the new address. If the API's
host name stays the same and only its DNS record changes, they need nothing.

### Never run two control planes on one database's data

A restored database carries the installation ID. Every sandbox Differently
creates is labelled with it, and the reconciler deletes any instance with
this installation's label that has no live record
([backup and restore](#backup-and-restore-postgresql)). Two control planes
started from copies of the same data, with access to the same backends,
therefore each delete the sandboxes the other created after the copy was
taken. External providers are shared by both clouds, so this is the normal
case, not an edge case. Hence:

- Stop the source's servers before the target's servers start, and keep
  them stopped (`--replicas=0`).
- To rehearse a restore while the source is still running, install the
  target with `server.replicas: 0`: the chart's migration Job runs against the
  restored copy, and no reconciler starts. Count rows, then drop that
  database again.
- To roll back after the target has taken writes, restore the target's
  database into the source, not the source's old one (see
  [rollback](#rolling-back-the-move)).

### Choose how private sandboxes end

Sandboxes on the source's own backends cannot follow the database. Pick one:

- **Drain first (no running sandbox is cut off).** Weeks before the window,
  register the target's OpenSandbox servers as new pools **on the source
  control plane**: `POST /v1/admin/pools` or `config.pools`, over HTTPS to an
  address the target exposes only to the source's egress IP, with the target
  server's own key. Then `differently admin pool drain <old-pool>` for each
  of the source's private pools. New sandboxes start on the target while the
  old ones run to their end, as in [replacing a backend](#replacing-a-backend).
  When `differently admin pool get <old-pool> | jq .active` is 0, delete the
  old pools. Do not use `pool use` here, because it also drains the external
  pools. This costs a cross-cloud hop on every call to the new pools until
  the window, and the target's OpenSandbox API exposed to one address.
- **Cut over in one window (simpler).** Sandboxes still on the source's
  private pools are lost at the window. After the move their records point
  at a backend that no longer has them, and the reconciler marks them
  failed. Tell their owners first, or delete them yourself.

### The window

Run the **same Differently version** on both sides; upgrade before or after
the move, not during it. Lower the TTL of the DNS records you will change a
day ahead.

1. **Stop the source.**

   ```sh
   kubectl -n differently scale deploy/dly-differently-server --replicas=0
   kubectl -n differently get pods -l app.kubernetes.io/component=server   # none left
   ```

   The API is down from here until the DNS change.
2. **Dump** from a host that reaches the source database (the source's
   control node, for a private database):

   ```sh
   pg_dump --format=custom --no-owner --no-acl -d "$SOURCE_DATABASE_URL" -f differently.dump
   ```

   Use a `pg_dump` at least as new as the source server. The dump holds key
   hashes and sealed credentials: copy it to the target over SSH, never
   through a bucket with public access, and `shred` it when done.
3. **Restore** into the target's empty database, as its application user:

   ```sh
   pg_restore --no-owner --no-acl --exit-on-error -d "$TARGET_DATABASE_URL" differently.dump
   ```

   `--no-owner --no-acl` drops the source cloud's own roles (such as
   `azure_pg_admin`), which the target does not have. Differently's schema
   needs no extension and no superuser.
4. **Keys.** The target's secret store holds the source's secrets key and
   bootstrap token exactly, without a trailing newline, and the target's own
   OpenSandbox keys.
5. **Start the target** (`helm install`, or `helm upgrade` with
   `server.replicas` back at its usual value). The migration Job finds the
   schema current.
6. **Check it before anyone else does**, against the target's own address:

   ```sh
   curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$TARGET_URL/v1/admin/audit/verify" | jq .
   curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$TARGET_URL/v1/admin/pools" \
     | jq '.items[] | {name, trust, draining, health: .health.status}'
   curl -fsS -X POST -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$TARGET_URL/v1/admin/reconcile"
   ```

   Pools with `secret:` credentials must be healthy. One that shows
   `secret <name> cannot be opened with this key` means the secrets key
   differs from the source's: stop and fix the key, rather than storing the
   credentials again. Then create, use and delete one sandbox on each pool. A pool that the
   database still holds from the source and that points at a backend the
   target cannot reach shows as unavailable. Delete it once its sandboxes
   are gone.
7. **Switch DNS** for the API host, and for the sandbox host if the target
   reuses it (only after the one-window cut-over, since a drained-first
   target already serves its own sandbox host).

### Rolling back the move

- **Before the target has taken writes** (before the DNS change, or with
  no traffic yet): scale the target's servers to 0, scale the source's back
  up, and point DNS back. The source's database was never changed.
- **After the target has taken writes:** scale the target's servers to 0
  first. Dump the target's database and restore it into the source's with
  `pg_restore --clean`, then start the source and point DNS back. Sandboxes
  the target created on its own private pools are reachable from the source
  only if it can reach those servers. Never start the source on its old
  database once the target has created sandboxes on external providers:
  its reconciler would delete them.

### Decommission

Keep the source stopped, with its database, for as long as you may roll
back. Then take a last dump, store it with the secrets key's backup, and
delete the source's resources. Back up the secrets key in the target's
secret store before you delete the source's
([back up the secrets key](../guides/keys-and-providers.md#back-up-the-secrets-key)).

## Rollback

```sh
helm history dly -n differently
helm rollback dly <revision> -n differently
```

`helm rollback` does not run the migration hook, and the database schema is
not rolled back. Before an upgrade that migrates the schema, take a database
backup. If the older server cannot read the newer schema, restore that backup
(below). Whether a given release's migrations are backward compatible is
something to check in its release notes. This chart makes no promise.
Rolling back to a release with node agents also needs those agents and their
`agents` table, which migration `0002_drop_agents` removed.

## Migrations by hand

Migrations normally run by themselves. The simplest way to run them on
demand, for example after a restore, is to re-run the hook:

```sh
helm upgrade dly deploy/helm/differently -n differently --reuse-values
```

Or apply a one-off Job that uses the release's ConfigMap and database Secret.
Adjust the Secret name and key for `postgresql.external.existingSecret`:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: dly-migrate-manual
  namespace: differently
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: migrate
          image: ghcr.io/different-ai/differently-server:0.1.0-alpha.9
          args: ["migrate", "--config", "/etc/differently/config.yaml"]
          env:
            - name: DIFFERENTLY_DATABASE_URL
              valueFrom: {secretKeyRef: {name: differently-database, key: database-url}}
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
          volumeMounts: [{name: config, mountPath: /etc/differently, readOnly: true}]
      volumes: [{name: config, configMap: {name: dly-differently-config}}]
```

```sh
kubectl apply -f migrate-manual.yaml
kubectl -n differently logs -f job/dly-migrate-manual
kubectl -n differently delete job dly-migrate-manual
```

## Backup and restore (PostgreSQL)

PostgreSQL holds everything durable:

- the installation ID;
- organizations, projects, API keys (hashes only);
- policies and API-created pools;
- sandbox records and snapshots metadata;
- usage;
- the audit chain.

Back up these alongside it:

- the bootstrap token Secret (`dly-differently-tokens`, or your `auth.existingSecret`);
- the OpenSandbox API key Secret and provider credential Secrets;
- your values file;
- `audit.file` exports, if you use them.

OpenSandbox's own state (sandboxes, snapshots, templates) is backed up, or
not, by OpenSandbox's tooling.

| Database | Backup |
|---|---|
| Managed (RDS, Cloud SQL, Azure Flexible Server) | Automated backups with point-in-time recovery; snapshots before upgrades |
| CloudNativePG | A `ScheduledBackup` to object storage (in recent CNPG releases through the Barman Cloud plugin), plus on-demand `Backup` before upgrades |
| Any | Logical dump: `pg_dump --format=custom` |
| `postgresql.embedded` (evaluation only) | `kubectl -n differently exec sts/dly-differently-postgresql -- pg_dump -U differently -Fc differently > differently.dump` |

To restore:

```sh
kubectl -n differently scale deploy/dly-differently-server --replicas=0
# restore with your database's tooling (PITR, CNPG recovery cluster, or pg_restore --clean)
helm upgrade dly deploy/helm/differently -n differently --reuse-values   # migrations, then servers start
curl -fsS -X POST -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/reconcile"
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/audit/verify" | jq .
```

A restore rewinds state:

- **Keys.** Keys created after the backup no longer exist.
- **Stored provider credentials.** They return to their values at backup
  time. Store again any key rotated since then, or the pool uses a key the
  provider may have revoked. Opening them needs the same secrets key as
  before.
- **Sandboxes.** Records may point at sandboxes that are gone from the
  backend; the reconciler marks those failed. Sandboxes created after the
  backup have no record any more; the reconciler finds them on the backend
  (they carry this installation's ID) and **deletes them as orphans**, with
  an audit event `reconcile.orphan_deleted`.
- **Audit chain.** The verify call confirms the chain is intact up to the
  restore point.

## Certificates

- **Ingress TLS with cert-manager:** renewal is automatic. Check it with
  `kubectl -n differently get certificate` and
  `kubectl -n differently describe certificate dly-differently-tls`. Force a
  renewal with `cmctl renew dly-differently-tls -n differently`.
- **Ingress TLS from your own CA:** replace the Secret named in
  `ingress.tls.secretName`. Most controllers reload it without a restart.
- **Database TLS:** for `sslmode=verify-full`, mount the CA bundle with
  `server.extraVolumes` and `server.extraVolumeMounts`, and reference it in
  the DSN (`sslrootcert=/etc/differently-db-ca/ca.pem`). When the provider
  rotates its CA, update that Secret or ConfigMap and restart the server
  (`kubectl rollout restart deploy/dly-differently-server`).
- **OpenSandbox over `https`:** if its certificate is issued by a private CA,
  the server pods must trust that CA; how to add it to the server image's
  trust store is not covered by the chart (**unverified**).

### Token and key rotation

**Bootstrap token.** Keep the `dly_<id>_` prefix and change only the secret
part:

1. Update the Secret, or `auth.bootstrapToken`.
2. Restart the server. On start it re-hashes the operator key (audit event
   `key.rotate`), and the old token stops working.
3. If you change `<id>`, a second operator key is created and the old one
   stays valid. Revoke it with `DELETE /v1/api-keys/<old-id>`.

Prefer named operator keys for daily work, and keep the bootstrap token
offline, or revoke it
([the bootstrap operator token](../guides/keys-and-providers.md#the-bootstrap-operator-token)).

**API keys.** Rotate a key with a grace period, so callers can switch
without downtime:

```sh
differently key rotate <key-id> --grace 1h    # the new token is shown once
```

The old key keeps working for the grace period (at most 7 days). See
[rotate Differently API keys](../guides/keys-and-providers.md#rotate-differently-api-keys-without-downtime).

**Provider keys stored in Differently** (`secret:` credentials). Replace
the value with `differently admin secret put <name> --from-file new.txt`.
Every replica uses it within about 5 seconds, with no restart. The same
holds for a `file:` credential once the mounted file changes
([rotate a provider key live](../guides/keys-and-providers.md#rotate-a-provider-key-live)).

**OpenSandbox API key.** OpenSandbox has one server-wide key, so there is no
overlap window. Change the key in OpenSandbox's configuration, update the
Secret Differently reads it from, and restart the server pods
(`kubectl -n differently rollout restart deploy/dly-differently-server`); the
key is read from the environment, which only changes on restart. Calls to the
pool fail between the two changes, and the pool shows as unavailable. If the
pool's key is stored in Differently instead, `differently admin secret put`
replaces the restart.

**Secrets key.** It cannot be rotated yet: changing it makes every stored
provider credential unreadable. Back it up instead
([back up the secrets key](../guides/keys-and-providers.md#back-up-the-secrets-key)).

## Node maintenance and drain

- `kubectl drain <node>` evicts server pods (the PDB keeps one replica's
  worth available). Differently's pods hold no sandbox state.
- Nodes that run sandboxes belong to OpenSandbox's runtime. What happens to
  sandboxes on a drained or rebooted node depends on that runtime (and is
  **unverified** for all of them). To be safe:

  1. Drain the pool (`differently admin pool drain <pool>`), or wait for its
     sandboxes to end.
  2. Follow OpenSandbox's procedure for the node.
  3. Undrain the pool (`differently admin pool undrain <pool>`).

  A sandbox that disappears from the backend is marked `failed` by the
  reconciler.

## Removing an external provider

Pools declared in config are written to the database at start-up, and they
stay there when you remove them from values. To remove one:

1. Set `config.policy.external: never`, or drop the pool from
   `config.policy.externalPools`, and upgrade. Placement stops at once.
2. Delete or terminate the sandboxes still on the pool. They keep running
   (and billing) at the provider until you do.
3. Set `providers.<name>.enabled=false` and upgrade.
4. `curl -X DELETE -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/pools/<name>"`.

## Uninstall and data retention

Before uninstalling:

1. Delete the sandboxes you no longer need through the API. Uninstalling
   Differently does not stop them: sandboxes on OpenSandbox keep running until
   deleted there or until OpenSandbox's own timeout (set from each sandbox's
   TTL, at least 60 s) ends them, and sandboxes on external pools keep running
   at the provider until deleted.
2. Export audit events (`GET /v1/audit-events`) if you must retain them.

```sh
helm uninstall dly -n differently
```

| Removed by `helm uninstall` | Kept |
|---|---|
| Server Deployment, Services, Ingress, ConfigMap, NetworkPolicies, PDB, ServiceMonitor, ServiceAccount | `dly-differently-tokens` Secret (`auth.keepSecretOnUninstall`, `helm.sh/resource-policy: keep`) |
| | `dly-differently-secrets-key` Secret, when the chart generated it (`helm.sh/resource-policy: keep`; without it, stored provider credentials cannot be read) |
| | `dly-differently-postgresql` Secret and its PVC `data-dly-differently-postgresql-0` (embedded database only) |
| | Your external database and every Secret you created yourself |
| | OpenSandbox and everything in it: sandboxes, their disks (which may hold workload data), snapshots |

To reinstall onto the same data, use the same release name and namespace.
Helm adopts the kept Secrets, so the bootstrap token, the secrets key and
the embedded database password still match. A reinstall with a new, empty database gets a
new installation ID, so it neither sees nor cleans up sandboxes the old
installation left in OpenSandbox; delete those with OpenSandbox's tooling.

To remove everything (irreversible):

```sh
kubectl -n differently delete secret dly-differently-tokens dly-differently-secrets-key dly-differently-postgresql --ignore-not-found
kubectl -n differently delete pvc -l app.kubernetes.io/instance=dly
kubectl delete namespace differently
# then uninstall OpenSandbox and remove its data with its own tooling
```

Drop or retain the external database according to your data-retention policy.

## Verify

After any of these procedures:

```sh
helm status dly -n differently
kubectl -n differently get pods                                          # server Ready
curl -fsS "$DIFFERENTLY_URL/readyz" && curl -fsS "$DIFFERENTLY_URL/version"
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/pools" \
  | jq '.items[] | {name, trust, draining, active, health: .health.status}'
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/audit/verify" | jq .
helm test dly -n differently --logs
```

After an uninstall, verify the opposite:

```sh
helm list -n differently               # no release
kubectl get ns differently             # NotFound once you deleted it
```

Then check OpenSandbox for sandboxes you meant to remove.
