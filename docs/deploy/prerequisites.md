# Prerequisites

## Control plane

| Requirement | Notes |
|---|---|
| Kubernetes 1.29 or newer | The chart's `kubeVersion` floor. Any conformant distribution |
| Helm 3.8+ or Helm 4 | `helm lint` and `helm template` were checked with Helm v4.2.3 |
| PostgreSQL for production | Managed PostgreSQL or a CloudNativePG cluster. The minimum supported major version is **unverified**; the evaluation-only embedded database uses 17 |
| A CNI that enforces NetworkPolicy | Optional but recommended: Calico, Cilium, GKE Dataplane V2, or k3s's built-in controller |
| `kubectl`, `curl`, `jq` | Used throughout these guides |

## KVM hosts

Each host that runs microVMs needs:

| Requirement | Check |
|---|---|
| x86_64 or aarch64 | `uname -m` |
| Hardware virtualization exposed to the OS (bare metal, or nested virtualization enabled) | `grep -cE 'vmx\|svm' /proc/cpuinfo` (x86, non-zero) |
| `/dev/kvm`, readable and writable by root | `ls -l /dev/kvm` |
| KVM module loaded | `lsmod \| grep -E '^kvm'` (shows `kvm_intel` or `kvm_amd`) |
| `/dev/net/tun` | `ls -l /dev/net/tun` (`modprobe tun` if missing) |
| cgroup v2 | `stat -fc %T /sys/fs/cgroup` prints `cgroup2fs` |
| Host kernel 5.10, 6.1 or 6.18 | `uname -r`. These are Firecracker's supported host kernels. 8th-gen Intel (`*8i`) needs 6.1 or 6.18 |
| Disk under `/var/lib/differently` | Guest images plus one disk per sandbox, at the profile's `diskGiB` |
| Memory and CPU headroom | The agent reserves `--reserve-cpu` and `--reserve-memory-mib` for the host |

Firecracker's tested platforms are AWS `.metal` instances: Intel m5n, m6i,
m7i and m8i; AMD m6a and m7a; Graviton m6g, m7g, m8g and m9g. Firecracker
documents nested virtualization only as a development setup. AWS, GCP and
Azure all offer nested virtualization on some machine families, but none of
those paths has been tried with Differently (see the support matrix in
[README.md](README.md)).

### Checking a Kubernetes node

Without SSH access, use a debug pod. It is scheduled with `nodeName`, so the
taint does not block it:

```sh
NODE=<node-name>
kubectl debug node/$NODE -it --image=busybox:1.37 --profile=sysadmin -- sh -c '
  ls -l /host/dev/kvm /host/dev/net/tun
  grep -cE "vmx|svm" /host/proc/cpuinfo
  stat -fc %T /host/sys/fs/cgroup
  uname -r'
# kubectl debug leaves a node-debugger-* pod behind; delete it afterwards:
kubectl get pods -o name | grep node-debugger | xargs kubectl delete
```

After installing the chart, the agent's `doctor` init container runs the
authoritative checks: `kubectl logs <agent-pod> -c doctor`.

## Label and taint KVM nodes

The agent DaemonSet runs only on nodes labelled `differently.dev/kvm=true`.
A matching `NoSchedule` taint keeps ordinary pods off these nodes, so the
memory and CPU stay free for microVMs. The chart tolerates this taint by
default.

```sh
kubectl label node <node> differently.dev/kvm=true
kubectl taint node <node> differently.dev/kvm=true:NoSchedule
```

For managed node groups, set the label and taint in the node group
definition instead, so replacement nodes get them too (see
[aws.md](aws.md), [gcp.md](gcp.md), [azure.md](azure.md)).

Only label nodes that pass the KVM checks. On a node without `/dev/kvm`, the
agent pod fails with `hostPath type check failed: /dev/kvm is not a
character device`.

## Pod Security Admission

The agent runs privileged, with host paths `/dev/kvm`, `/dev/net/tun` and
`/var/lib/differently`. The namespace must allow it:

```sh
kubectl label namespace differently pod-security.kubernetes.io/enforce=privileged
```

Talos enforces `baseline` by default, and RKE2 with the CIS profile enforces
`restricted`. Both need this label or an exemption (see
[bare-metal.md](bare-metal.md)). The server, migration Job, test Pod and
embedded PostgreSQL would meet `restricted` on their own; the chart installs
every component into the release namespace, so that namespace needs
`privileged` while the agent is enabled.

## Guest kernel and root filesystem

Each KVM host needs a guest kernel at `/var/lib/differently/images/vmlinux`.
It also needs at least one root filesystem, `/var/lib/differently/images/<template>.ext4`,
with `differently-guest` started at boot. Build them with
[`deploy/images/`](../../deploy/images/README.md):

```sh
deploy/images/build-kernel.sh --version 6.1.<patch> --out out/
deploy/images/build-rootfs.sh --image ubuntu:24.04 --guest-binary ./differently-guest --output out/default.ext4
```

Then stage them in one of these ways:

- the chart's `agent.guestImages` init container (from an OCI image built by
  `deploy/images/package-oci.sh`);
- node bootstrap (cloud-init, golden images);
- `deploy/systemd/install.sh` on plain Linux hosts.

## Network

| From | To | Port | Purpose |
|---|---|---|---|
| Clients, dashboard users | Server (Service or ingress) | 8080 / 443 | API and dashboard |
| Agents | Server | 8080 / 443 | Heartbeats (`/internal/v1/agents/heartbeat`, agent token plus per-agent secret) |
| Server | Agents | 9090 | Sandbox lifecycle, exec, files (that agent's own secret) |
| Server | PostgreSQL | 5432 | State |
| Server | Daytona / Freestyle APIs | 443 | Only when a provider is enabled |
| Prometheus | Server | 9091 | `/metrics` (unauthenticated; `metrics.port`) |

- **Agent traffic is unencrypted.** Server-to-agent traffic is plain HTTP.
  Keep it on a private network, or use an encrypting CNI (for example
  WireGuard mode in Cilium or Calico).
- **Agent URL family.** The default advertise URL, `http://$(POD_IP):9090`,
  assumes IPv4 pod IPs. Set `agent.advertiseURL` for IPv6.
- **Sandbox egress.** Sandbox traffic leaves through the agent pod's network
  namespace, or the host's for systemd agents. Restrict it with node
  firewalling or egress policy. Always block cloud metadata endpoints such as
  `169.254.169.254`.

## Verify

You are ready to install when all of these hold:

```sh
kubectl version                                   # server 1.29+
helm version                                      # 3.8+ or 4.x
kubectl get nodes -l differently.dev/kvm=true     # at least one node
kubectl get ns differently --show-labels          # pod-security.kubernetes.io/enforce=privileged
# and on each KVM node: /dev/kvm and /dev/net/tun exist, cgroup2fs, kernel 5.10/6.1/6.18,
# and /var/lib/differently/images holds vmlinux and at least one *.ext4 (or agent.guestImages is set)
```
