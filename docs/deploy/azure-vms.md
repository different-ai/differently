# Azure VMs with Fast Sandbox microVMs

A production-shaped install on two Azure VMs: k3s, OpenSandbox with both its
Kubernetes runtime (containers) and Fast Sandbox (Firecracker microVMs),
Differently in front, and managed services around it. It is the layout that
serves OpenWork's Workbot computers ([OpenWork guide](../guides/openwork.md)),
with Freestyle and Daytona as external pools.

> **Verified 2026-10-29** in `eastus`: k3s v1.36.5, OpenSandbox 1.2.0,
> Differently `pre-release` after `v0.1.0-alpha.2`. Workbot's desktop ran on
> Fast Sandbox through Differently, driven by OpenWork's runner; the adapter's
> live scenario and the end-to-end tests listed in [Verify](#verify) passed.
> One node per role: not highly available.

| Piece | Choice |
|---|---|
| Control node | `Standard_D8s_v6`: k3s server, Differently (2 replicas), OpenSandbox servers, container sandboxes |
| KVM node | `Standard_D16s_v6` with a 256 GiB Premium SSD (XFS, reflink) for Fast Sandbox: microVMs, template builds, artifact store |
| Database | Azure Database for PostgreSQL Flexible Server, private access |
| Snapshot registry | Azure Container Registry (Standard), a repository-scoped token |
| Secrets | Key Vault through the Secrets Store CSI driver, the control node's managed identity |
| Ingress | Caddy on each node with Let's Encrypt: the API on one host name, sandbox port URLs on another |

## 1. Network, VMs and quota

- **Nested virtualization.** `Dsv6` sizes expose `/dev/kvm` to Ubuntu 24.04
  with security type `Standard`; check with `ls -l /dev/kvm` on the VM.
- **Quota.** A new subscription allows 10 regional vCPUs; this layout needs
  24. `az quota update --resource-type dedicated` raised `cores` and
  `StandardDsv6Family` at once (register `Microsoft.Quota` first).
- **Postgres region.** Some subscriptions may not create Flexible Server in
  every region (`az postgres flexible-server list-skus -l <region>` says
  "Provisioning is restricted"). Put the server in a nearby region's VNet and
  peer it with the nodes' VNet; pass the private DNS zone by resource ID and
  link it to both VNets.
- **Two public IPs**, one per node, each with a DNS label. Sandbox content
  must not share an origin with the API and dashboard, so the sandbox gateway
  gets its own host name.
- **NSG:** 443 and 80 (ACME) from anywhere, 22 from operators only. Port URLs
  are opened by people's browsers and the API by OpenWork's runner, so 443
  is public; every API call still needs a key.

Format the KVM node's data disk as XFS with reflink and mount it at
`/var/lib/fast-sandbox`; Fast Sandbox's node check refuses anything else.

## 2. k3s and registry access

Install k3s with `--disable traefik --disable servicelb` on the control node
and join the KVM node as an agent. Label them (for example
`differently.dev/role=control` and `sandbox-runtime=fast-sandbox`).

Give both nodes ACR credentials in `/etc/rancher/k3s/registries.yaml`. The
token's scope map needs wildcards: the image committer pushes each sandbox's
snapshot to its own repository under `opensandbox-snapshots/`, so a scope on
`opensandbox-snapshots` alone fails with `insufficient_scope`.

```sh
az acr scope-map create -r <acr> -n cluster \
  --repository 'opensandbox-snapshots/*' content/read content/write content/delete metadata/read metadata/write \
  --repository 'images/*' content/read metadata/read
az acr token create -r <acr> -n cluster --scope-map cluster
```

## 3. OpenSandbox, Kubernetes runtime

As in [single-vm.md](single-vm.md#2-install-opensandbox), with these values:

- controller: `snapshot.registry: <acr>/opensandbox-snapshots`,
  `snapshotPushSecret` and `resumePullSecret` naming a docker-registry Secret
  with the ACR token (in the `opensandbox` namespace);
- server: `gateway.host` is the sandbox host name, route mode `uri`, Secure
  Access keys; `max_sandbox_timeout_seconds` at least the longest TTL clients
  ask for (OpenWork keeps computers 14 days: `2592000` covers it);
- the ingress gateway chart in route mode `uri` with the same keys.

## 4. Fast Sandbox

Fast Sandbox has its own lifecycle server and ingress gateway (separate
releases, here `opensandbox-server-fsb` and `opensandbox-ingress-gateway-fsb`)
and needs:

- **An artifact store** for templates and checkpoints. Garage (v2.4.1) on the
  KVM node's data disk, with zstd compression and block dedup, keeps a 14 GB
  desktop checkpoint to about 1.3 GiB. Give its Service a fixed `clusterIP`:
  the node agent's registry config names it by address.
- **A guest kernel with nftables.** The stock kernel lacks nftables' `inet`
  family, `reject` and the owner match. Build Firecracker CI's 6.18.36 config
  with `NF_TABLES_INET`, `NF_TABLES_IPV6`, `NFT_REJECT`, `NFT_REJECT_INET`,
  `NFT_LOG`, `NFT_SOCKET`, `NFT_FIB_*`, `NETFILTER_XT_MATCH_OWNER` and
  `NETFILTER_XT_MATCH_MULTIPORT` set to `y` (about five minutes on the D16s
  node), copy it over `vmlinux.bin` in a derived
  `fsb-sandboxtemplate-builder:release-1.2.0` image, and set
  `controller.sandboxtemplateBuilderImage` to it. Templates built afterwards
  use it.
- **A source registry the template builder can read anonymously.** The
  builder reads no pull secrets, so copy template images from ACR to a
  `registry:2` bound to the KVM node's private address (never exposed by the
  NSG). Pull them into a separate containerd namespace first
  (`k3s ctr -n stage images pull --user ...`): k3s discards layers it has
  unpacked, so pushing from `k8s.io` fails with `content digest not found`.
- **The idle sweeper** (`[automation.idle_pause]` with Redis) and the
  gateway's activity tracking and wake-on-access.
- **The template store on a volume**: mount a hostPath at the Fast Sandbox
  server's `/root/.opensandbox`, or a restart forgets every template.
- **A warm SandboxPool.** With `poolMin` below `poolMax`, a create that needs a
  new fastlet fails with `no eligible Fastlet` instead of waiting. On a
  D16s_v6, three fastlets of two 2 vCPU / 4 GiB sandboxes each fit; set the
  Differently pool's `maxConcurrency` to match.
- **One desktop-sized template per node.** Fast Sandbox 1.2.0 caps each
  node's converted-template cache at 20 GiB; a second 14 GB template evicts
  the first, and the evicted one fails with `RuntimeNotReady ... no committed
  restore set` instead of being pulled again. Delete superseded templates.

Create templates with an explicit `entrypoint` (the image's own is not run)
and, for a desktop, a `readiness.probe` (`cmd://...`) so the snapshot is taken
once it is up. OpenWork's runner README has the Workbot template's settings.

## 5. Key Vault through the CSI driver

On VMs (not AKS), install `csi-secrets-store-provider-azure` with Helm and let
it authenticate as a user-assigned identity on the control node
(`useVMManagedIdentity: "true"`, `userAssignedIdentityID`). Assign the
identity only to that node. Three things to know:

- If your account cannot create role assignments, create the vault with
  access policies (`--enable-rbac-authorization false`); a vault cannot be
  switched without that right.
- The CSI driver syncs a Kubernetes Secret only while some pod mounts the
  volume, and the chart's pre-install migration Job reads the database Secret
  before any server pod exists. Run a small Deployment (a `pause` container)
  that mounts the volume, so the Secret exists first and stays fresh.
- Store values without a trailing newline (`printf '%s'`): `--file` keeps it,
  and a token or key with a newline no longer matches.

The chart reads the synced Secret for the database URL, bootstrap token,
secrets key (pin its `objectVersion`) and OpenSandbox key; a second
OpenSandbox server's key can be a `file:` credential on the mount. Provider
keys (Freestyle, Daytona) are stored in Differently (`secret:`), so they
rotate without a restart.

Sandboxes must not reach the instance metadata service, or a container on
the control node could read the vault through its identity. The
NetworkPolicies below cover that.

## 6. Differently

Install the chart with `config.store.driver: postgres`, two replicas pinned
to the control node, `secrets.existingSecret` and `postgresql.external`
from the synced Secret, the CSI volume in `server.extraVolumes`, and pools:

```yaml
opensandbox:                       # container pool on the Kubernetes runtime
  runtime: kubernetes
  ports: signed
  portScheme: https
  lineEndings: kept
config:
  pools:
    - name: opensandbox-microvm
      adapter: opensandbox
      trust: private
      maxConcurrency: 4
      credentials: {file: /mnt/kv/osb-fsb-api-key}
      config:
        apiURL: http://opensandbox-server-fsb.opensandbox-system.svc/v1
        runtime: fastsandbox
        isolation: microvm
        templateId: <the template>
        poolCPU: "2"
        poolMemory: 4Gi            # equal to the SandboxPool's sandboxResources
        portScheme: https
        lineEndings: kept
        features: exec,exec.stream,files,lifecycle.pause-resume,snapshots.disk,fork,ports.expose
    - {name: freestyle, adapter: freestyle, trust: external, maxConcurrency: 20, credentials: {secret: freestyle-key}, config: {egress: public}}
    - {name: daytona, adapter: daytona, trust: external, maxConcurrency: 10, credentials: {secret: daytona-key}, config: {target: us, class: container, snapshot: daytona-medium}}
  policy: {external: overflow, externalPools: [freestyle], strategy: private-first}
networkPolicy:
  server:
    egress:
      databaseCIDRs: [<the database subnet>]
      extra:                       # Fast Sandbox execd through the public sandbox host
        - to: [{ipBlock: {cidr: <KVM node public IP>/32}}]
          ports: [{port: 443, protocol: TCP}]
```

Fast Sandbox execd is reached past the server proxy, through the ingress
gateway's public URL, so the server needs egress to the sandbox host.

## 7. Ingress and sandbox network policy

Caddy runs as a host-network Deployment on each node and proxies with
`flush_interval -1` (streamed exec and WebSockets): the API host to
`dly-differently-server:8080` on the control node; the sandbox host on the
KVM node, `/fsb/*` (prefix stripped) to the Fast Sandbox gateway and
everything else to the Kubernetes runtime's gateway. Set the Fast Sandbox
server's `gateway.host` to `<sandbox host>/fsb`.

NetworkPolicies, enforced by k3s:

- namespace `opensandbox` (container sandboxes): egress to kube-dns and to
  `0.0.0.0/0` except `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
  `169.254.0.0/16`, `100.64.0.0/10` and `168.63.129.16/32`;
- namespace `opensandbox-dataplane` (fastlets carry microVM traffic): no
  `169.254.0.0/16`, `168.63.129.16/32` or the VNets;
- template build pods (label `sandbox.fast.io/sandboxtemplate`) also need
  the API server (6443) and the source registry, or builds fail when they
  report their result.

## Verify

Checked on this layout:

| Check | Result |
|---|---|
| Container sandbox egress | metadata service, wire server, Postgres, Kubernetes API and in-cluster Redis refused; the internet reachable |
| Container pool stop/start through ACR | stop 77 s on the first push of the base layers, start 3 s, disk kept |
| Fast Sandbox create, template on the node | about 2 s through the public API |
| Workbot desktop on Fast Sandbox | its nftables egress guard loads; desktop and browser stream through the gateway's WebSocket |
| Pause / resume, 2 vCPU / 4 GiB desktop | 174–186 s / under 1–2 s, processes and `/dev/shm` kept |
| Fork | snapshot about 180 s, the fork running about 85 s later with the parent's disk, memory and processes |
| Adapter live scenario (`fastsandbox-1.2.0`) | passed in 9 min 27 s |
| Freestyle through Differently, Workbot snapshot | create 4 s, pause 44 s, resume 3 s, process kept |
| End-to-end suite on the microVM pool | 14 of 14 run passed: limits, exec, files, pause and resume (memory, processes and `/dev/shm` kept), snapshot and fork, memory snapshot, port URL, idle pause and wake by a port request (2.3 s), quotas, delete, drift, audit and metrics, capability errors. Not run: the e2e desktop image (Workbot's desktop streamed instead), unpullable images (templates), Differently's own idle sweep (Fast Sandbox idles natively), provider switching (it drains pools) |

To run the suite against such an install, set `DIFFERENTLY_E2E_POOL` to the
microVM pool and `DIFFERENTLY_E2E_TEMPLATE` / `DIFFERENTLY_E2E_PYTHON_TEMPLATE`
to its template, and point `DIFFERENTLY_E2E_METRICS_URL` at the scheduler
leader's pod, not the Service: with two replicas only the leader counts
transitions.
