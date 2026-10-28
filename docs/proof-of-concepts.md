# Proofs of concept

| | |
|---|---|
| Status | Planned. None of the open experiments below has been run as of 2026-10-28. The [OpenSandbox spike](research/opensandbox-spike.md) ran OpenSandbox's Docker runtime live; it is recorded there, not here. |
| Why | Most of the implementation is tested in-process with fakes; only the `opensandbox` adapter has run against a real backend (Docker runtime). These experiments retire the risks that unit tests cannot. |
| Runtime change | [ADR 0020](adr/0020-opensandbox-replaces-node-agent.md) removed Differently's Firecracker node agent. POCs about the agent are obsolete and kept as history; POC 11–13 cover OpenSandbox. |
| Related | [Roadmap](roadmap.md) · [Backlog E2, E5, E18–E21](backlog.md) · [Reliability and performance](reliability-and-performance.md) |

Each POC states the risk, a falsifiable hypothesis, the method, success
criteria and the decision it unblocks. Targets marked *hypothesis* are guesses
to be replaced by measurements. Record every run with: commit, instance type,
host kernel, OpenSandbox server, `execd` and runtime versions, filesystem,
Kubernetes distribution and CNI.

| # | POC | Risk retired | Blocks | State |
|---|---|---|---|---|
| 1 | Firecracker on real KVM | The node agent had never booted a real VM in the cloud | Every private-pool claim | ⛔ obsolete, see 11 |
| 2 | Agent restart semantics in Kubernetes | VMs may die with the agent container | Upgrade procedure, restart recovery claims | ⛔ obsolete, see 13 |
| 3 | Jailer integration | VMM runs as root without a jail | Production use of private pools | ⛔ obsolete, see 11 |
| 4 | Memory snapshot restore with network overrides | Restored clones may be broken or not unique | `snapshots.memory` on Firecracker | ⛔ obsolete, see 11 |
| 5 | Rootfs copy-on-write | Full copies make create slow and disks large | Default storage layout, boot SLO | ⛔ obsolete |
| 6 | Postgres failover and leader handover | Lost writes, double activation, stuck leadership | HA claims | 📋 |
| 7 | External provider live-account verification | Adapters written from specs may be wrong | Moving Daytona and Freestyle from 🧪 to ✅ | 📋 |
| 8 | Agent liveness and partition tolerance | A network blip may destroy VMs | Production use of private pools | ⛔ obsolete, see 13 |
| 9 | VM networking under real CNIs | Isolation rules untested; DNS undefined | Network isolation claims | ⛔ obsolete, see 12 and E19 |
| 10 | Control-plane throughput on Postgres | Unknown scheduling limits | Capacity guidance | 📋 |
| 11 | Fast Sandbox through the adapter | MicroVM isolation through Differently is unverified | Any microVM claim; `snapshots.memory` on OpenSandbox pools | 📋 next |
| 12 | OpenSandbox Kubernetes runtime | The Kubernetes runtime and in-cluster networking are unverified | Kubernetes deploy guidance | 📋 |
| 13 | OpenSandbox outage and recovery | Unknown behaviour when the OpenSandbox server is down, restarted or upgraded | Upgrade procedure, Scenario J and K claims | 📋 |

---

## POC 1. Firecracker on real KVM

