# Proofs of concept

| | |
|---|---|
| Status | Planned. None of these experiments has been run as of 2026-10-28. |
| Why | The implementation is tested only in-process with fakes. These experiments retire the technical risks that unit tests cannot. |
| Related | [Roadmap](roadmap.md) · [Backlog E1, E2, E5](backlog.md) · [Reliability and performance](reliability-and-performance.md) |

Each POC states the risk, a falsifiable hypothesis, the method, success
criteria and the decision it unblocks. Targets marked *hypothesis* are guesses
to be replaced by measurements. Record every run with: commit, instance type,
host kernel, Firecracker version, filesystem, Kubernetes distribution and CNI.

| # | POC | Risk retired | Blocks |
|---|---|---|---|
| 1 | Firecracker on real KVM | The node agent has never booted a real VM | Every private-pool claim |
| 2 | Agent restart semantics in Kubernetes | VMs may die with the agent container | Upgrade procedure, restart recovery claims |
| 3 | Jailer integration | VMM runs as root without a jail | Production use of private pools |
| 4 | Memory snapshot restore with network overrides | Restored clones may be broken or not unique | `snapshots.memory` on Firecracker |
| 5 | Rootfs copy-on-write | Full copies make create slow and disks large | Default storage layout, boot SLO |
| 6 | Postgres failover and leader handover | Lost writes, double activation, stuck leadership | HA claims |
| 7 | External provider live-account verification | Adapters written from specs may be wrong | Moving Daytona and Freestyle from 🧪 to ✅ |
| 8 | Agent liveness and partition tolerance | A network blip may destroy VMs | Production use of private pools |
| 9 | VM networking under real CNIs | Isolation rules untested; DNS undefined | Network isolation claims |
| 10 | Control-plane throughput on Postgres | Unknown scheduling limits | Capacity guidance |

---

## POC 1. Firecracker on real KVM

**Risk.** The agent's VMM configuration, tap and iptables setup, vsock
handshake and lifecycle have only run against a fake VMM
(`internal/agent/agent_test.go`).

**Hypothesis.** The current agent, unchanged, boots, runs and tears down
microVMs on (a) EKS with `.metal` instances, (b) EKS with nested
virtualization on M8i/C8i-class instances, and (c) a bare-metal k3s or RKE2
node, with all features it advertises working.

**Method.**

1. Environments: an EKS managed node group of an x86 `.metal` family from
   Firecracker's tested list (m6i, m7i, m8i, m6a, m7a) and one Graviton
   `.metal` (m7g or m8g); an EKS or Karpenter node pool with
   `NestedVirtualization=enabled` on a supported family (Karpenter
   `EC2NodeClass` support since v1.13.0; managed node groups reportedly
   respect the launch-template option since July 2026, which needs this test);
   one bare-metal k3s or RKE2 host (Talos optional, with the agent namespace
   labelled `privileged`).
2. Build a guest kernel and rootfs with `deploy/images/build-kernel.sh` and
   `build-rootfs.sh` (marked unverified by their README), with
   `differently-guest` on vsock port 10000 and a resolver the guest can reach
   (the script writes none).
3. Install with the Helm chart, run the `doctor` init container, register the
   pool.
4. Smoke suite through the public API: create, wait, exec (sync and stream),
   files up/down, stop/start, pause/resume, snapshot, fork, delete; repeat 100
   times.
5. Isolation probes from inside a VM: cloud metadata `169.254.169.254`, the
   node's private and public IPs, another VM's IP, pod CIDR, service CIDR,
   kubelet port, a public host. From another pod: the VM's IP.
