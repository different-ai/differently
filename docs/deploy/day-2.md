# Day-2 operations

Status: **documented**. None of these procedures has been rehearsed on a live
installation. The examples use namespace `differently`, release `dly`, and
`DIFFERENTLY_URL` and `DIFFERENTLY_API_KEY` set to an operator key (see
[kubernetes.md](kubernetes.md)).

## Upgrade

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
4. **Agents stay put.** Agents do not restart: `agent.updateStrategy` is
   `OnDelete`. Roll them node by node (below).

Changing `config.*` restarts the server pods, through a config checksum
annotation. Changing `config.policy` in values does not override a global
policy stored through the API (`PUT /v1/policies/global`). Check
`GET /v1/policies` after policy changes.

### Roll agents node by node

Assume an agent restart stops the microVMs on that node: they run in the
agent pod's cgroup. Whether a restarted agent re-adopts running VMs is
**unverified**.

There is no per-node drain in the API yet. Pool drain (`POST
/v1/admin/pools/{name}/drain`) stops new placements on the whole pool, while
running sandboxes continue.

```sh
POOL=firecracker
# 1. Stop new placements on the pool (skip this on a large pool and accept that
#    the node's sandboxes end).
curl -fsS -X POST -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/pools/$POOL/drain"

# 2. For each node: wait until its agent reports 0 sandboxes, then replace the pod.
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/agents?pool=$POOL" \
  | jq '.items[] | {node, sandboxes, version}'
kubectl -n differently delete pod -l app.kubernetes.io/component=agent --field-selector spec.nodeName=<node>
kubectl -n differently rollout status ds/dly-differently-agent      # new pod passes doctor and is Ready

# 3. Allow placements again.
curl -fsS -X POST -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/pools/$POOL/undrain"
```

To roll automatically instead, set
`agent.updateStrategy={type: RollingUpdate, rollingUpdate: {maxUnavailable: 1}}`
and accept that each node's sandboxes end during its turn.

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
          image: ghcr.io/different-ai/differently-server:0.1.0
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

- organizations, projects, API keys (hashes only);
- policies and API-created pools;
- sandbox records and snapshots metadata;
- usage;
- the audit chain.

Back up these alongside it:

- the tokens Secret (`dly-differently-tokens`, or your `auth.existingSecret`);
- provider credential Secrets;
- your values file;
- `audit.file` exports, if you use them.

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
- **Sandboxes.** Sandbox records may point at microVMs that are gone, or miss
  VMs that are still running. The reconcile pass repairs the difference.
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

### Token rotation

**Bootstrap token.** Keep the `dly_<id>_` prefix and change only the secret
part:

1. Update the Secret, or `auth.bootstrapToken`.
2. Restart the server. On start it re-hashes the operator key (audit event
   `key.rotate`), and the old token stops working.
3. If you change `<id>`, a second operator key is created and the old one
   stays valid. Revoke it with `DELETE /v1/api-keys/<old-id>`.

Prefer named operator keys for daily work, and keep the bootstrap token
offline.

**Agent token.** There is one shared token and no overlap window. Update the
Secret, restart the server and every agent together, and update
`/etc/differently/agent.env` on systemd hosts. Heartbeats fail between the
two restarts. Agent pods restart under `OnDelete` only when you delete them,
which also ends their microVMs.

## Node maintenance and drain

- `kubectl drain <node>` evicts server pods (the PDB keeps one replica's
  worth available) and skips DaemonSet pods with `--ignore-daemonsets`.
- On a KVM node the agent keeps running until the node shuts down, and the
  node's microVMs end then. Plan maintenance as you would an agent roll:

  1. Drain the pool, or wait for the node's sandbox count to reach 0.
  2. `kubectl cordon <node>` and `kubectl drain <node> --ignore-daemonsets --delete-emptydir-data`.
  3. Do the maintenance, then `kubectl uncordon <node>`.
  4. Undrain the pool.

- Removing a node permanently: also remove the `differently.dev/kvm` label
  and wipe `/var/lib/differently` on it (see below). The agent record ages
  out of placement when heartbeats stop.

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

1. Delete the sandboxes you no longer need through the API. Sandboxes on
   external pools keep running at the provider until deleted.
2. Export audit events (`GET /v1/audit-events`) if you must retain them.

```sh
helm uninstall dly -n differently
```

| Removed by `helm uninstall` | Kept |
|---|---|
| Server Deployment, agent DaemonSet (this stops every microVM), Services, Ingress, ConfigMap, NetworkPolicies, PDB, ServiceMonitor, ServiceAccount | `dly-differently-tokens` Secret (`auth.keepSecretOnUninstall`, `helm.sh/resource-policy: keep`) |
| | `dly-differently-postgresql` Secret and its PVC `data-dly-differently-postgresql-0` (embedded database only) |
| | Your external database and every Secret you created yourself |
| | `/var/lib/differently` on every KVM node: guest images, agent state and **sandbox disks, which may hold workload data** |

To reinstall onto the same data, use the same release name and namespace.
Helm adopts the kept Secrets, so the tokens and the embedded database
password still match.

To remove everything (irreversible):

```sh
kubectl -n differently delete secret dly-differently-tokens dly-differently-postgresql --ignore-not-found
kubectl -n differently delete pvc -l app.kubernetes.io/instance=dly
kubectl delete namespace differently
# on each KVM node, after the agent pod is gone:
sudo rm -rf /var/lib/differently
# systemd hosts:
sudo systemctl disable --now differently-agent && sudo rm -rf /var/lib/differently /etc/differently \
  /etc/systemd/system/differently-agent.service && sudo systemctl daemon-reload
```

Drop or retain the external database according to your data-retention policy.

## Verify

After any of these procedures:

```sh
helm status dly -n differently
kubectl -n differently get pods                                          # server Ready, one agent per KVM node
curl -fsS "$DIFFERENTLY_URL/readyz" && curl -fsS "$DIFFERENTLY_URL/version"
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/agents" | jq '.items[] | {node, healthy, version}'
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/pools" | jq '.items[] | {name, trust, draining}'
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/audit/verify" | jq .
helm test dly -n differently --logs
```

After an uninstall, verify the opposite:

```sh
helm list -n differently               # no release
kubectl get ns differently             # NotFound once you deleted it
```

On each former KVM node, `/var/lib/differently` should be absent.