> Obsolete ([ADR 0020](adr/0020-opensandbox-replaces-node-agent.md)): the
> agent was removed. MicroVMs on real KVM through Differently are now
> [POC 11](#poc-11-fast-sandbox-through-the-adapter). History kept.

**Risk (historical).** The agent's VMM configuration, tap and iptables setup,
vsock handshake and lifecycle had run only against a fake VMM, and, shortly
before removal, under nested virtualization on Colima (arm64) in the agent's
end-to-end suite.

**Plan (never run).** EKS `.metal` and nested-virtualization node groups and
a bare-metal k3s or RKE2 host; guest images from `deploy/images/` (removed);
a smoke suite through the public API repeated 100 times; isolation probes
from inside a VM; benchmarks B1 and B2. The environment notes (Firecracker's
tested instance families, nested virtualization on M8i/C8i-class instances)
still matter as host prerequisites for Fast Sandbox.

## POC 2. Agent restart semantics in Kubernetes

> Obsolete ([ADR 0020](adr/0020-opensandbox-replaces-node-agent.md)): there is
> no node agent. Restarts of the OpenSandbox server are
> [POC 13](#poc-13-opensandbox-outage-and-recovery). History kept.

**Risk (historical).** In a container the agent was PID 1 of its PID
namespace and the systemd unit used `KillMode=control-group`, so any agent
restart was expected to kill every microVM on the node. The plan was to
measure each restart type and choose between `hostPID`, launching VMMs
outside the container, a separate supervisor, or reporting VMs as `stopped`.

## POC 3. Jailer integration

> Obsolete ([ADR 0020](adr/0020-opensandbox-replaces-node-agent.md)):
> Differently no longer launches Firecracker. How Fast Sandbox runs its VMM
> (jailer, user, seccomp, chroot) is recorded in
> [POC 11](#poc-11-fast-sandbox-through-the-adapter). History kept.

**Risk (historical).** Firecracker ran as root in the agent's privileged
container with no chroot ([ADR 0015](adr/0015-defer-jailer-integration.md)).
The plan was a `JailerLauncher` with a per-VM chroot, an unprivileged UID/GID
and cgroup v2 limits, wired to the agent's unused `--jailer` flag.

## POC 4. Memory snapshot restore with network overrides

> Obsolete ([ADR 0020](adr/0020-opensandbox-replaces-node-agent.md); ADR 0014
> is obsolete with it): memory snapshots now depend on the runtime. Its
> questions (process survival, clone uniqueness, trusted snapshot files,
> memory-file lifetime) carry over to
> [POC 11](#poc-11-fast-sandbox-through-the-adapter). History kept.

**Risk (historical).** Firecracker restores lose network and vsock
connections; clones need new network identity and uniqueness; snapshot files
are trusted input; the memory file must outlive restored VMs
([ADR 0014](adr/0014-defer-memory-snapshots-until-restore-proven.md)). The
plan followed the Firecracker capture-and-restore steps that
[storage-and-lifecycle.md §9](storage-and-lifecycle.md#9-memory-snapshot-plan)
used to describe.

## POC 5. Rootfs copy-on-write

> Obsolete ([ADR 0020](adr/0020-opensandbox-replaces-node-agent.md)):
> Differently no longer copies root filesystems. Fast Sandbox itself requires
> XFS with reflink for images ([POC 11](#poc-11-fast-sandbox-through-the-adapter)).
> History kept.

**Risk (historical).** The agent copied the template image per VM; without
reflink, create time and disk use scaled with image size. The plan compared
ext4, XFS reflink, btrfs reflink and LVM thin snapshots on one `.metal` host.

## POC 6. Postgres failover and leader handover

**Risk.** Correctness under failover rests on transactions, idempotent creates
and an advisory lock whose release depends on Postgres noticing dead sessions.
Multi-replica failover has not been exercised.

**Hypothesis.** With CloudNativePG (three instances, synchronous replication)
and three server replicas, failovers lose no acknowledged admission, never
over-allocate quota, never create duplicate instances, and resume scheduling
within 30 s.

**Method.**

1. Run the store conformance suite against Postgres
   (`DIFFERENTLY_TEST_POSTGRES_URL`).
2. Drive steady load (creates, deletes, execs) through three replicas with the
   fake adapter, then repeat with an `opensandbox` pool on a real OpenSandbox
   server (Docker runtime) to check for duplicate backend instances.
3. Inject: primary switchover, forced primary failure, leader process kill,
   leader network partition without socket close, Postgres restart.
4. After each, check invariants with SQL (per organization: active, CPU and
   memory within quota; per pool: slots within `maxConcurrency`), the backend's
   instance count against `running` rows, and `GET
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
and turned into fake-server fixtures. The OpenSandbox spike is the model: it
found four spec-versus-image differences that fake servers built from specs
could not.

Items to resolve:

| Provider | Item |
|---|---|
| Daytona | Whether the API rejects or ignores `cpu`/`memory`/`disk` when `snapshot` is set. The adapter sends no resources today, so sandboxes get the snapshot's size while quota and cost use the requested size |
| Daytona | Effective default of the exec `timeout` (spec says 10 s; the adapter always sends 600 s) |
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

> Obsolete ([ADR 0020](adr/0020-opensandbox-replaces-node-agent.md)): there
> are no agent heartbeats. A partition between the control plane and an
> OpenSandbox server is part of
> [POC 13](#poc-13-opensandbox-outage-and-recovery). History kept.

**Risk (historical).** The Firecracker pool listed only agents seen within
`scheduler.agentTimeoutSeconds`; after a longer gap the reconciler treated a
node's sandboxes as lost and later deleted its VMs as orphans. A 10-minute
partition grace was added before the agent was removed.

## POC 9. VM networking under real CNIs

> Obsolete ([ADR 0020](adr/0020-opensandbox-replaces-node-agent.md)):
> Differently no longer creates taps or iptables rules. Sandbox networking
> under real CNIs is part of
> [POC 12](#poc-12-opensandbox-kubernetes-runtime); per-sandbox egress is
> [backlog E19](backlog.md#e19-opensandbox-network-policy-and-egress). History
> kept.

**Risk (historical).** The per-VM rules (`internal/agent/network.go`, removed)
were checked only as command strings; interaction with CNIs, DNS and IPv6 was
unknown.

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

## POC 11. Fast Sandbox through the adapter

**Risk.** ADR 0020 made OpenSandbox the runtime before Fast Sandbox, its
Firecracker runtime, had run on our targets. Through Differently, microVM
isolation, memory snapshot restore, egress control under Fast Sandbox and
multi-node behaviour are all unverified. Fast Sandbox's host requirements
differ from our current test host (the Colima VM has an ext4 root; Fast
Sandbox wants XFS with reflink for images).

**Hypothesis.** OpenSandbox's Fast Sandbox, at a pinned release, runs every
feature the `opensandbox` adapter advertises with `isolation: microvm` on
(a) the `colima-differently` VM (arm64, nested KVM) with an added XFS reflink
volume and (b) one x86_64 cloud KVM target (`.metal`, or nested
virtualization on a supported family), and restores memory snapshots so that
`snapshots.memory` can be enabled on such pools.

**Method.**

1. Prepare each host to Fast Sandbox's documented requirements: Linux
   ≥ 5.10, `/dev/kvm`, `/dev/net/tun`, cgroup v2, XFS with reflink for images,
   an enlarged `/dev/shm`.
2. Install OpenSandbox with the Fast Sandbox runtime following its own
   documentation; pin the server, `execd` and runtime agent versions.
3. Register an `opensandbox` pool with `trust: private` and
   `isolation: microvm`; add `snapshots.memory` to `config.features` only for
   step 6.
4. Run `TestLiveOpenSandbox` and the end-to-end suite against it; repeat the
   create, exec, files, pause/resume, snapshot, fork, delete cycle 100 times
   and check OpenSandbox for leaked sandboxes and snapshots.
5. Isolation probes from inside a sandbox: the guest kernel differs from the
   host's; cloud metadata `169.254.169.254`, the node's addresses, another
   sandbox, the control plane, the pod and service CIDRs (on Kubernetes), a
   public host.
6. Memory snapshots: a process started before a snapshot is still running
   after restore and in a fork; two clones have distinct machine ID, network
   identity and random output; record how OpenSandbox stores and protects
   snapshot files.
7. Restart the OpenSandbox server and the per-node runtime agent with
   sandboxes running; record what survives and what Differently's reconciler
   does.
8. Benchmarks B1 and B2, compared with the Docker runtime on the same host.
9. Record how Fast Sandbox runs Firecracker (jailer or not, user, seccomp,
   chroot) for the trust model.

**Success criteria.**

- Every advertised feature works through Differently on both hosts; 100
  consecutive cycles leave nothing behind.
- Probes behave as OpenSandbox documents for its default network settings,
  and the result is written down per target.
- Memory restore works as in step 6, or `snapshots.memory` stays off with the
  reason recorded.
- Create-to-running p95 ≤ 3 s for a 1 GiB image (*hypothesis*).
- Results recorded in the [spike](research/opensandbox-spike.md).

**Unblocks.** [Backlog E18](backlog.md#e18-fast-sandbox-spike); microVM
isolation claims in the trust model; the remaining
[ADR 0019](adr/0019-opensandbox-backend-first.md) criteria.

## POC 12. OpenSandbox Kubernetes runtime

**Risk.** Only the Docker runtime has run. With the Kubernetes runtime,
sandbox networking, how the control plane reaches `execd` (through the
server's proxy, the adapter's default, or directly), port endpoints and
multi-node scheduling are unverified, as is the interaction with the Helm
chart's NetworkPolicy, which lets the control plane reach OpenSandbox only on
the server's port.

**Hypothesis.** With OpenSandbox installed from its own chart using the
Kubernetes runtime, and Differently's chart rendering its `opensandbox` pool
with the default `apiURL` (`http://opensandbox-server.opensandbox-system.svc/v1`)
and `isolation: container`, every advertised feature works through the
server proxy, and control-plane egress stays limited to OpenSandbox's port
(80 with its chart).

**Method.** On a local cluster (kind or k3s) and one managed cluster, each
with a CNI that enforces NetworkPolicy (Calico or Cilium):

1. Install OpenSandbox and the Differently chart; register nothing by hand.
2. Run `TestLiveOpenSandbox` (pointed at the in-cluster API) and the
   end-to-end suite.
3. Repeat with `useServerProxy: "false"` and record which paths need extra
   NetworkPolicy rules.
4. Port exposure: whether OpenSandbox returns endpoints without request
   headers (the adapter refuses the others).
5. Spread 50 sandboxes over several nodes; drain a node; record what
   OpenSandbox and Differently report.
6. Probes from inside a sandbox: pod and service CIDRs, the Kubernetes API,
   cloud metadata, the Differently server, a public host.

**Success criteria.** A support matrix and recommended values for both
charts; the features that work recorded per setting; node drain behaviour
documented.

**Unblocks.** Kubernetes guidance in [deploy/](deploy/README.md); Scenario A on
a real cluster; [backlog E19](backlog.md#e19-opensandbox-network-policy-and-egress).

**Result (2026-10-28, partial: steps 1, 2 and 4).** Single-node k3s
v1.36.5 on an Azure `Standard_D8s_v6`, OpenSandbox 1.2.0 from its own charts
(`base`, `controller`, `server`; images from Docker Hub), Differently from its
chart with the embedded PostgreSQL:

- **Works through the server proxy:** create (running in about 2 s with the
  image cached), CPU and memory limits as cgroups, exec (exact output with
  `lineEndings: kept`), streaming, files, quotas, drift reconciliation,
  delete, audit and the trust report. `TestLiveOpenSandbox` and the
  end-to-end suite pass (11/11), and the run is recorded in
  `internal/adapters/opensandbox/testdata/recordings/kubernetes-1.2.0.json`.
- **Needs a snapshot registry:** pause is accepted but never starts (the
  sandbox stays `Running`), and snapshots fail with `RegistryNotConfigured`,
  until the controller has `controller.snapshot.registry`. Without one, list
  only `exec`, `exec.stream` and `files` in the pool's `features`; the
  adapter reports a stalled pause or a registry-less snapshot as
  `unsupported_operation`.
- **Ports:** direct endpoints are pod IPs (`10.42.x.x:<port>`), so
  `ports.expose` is no longer a default feature.
- **Slow first pulls:** OpenSandbox waits 60 s for a pod to become ready and
  then answers 504 `POD_READY_TIMEOUT`, which is also how a missing image
  shows up. The adapter retries it without marking the pool unhealthy.
- **Install notes:** OpenSandbox's `base` chart creates its own namespaces
  (`opensandbox-system`, `opensandbox-dataplane`), so do not create them first;
  its charts default to images in a registry in mainland China, so set
  Docker Hub's `docker.io/opensandbox/*:release-1.2.0` instead.

Not yet run: `useServerProxy: "false"` (step 3), several nodes and a drain
(step 5), probes from inside a sandbox (step 6), and a CNI other than k3s's
built-in NetworkPolicy controller.

## POC 13. OpenSandbox outage and recovery

**Risk.** Every sandbox on an OpenSandbox pool depends on its server being
reachable. What the code does is known; what OpenSandbox does across outages,
restarts and upgrades is not:

- the pool manager probes health every 5 s on every replica; an unhealthy
  pool receives no new placements;
- when `List` fails, the reconciler skips the pool for that pass instead of
  failing its sandboxes;
- a sandbox OpenSandbox no longer lists (or reports `Terminated`) is failed
  as lost; an OpenSandbox `Failed` state fails it with the backend message;
- terminating sandboxes retry `Delete` on every tick until it succeeds;
- OpenSandbox's own `timeout` (set at create, minimum 60 s) can end a
  sandbox before Differently's TTL when the TTL is extended later.

**Hypothesis.** Server outages of 20 s, 60 s and 10 min fail no sandbox and
leak no instance; after a server restart or a patch upgrade, existing
sandboxes are still listed by their `differently-installation` metadata and
answer exec (this depends on OpenSandbox persisting its state, which is
unverified).

**Method.** With the Docker runtime first, then the runtimes of POC 11 and 12:
stop the server container; block traffic from the control plane; restart the
server; upgrade it to the next patch release with sandboxes running; extend a
sandbox's TTL past its OpenSandbox `timeout` and let the backstop expire.
Record sandbox states, audit events (`reconcile.*`), pool health and
`differently_reconcile_actions_total`.

**Success criteria.** Behaviour documented for each case; no sandbox failed
during an outage; operators can alert on pool health within one refresh
interval; every gap filed (for example
[E0.20](backlog.md#e0-correctness-and-security-fixes-from-the-code-review),
TTL backstops).

**Unblocks.** The upgrade procedure in
[backlog E21](backlog.md#e21-opensandbox-version-pinning-and-upgrade-testing);
Scenario J and K claims with a real backend.