6. Benchmarks B1 and B2 from
   [reliability-and-performance.md](reliability-and-performance.md#4-benchmark-plan).

**Success criteria.**

- Every advertised feature works on all three environments; 100 consecutive
  smoke runs with no leaked VM, tap, chain or directory.
- Probes: everything except the public host is blocked with `--egress allow`;
  everything is blocked with `--egress deny`; nothing reaches the VM.
- Create-to-running p95 ≤ 3 s on `.metal` for a 1 GiB image (*hypothesis*);
  nested virtualization within 2× of `.metal` (*hypothesis*).
- Stop uses Ctrl-Alt-Del successfully on x86; on arm64 the forced kill path is
  documented.

**Unblocks.** Changing private-pool features from "unit-tested" to
"validated" in [feature-parity.md](feature-parity.md); boot SLO; deploy guide
accuracy.

## POC 2. Agent restart semantics in Kubernetes

**Risk.** The agent launches Firecracker with `setsid` and its code comment
says VMs survive an agent restart. In a container the agent is PID 1 of its PID
namespace; when it exits, the kernel kills every process in that namespace.
The systemd unit uses `KillMode=control-group`, which kills the unit's whole
cgroup on stop and on restart after a crash.

**Hypothesis.** With the current DaemonSet (no `hostPID`) and the current
systemd unit, any agent restart kills every microVM on the node; disks
survive.

**Method.** Start 10 VMs; then in turn `kill 1` inside the container, delete
the pod, `crictl stop` the container, and roll the DaemonSet; on a systemd
host, `systemctl restart` the unit and kill the agent process to trigger
`Restart=on-failure`. After each, check the Firecracker processes on the host
and what the API reports. Then evaluate mitigations:

1. `hostPID: true`, so the agent is not a namespace init;
2. launching each VMM outside the container (for example `systemd-run
   --scope` through `nsenter` into the host), which also changes the cgroup the
   VM memory is charged to;
3. a separate, rarely updated VMM supervisor container, with the agent as a
   client;
4. accepting the restart and making it explicit: the agent reports VMs as
   `stopped`, and the control plane moves sandboxes to `stopped` so users can
   start them again.

**Success criteria.** Behaviour documented for each restart type; a chosen
design that either keeps VMs running across agent upgrades or reliably
transitions affected sandboxes to `stopped` within one reconcile interval; an
upgrade procedure in the day-2 guide.

**Unblocks.** [Backlog E0](backlog.md#e0-correctness-and-security-fixes-from-the-code-review)
state reconciliation; [backlog E4](backlog.md#e4-firecracker-jailer) (the
jailer changes process ownership too).

## POC 3. Jailer integration

**Risk.** Firecracker runs as root in the agent's (privileged) container with
no chroot ([ADR 0015](adr/0015-defer-jailer-integration.md)).

**Hypothesis.** The agent can launch every VM through `jailer` with a
per-VM chroot and an unprivileged UID/GID, keeping taps in the agent's network
namespace, with no lifecycle regressions and small boot overhead.

**Method.**

1. Use a statically linked Firecracker and jailer of the same release.
2. Build a `JailerLauncher` implementing the agent's `Launcher` interface:
   chroot base per VM, hard-link or bind-mount the kernel and `rootfs.ext4`
   (same filesystem for hard links), create `/dev/kvm` and `/dev/net/tun` nodes
   as the jailer expects, place `fc.sock` and `vsock.sock` inside the chroot,
   cgroup v2 limits per VM.
3. Decide network namespace placement: taps in the agent namespace (current
   design) versus a namespace per VM (`--netns`) with veth plumbing.
4. Run the POC 1 smoke suite and the restart tests with the jailer.
5. Wire the existing `--jailer` flag (accepted today but unused) and the chart's
   `agent.jailer.*` values to the new launcher.

**Success criteria.** All lifecycle operations pass; each VMM process runs
non-root inside its chroot with the expected seccomp filter; boot overhead
≤ 10 % (*hypothesis*); restart recovery handles jailed PIDs; enabling the
chart value actually jails VMs.

**Unblocks.** [Backlog E4](backlog.md#e4-firecracker-jailer); production
recommendation for private pools.

## POC 4. Memory snapshot restore with network overrides

**Risk.** Firecracker restores lose network and vsock connections; clones need
new network identity and uniqueness; snapshot files are trusted input; the
memory file must outlive restored VMs
([ADR 0014](adr/0014-defer-memory-snapshots-until-restore-proven.md)).

**Hypothesis.** Following the plan in
[storage-and-lifecycle.md §9](storage-and-lifecycle.md#9-memory-snapshot-plan),
the agent can snapshot a running VM and restore one or more clones on the same
node, each with its own slot, that answer exec with process state intact.

**Method.**

1. Capture with `PUT /snapshot/create` (full snapshot) and a reflink copy of
   the disk while paused.
2. Restore into a new slot. Check whether the pinned Firecracker release
   accepts network overrides on snapshot load or requires the original tap
   name; implement the corresponding path.
3. Add a guest-agent hook to re-address `eth0`, regenerate machine IDs and
   reseed entropy; reconnect vsock from the agent.
4. HMAC the snapshot files with a node key and verify before load.
5. Reference-count memory files; refuse to delete a snapshot while a restored
   VM maps it.
6. Measure on cgroup v2 hosts, 1 GiB and 4 GiB guests.

**Success criteria.** A process started before the snapshot is still running
in the clone; two clones have distinct MAC, IP, machine ID and random output;
restore-to-exec p95 ≤ 500 ms for 1 GiB (*hypothesis*); tampered files are
rejected; restore across mismatched Firecracker versions fails cleanly.

**Unblocks.** [Backlog E9](backlog.md#e9-memory-snapshots-on-firecracker):
advertising `snapshots.memory`, memory-based fork, pause to disk.

## POC 5. Rootfs copy-on-write

**Risk.** The agent copies the template image per VM (`cp --reflink=auto
--sparse=always`, falling back to a byte copy). Without reflink, create time
and disk use scale with image size, and disk snapshots pause the VM for the
whole copy.

**Hypothesis.** XFS with reflink (or btrfs) makes create and disk snapshot time
independent of image size, and is the right default; dm-thin is only needed
for block-level snapshot features.

**Method.** On one `.metal` host, compare ext4 (full copy), XFS reflink,
btrfs reflink and LVM thin snapshots for: create time, disk used by 100 VMs
from one 2 GiB image, pause duration during a disk snapshot of a busy VM,
throughput inside the guest, fragmentation after 1 000 create/delete cycles,
and behaviour when the filesystem fills.

**Success criteria.** A recommended filesystem with numbers; disk snapshot
pause ≤ 100 ms with reflink (*hypothesis*); the prerequisite documented in
the deploy guides and checked by `doctor`.

**Unblocks.** Boot SLO, snapshot design, deploy prerequisites.

## POC 6. Postgres failover and leader handover

**Risk.** Correctness under failover rests on transactions, idempotent creates
and an advisory lock whose release depends on Postgres noticing dead sessions.
The Postgres conformance suite has not been run in the recorded environment.

**Hypothesis.** With CloudNativePG (three instances, synchronous replication)
and three server replicas, failovers lose no acknowledged admission, never
over-allocate quota, never create duplicate instances, and resume scheduling
within 30 s.

**Method.**

1. Run the store conformance suite against Postgres
   (`DIFFERENTLY_TEST_POSTGRES_URL`).
2. Drive steady load (creates, deletes, execs) through three replicas with the
   fake adapter.
3. Inject: primary switchover, forced primary failure, leader process kill,
   leader network partition without socket close, Postgres restart.
4. After each, check invariants with SQL (per organization: active, CPU and
   memory within quota; per pool: slots within `maxConcurrency`), the fake
   adapter's instance count against `running` rows, and `GET
   /v1/admin/audit/verify`.
5. Tune server TCP keepalives and record takeover times.

**Success criteria.** Invariants hold after every injection; audit chain
verifies; scheduling resumes within 30 s (*hypothesis*) on failover and within
the keepalive window on partition; recommended Postgres settings documented.

**Unblocks.** HA claims; [backlog E5](backlog.md#e5-postgres-in-ci-failover-and-scale).

## POC 7. External provider live-account verification

**Risk.** The Daytona and Freestyle adapters are written from published
OpenAPI specs and tested against fake servers built from the same specs.

**Hypothesis.** The adapters work against live accounts after small fixes, and
every item marked UNVERIFIED in the research notes can be resolved.

**Method.** Dedicated test accounts with scoped keys; scripted runs of every
adapter method through Differently; responses recorded (with secrets removed)
and turned into fake-server fixtures.

Items to resolve:

| Provider | Item |
|---|---|
| Daytona | Whether the API rejects or ignores `cpu`/`memory`/`disk` when `snapshot` is set. The adapter sends no resources today, so sandboxes get the snapshot's size while quota and cost use the requested size |
| Daytona | Effective default of the exec `timeout` (spec says 10 s; the adapter always sends 600 s, which exceeds the adapter's 6-minute HTTP client timeout) |
| Daytona | Whether control-plane error bodies include `code` and `source` |
| Daytona | Array query encoding for `states` and `snapshots`; the JSON-object `labels` filter used by `find` |
| Daytona | `ttlMinutes` and `autoStopInterval: 0` behaviour; `archived` state mapping to `stopped` |
| Daytona | Snapshot creation from a sandbox, async completion and restore by name; pause/resume on `linux-vm` |
| Daytona | Signed preview URL format and expiry |
| Freestyle | Whether `pause` returns while still `pausing` |
| Freestyle | Whether `exec-await` wakes a paused VM |
| Freestyle | Extra fields in the create response (`firewallRules`, `tlsRules`) |
| Freestyle | API key format, request rate limits, region selection, whether any pre-v5 VM API still works |
| Freestyle | Stop via `poweroff` reliability and timing; env file sourcing; `/resize` semantics |
| Freestyle | `POST /v5/tls` conflict behaviour on repeated port exposure; firewall default deny |
| Both | 429 mapping to `insufficient_capacity` at plan limits; 401/403 messages |

**Success criteria.** Every item answered and recorded; adapter fixes merged;
fake servers updated to match observed behaviour; the backend matrix in
[feature-parity.md](feature-parity.md) updated from 🧪 to ✅ where verified.

**Unblocks.** [Backlog E2](backlog.md#e2-external-adapters-on-live-accounts);
Scenario D on real providers.

## POC 8. Agent liveness and partition tolerance

**Risk.** The Firecracker pool lists only agents seen within
`scheduler.agentTimeoutSeconds` (30 s). After a longer heartbeat gap, the
reconciler treats that node's sandboxes as lost and fails them; when the agent
returns, its VMs are orphans and are deleted.

**Hypothesis.** The current behaviour is as described; a design that separates
"agent unreachable" from "instance gone" tolerates partitions of at least 10
minutes without failing sandboxes or deleting VMs.

**Method.** Drop heartbeats and control-plane traffic to one node with
iptables for 20 s, 60 s and 10 min; record sandbox states and VM survival.
Prototype: an agent `unknown` state that suspends reconciliation for its
instances; failing sandboxes only after an operator-set grace period; never
deleting instances of a returning agent without re-checking their sandboxes.

**Success criteria.** With the fix, a 10-minute partition fails no sandbox and
deletes no VM; operators get an alert within one heartbeat timeout.

**Unblocks.** [Backlog E0](backlog.md#e0-correctness-and-security-fixes-from-the-code-review)
(partition handling); production use of private pools.

## POC 9. VM networking under real CNIs

**Risk.** The per-VM rules (`internal/agent/network.go`) were checked only as
command strings. Interaction with CNIs, DNS and IPv6 is unknown.

**Hypothesis.** With `agent.hostNetwork: false`, taps and rules in the agent
pod's namespace isolate VMs as intended under Calico, Cilium and the AWS VPC
CNI; VM egress leaves with the pod IP, so pod egress NetworkPolicies also apply
to VM traffic.

**Method.** For each CNI and for `hostNetwork` true and false: the POC 1
isolation probes; DNS resolution from the guest with a public resolver and
with cluster DNS; IPv6 reachability on dual-stack clusters; whether an agent
pod egress NetworkPolicy constrains VM egress; MTU and NAT behaviour; many VMs
per node (rule-chain growth).

**Success criteria.** A support matrix; a recommended DNS setup for images;
documented IPv6 behaviour, and `ip6tables` rules if needed; no regression in
heartbeat or adapter traffic.

**Unblocks.** [Backlog E12](backlog.md#e12-per-sandbox-egress-allow-lists);
network claims in the trust model.

## POC 10. Control-plane throughput on Postgres

**Risk.** Admission and activation recompute usage from every live sandbox of
an organization under its row lock; audit appends are globally serialized; one
leader dispatches at most 100 candidates per tick.

**Hypothesis.** One leader sustains ≥ 50 activations/s and the API ≥ 200
admissions/s across replicas at 1 000 live sandboxes per organization
(*hypotheses*); the tally query is the first per-tenant limit and the audit
lock the first global one.

**Method.** Benchmarks B3 and B5 from
[reliability-and-performance.md](reliability-and-performance.md#4-benchmark-plan)
with the fake adapter, varying replicas, organizations and live sandboxes per
organization; Postgres statement statistics and lock waits recorded.

**Success criteria.** Measured limits published; if below the hypotheses, the
fix chosen (aggregate tally query or summary rows, batched audit appends,
`SKIP LOCKED` dispatch) and re-measured.

**Unblocks.** Capacity guidance; [backlog E5](backlog.md#e5-postgres-in-ci-failover-and-scale).
