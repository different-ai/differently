# Bare metal: k3s, RKE2, Talos and plain Linux hosts

Status: k3s, RKE2 and the systemd agent are **documented**; Talos is
**unverified**. Nothing here has been run on real hardware yet.

Bare metal is the most natural home for Firecracker: `/dev/kvm` comes
straight from the CPU, with no nested virtualization. Enable VT-x/AMD-V (and
VT-d if you like) in firmware, and confirm the host checks in
[prerequisites.md](prerequisites.md).

## Kubernetes distributions

The chart needs only a privileged DaemonSet with host paths. Each distribution
differs mainly in Pod Security Admission (PSA) defaults.

### k3s

- k3s embeds containerd 2.0 (since v1.31.6 / v1.32.2). Its NetworkPolicy
  controller enforces the chart's policies.
- **No PSA enforcement by default**, so the privileged agent runs as is.
  Labelling the namespace is still good practice.
- If you followed the k3s hardening guide, `/var/lib/rancher/k3s/server/psa.yaml`
  sets `restricted` as the cluster-wide default. Label the namespace
  `pod-security.kubernetes.io/enforce=privileged` (namespace labels override
  the defaults), or add `differently` to its `exemptions.namespaces`.
- k3s ships RuntimeClasses for crun, nvidia and Wasm, but not Kata. Differently
  needs none of them.

```sh
kubectl label node <node> differently.dev/kvm=true
kubectl taint node <node> differently.dev/kvm=true:NoSchedule
kubectl create namespace differently
kubectl label namespace differently pod-security.kubernetes.io/enforce=privileged
helm install dly deploy/helm/differently -n differently -f values.yaml
```

### RKE2

- The default PSA level is `privileged`.
- With the **CIS profile**, RKE2 enforces `restricted` and exempts only
  `kube-system`, `compliance-operator-system` and `tigera-operator`. Add `differently`
  to the exemptions in a custom PSA configuration file, set through the
  `pod-security-admission-config-file` option.
- How SELinux on RHEL-family hosts interacts with the privileged agent, the
  `/var/lib/differently` hostPath and `/dev/kvm` is **unverified**. If you
  see AVC denials, check `ausearch -m avc`.
- The containerd config template lives at
  `/var/lib/rancher/rke2/agent/etc/containerd/config-v3.toml.tmpl`.
  Differently does not need containerd changes.

### Talos

- Talos is immutable and enforces PSA `baseline` by default, which blocks
  privileged pods outside `kube-system`. Label the namespace:
  `kubectl label namespace differently pod-security.kubernetes.io/enforce=privileged`.
- Host packages come from system extensions, not package installs. The
  Differently agent image brings its own Firecracker binaries, so no extension
  should be needed.
- **Unverified:** whether the Talos kernel loads `kvm_intel` or `kvm_amd`
  and `tun` without extra machine-config `kernel.modules` entries, and whether
  Firecracker runs from a container on Talos without additional kernel
  modules.
- Talos on bare metal needs firmware virtualization. A Talos VM needs nested
  virtualization from its hypervisor.
- Guest images under `/var/lib/differently` live on Talos's EPHEMERAL
  partition. Check whether your upgrade and reset procedure preserves it
  (**unverified**). If it does not, stage the images with `agent.guestImages`.

## Register plain Linux hosts (systemd)

Scenario: the control plane already runs (on Kubernetes or elsewhere), and
you want existing Linux KVM servers to join as a Firecracker pool without
running Kubernetes on them.

### 1. Declare the pool on the control plane

Use Helm values (preferred, because they are versioned with the deployment):

```yaml
config:
  pools:
    - name: firecracker            # the in-cluster DaemonSet pool, if you keep it
      adapter: firecracker
    - name: edge-linux
      adapter: firecracker
      region: dc1
      labels: {site: dc1}
networkPolicy:
  server:
    egress:
      agentCIDRs: ["192.168.50.0/24"]   # where the hosts' agents listen
```

Or use the API, which creates a pool with `source: api` that survives
restarts in PostgreSQL:

```sh
curl -fsS -X POST "$DIFFERENTLY_URL/v1/admin/pools" \
  -H "Authorization: Bearer $DIFFERENTLY_API_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"edge-linux","adapter":"firecracker","region":"dc1"}'
```

### 2. Open the network paths

| Path | Requirement |
|---|---|
| Host to control plane | HTTPS to the API (ingress or LoadBalancer), for heartbeats |
| Control plane to host | TCP to each host's advertise URL (default port 9090). Server pods must be able to route to the host IPs; allow them in `networkPolicy.server.egress.agentCIDRs` |
| Firewall on the host | Accept 9090 **only** from the control plane's egress addresses (node IPs or NAT gateway) |

- **Encryption.** Server-to-agent traffic is plain HTTP authenticated with the
  agent token. Across any network you do not fully control, put it inside a
  WireGuard or IPsec tunnel, or a private interconnect.
- **Ingress allow-lists.** If you set `networkPolicy.server.ingressFrom`, add
  the hosts' source addresses, or their heartbeats will be dropped.

### 3. Install the agent

Copy the binaries and images to the host, then run the installer
([`deploy/systemd/install.sh`](../../deploy/systemd/install.sh)):

```sh
sudo ./install.sh \
  --agent-binary ./differently-agent \
  --firecracker-tarball ./firecracker-v1.17.0-x86_64.tgz \
  --firecracker-sha256 <sha256-from-the-release> \
  --kernel ./out/vmlinux \
  --rootfs ./out/default.ext4 \
  --env-file ./agent.env
```

`agent.env` starts from
[`agent.env.example`](../../deploy/systemd/agent.env.example). Set these
values:

- `DIFFERENTLY_SERVER_URL`
- `DIFFERENTLY_POOL=edge-linux`
- `DIFFERENTLY_ADVERTISE_URL=http://<host-ip>:9090`
- `DIFFERENTLY_NODE_NAME`
- `DIFFERENTLY_AGENT_TOKEN`, which is the server's agent token:

  ```sh
  kubectl -n differently get secret dly-differently-tokens -o jsonpath='{.data.agent-token}' | base64 -d
  ```

The installer:

1. Checks `/dev/kvm`, `/dev/net/tun`, cgroup v2 and the kernel version.
2. Installs `differently-agent`, `firecracker` and `jailer` into
   `/usr/local/bin`, and the images into `/var/lib/differently/images`.
3. Writes `/etc/differently/agent.env` with mode 0600, if it does not exist.
4. Installs [`differently-agent.service`](../../deploy/systemd/differently-agent.service)
   and runs `differently-agent doctor`.
5. Enables and starts the service.

Use `--dry-run` to see each action first, or `--no-start` to stop before
starting. To have the installer download Firecracker from the official GitHub
release, use `--firecracker-version v1.17.0` instead of a local tarball. It
refuses to install the download without `--firecracker-sha256`.

### 4. Route sandboxes to the pool

Placement picks among every allowed private pool. To target these hosts
explicitly, narrow placement in the request:

```sh
curl -fsS -X POST "$DIFFERENTLY_URL/v1/sandboxes" -H "Authorization: Bearer $PROJECT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"profile":"small","placement":{"allowedPools":["edge-linux"]}}'
```

To pin projects to the pool, use a project policy instead: `PUT /v1/policies/project:<id>`
with `allowedPools`.

## Verify

```sh
# On the host
systemctl status differently-agent
journalctl -u differently-agent -n 50 --no-pager
/usr/local/bin/differently-agent doctor --state-dir=/var/lib/differently/agent \
  --images-dir=/var/lib/differently/images --kernel=/var/lib/differently/images/vmlinux \
  --firecracker=/usr/local/bin/firecracker

# From an operator workstation
curl -fsS -H "Authorization: Bearer $DIFFERENTLY_API_KEY" "$DIFFERENTLY_URL/v1/admin/agents?pool=edge-linux" \
  | jq '.items[] | {id, node, endpoint, healthy, freeMemoryMiB}'
# then create a sandbox with placement.allowedPools=["edge-linux"] and exec `echo ok` (kubernetes.md, step 8)
```

For Kubernetes distributions, use the verification in
[kubernetes.md](kubernetes.md#verify).
