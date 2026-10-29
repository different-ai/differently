# Prerequisites

## Control plane

| Requirement | Notes |
|---|---|
| Kubernetes 1.29 or newer | The chart's `kubeVersion` floor. Any conformant distribution |
| Helm 3.8+ or Helm 4 | |
| PostgreSQL for production | Managed PostgreSQL or a CloudNativePG cluster. The minimum supported major version is **unverified**; the evaluation-only embedded database uses 17 |
| A CNI that enforces NetworkPolicy | Optional but recommended: Calico, Cilium, GKE Dataplane V2, or k3s's built-in controller |
| `kubectl`, `curl`, `jq` | Used throughout these guides |

The control plane needs no KVM, privileged pods or host paths.

## OpenSandbox

The default private runtime is an
[OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) server, which
you install and operate with OpenSandbox's own Helm chart or manifests,
following its documentation. Differently's chart does not install it.

| Requirement | Notes |
|---|---|
| An OpenSandbox lifecycle server reachable from the control plane | The chart's default `opensandbox.apiURL` is `http://opensandbox-server.opensandbox-system.svc/v1`, a server in namespace `opensandbox`. Any URL ending in `/v1` works |
| A server API key | OpenSandbox's server-wide `api_key` (sent as the `OPEN-SANDBOX-API-KEY` header). Differently reads it from a Secret into an environment variable and refers to it by name; it never reaches tenants or sandboxes |
| A runtime | Docker or Kubernetes (containers that share the host kernel), or Fast Sandbox (Firecracker microVMs, which need KVM hosts, below) |
| Pinned versions | Pin the server and `execd` image versions. The adapter was checked against server `release-1.2.0` with `execd` `v1.1.0`; the published spec runs ahead of the images, so rerun the live test after any upgrade ([day-2.md](day-2.md#upgrade)) |

What has been run: the Docker runtime only, on one host
([spike](../research/opensandbox-spike.md)). The Kubernetes runtime and Fast
Sandbox are **unverified** with Differently.

One OpenSandbox server is one trust domain, so register it as one pool and
declare its `trust` honestly: `private` if you operate the server and its
hosts, `external` if someone else does.

## KVM hosts (Fast Sandbox only)

Skip this section for the Docker and Kubernetes runtimes. OpenSandbox's Fast
Sandbox runs Firecracker microVMs through its own per-node agent, and its
documented host requirements (as read for the spike; **not yet tried** with
Differently) are:

| Requirement | Check |
|---|---|
| Linux 5.10 or newer | `uname -r` |
| Hardware virtualization exposed to the OS (bare metal, or nested virtualization enabled) | `grep -cE 'vmx\|svm' /proc/cpuinfo` (x86, non-zero) |
| `/dev/kvm` | `ls -l /dev/kvm`; `lsmod \| grep -E '^kvm'` shows `kvm_intel` or `kvm_amd` |
| `/dev/net/tun` | `ls -l /dev/net/tun` (`modprobe tun` if missing) |
| cgroup v2 | `stat -fc %T /sys/fs/cgroup` prints `cgroup2fs` |
| An XFS filesystem with reflink for images | `xfs_info <mount> \| grep reflink=1` |
| An enlarged `/dev/shm` | `df -h /dev/shm` |

Check OpenSandbox's documentation for the current list, its supported host
kernels, and how its node components are scheduled and privileged.
Firecracker itself supports 5.10, 6.1 and 6.18 host kernels, and its tested
platforms are AWS `.metal` instances: Intel m5n, m6i, m7i and m8i; AMD m6a and
m7a; Graviton m6g, m7g, m8g and m9g. Firecracker documents nested
virtualization only as a development setup. AWS, GCP and Azure all offer
nested virtualization on some machine families, but none of those paths has
been tried (see the support matrix in [README.md](README.md)).

### Checking a Kubernetes node

Without SSH access, use a debug pod. It is scheduled with `nodeName`, so a
taint does not block it:

```sh
NODE=<node-name>
kubectl debug node/$NODE -it --image=busybox:1.37 --profile=sysadmin -- sh -c '
  ls -l /host/dev/kvm /host/dev/net/tun
  grep -cE "vmx|svm" /host/proc/cpuinfo
  stat -fc %T /host/sys/fs/cgroup
  df -h /host/dev/shm
  uname -r'
# kubectl debug leaves a node-debugger-* pod behind; delete it afterwards:
kubectl get pods -o name | grep node-debugger | xargs kubectl delete
```

Dedicate KVM nodes to Fast Sandbox with a label and a `NoSchedule` taint, so
ordinary pods leave their memory and CPU to microVMs. Which label, taint and
tolerations OpenSandbox's components use is set in OpenSandbox's
installation, not in Differently's chart. For managed node groups, set the
label and taint in the node group definition, so replacement nodes get them
too (see [aws.md](aws.md), [gcp.md](gcp.md), [azure.md](azure.md)).

## Pod Security Admission

Differently's own pods (server, migration Job, test Pod, embedded PostgreSQL)
are written to run without privilege, so the release namespace does not need
the `privileged` level. Enforcing `restricted` on it has not been tried.

OpenSandbox's namespace needs whatever its runtime requires: the Docker
runtime gives the server the Docker socket (as `test/opensandbox/compose.yaml`
does), and Fast Sandbox needs access to `/dev/kvm`
and `/dev/net/tun` on its nodes. Label that namespace according to
OpenSandbox's documentation. Talos enforces `baseline` by default, and RKE2
with the CIS profile enforces `restricted` (see [bare-metal.md](bare-metal.md)).

## Network

| From | To | Port | Purpose |
|---|---|---|---|
| Clients, dashboard users | Server (Service or ingress) | 8080 / 443 | API and dashboard |
| Server | OpenSandbox server | 80 with OpenSandbox's chart, 8090 for a standalone server (or your `apiURL`) | Lifecycle calls, and exec and file transfer through OpenSandbox's proxy (API key header) |
| Server | PostgreSQL | 5432 | State |
| Server | Daytona / Freestyle APIs | 443 | Only when a provider is enabled |
| Prometheus | Server | 9091 | `/metrics` (unauthenticated; `metrics.port`) |

- **Exec and files go through the OpenSandbox server's proxy** by default
  (`opensandbox.useServerProxy: true`), so the control plane needs only the
  server's port, which the chart allows through
  `networkPolicy.server.egress.opensandbox` (namespace `opensandbox-system`,
  port 80 by default; set `cidrs` for a server outside the cluster). If you turn
  the proxy off, the control plane connects to each sandbox's endpoint
  directly; allow that egress with `networkPolicy.server.egress.extra`.
- **Encryption.** The default `apiURL` is plain HTTP inside the cluster, and
  every request carries the OpenSandbox API key. Keep it on a private network,
  use an encrypting CNI (for example WireGuard mode in Cilium or Calico), or
  serve OpenSandbox over `https`.
- **Sandbox egress** is controlled by the OpenSandbox runtime and the network
  under it, not by Differently, which does not wire OpenSandbox's network
  policy yet ([backlog E19](../backlog.md#e19-opensandbox-network-policy-and-egress)).
  Restrict it with OpenSandbox's configuration, Kubernetes NetworkPolicy in
  OpenSandbox's namespace, or host and VPC firewalls. Always block cloud
  metadata endpoints such as `169.254.169.254`.

## Verify

You are ready to install when all of these hold:

```sh
kubectl version                                   # server 1.29+
helm version                                      # 3.8+ or 4.x
# OpenSandbox answers with its API key (from a pod in the cluster, or through a port-forward):
curl -fsS -H "OPEN-SANDBOX-API-KEY: $OSB_KEY" \
  "http://opensandbox-server.opensandbox-system.svc/v1/sandboxes?page=1&pageSize=1"
# Fast Sandbox only, on each KVM node: /dev/kvm and /dev/net/tun exist, cgroup2fs,
# an XFS image volume with reflink, and an enlarged /dev/shm
```

The `GET /v1/sandboxes` call is the same one the adapter uses as its health
check.
