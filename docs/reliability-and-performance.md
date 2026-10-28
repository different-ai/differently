# Reliability and performance

| | |
|---|---|
| Status | **No measurements exist yet.** Every number below is a proposal or a hypothesis to be measured, labelled as such. Nothing has run on real KVM or against live provider accounts. |
| Related | [Architecture](architecture/README.md) · [Proofs of concept](proof-of-concepts.md) · [Backlog](backlog.md) |

## 1. Service level objectives (proposed)

SLOs apply to the control plane an operator runs; Differently cannot promise
the availability of the operator's hardware or of external providers.

| SLO (proposal) | SLI | Measured from today | Hypothesis target |
|---|---|---|---|
| API availability | Share of `/v1` requests not answered with 5xx caused by the control plane, over 30 days | `differently_api_requests_total{route,status}` | 99.9 % with ≥ 2 replicas and HA Postgres |
| Read latency | p99 of `GET /v1/sandboxes/{id}` and list calls | `differently_api_request_seconds{route}` | p99 ≤ 100 ms |
| Admission latency | p99 of `POST /v1/sandboxes` until `202` | `differently_api_request_seconds` | p99 ≤ 250 ms for organizations with < 1 000 live sandboxes |
| Time to running, private pool | Activation to `running` on a warm node with reflink storage | `differently_provision_seconds{pool}` (adapter `Create` duration) plus `differently_queue_wait_seconds` | p50 ≤ 1.5 s, p95 ≤ 3 s for a 1 GiB image |
| Scheduling lag | Admission to activation when capacity and quota are available | `differently_queue_wait_seconds` | p95 ≤ 1 s with one leader |
| Exec overhead | Control-plane-added latency of `exec` for `true` | not instrumented (needs tracing, [backlog E11](backlog.md#e11-opentelemetry-tracing)) | p50 ≤ 30 ms on Firecracker pools |
| Drift detection | Time from a backend instance vanishing to the sandbox being marked failed | `differently_reconcile_actions_total{action="lost"}` | ≤ 2 × `reconcileIntervalSeconds` (60 s) |
| Leader failover | Time from leader process death to the next dispatch | not instrumented | ≤ 10 s on process crash; bounded by Postgres TCP keepalives on host loss |
| Audit completeness | Share of state-changing calls with an audit event | measurable with `differently_audit_append_failures_total`; with `audit.failClosed` unaudited changes are refused | 100 %; alert on any increase of the failure counter |

Missing instrumentation worth adding before SLOs can be enforced: leader
status per replica, agent heartbeat age, reconcile errors, audit append
failures, adapter call latency by operation, and end-to-end traces.

## 2. Telemetry available today

`internal/metrics/metrics.go` (Prometheus `/metrics`, on `metrics.listen` when set):

| Metric | Labels |
|---|---|
| `differently_sandbox_transitions_total` | `to`, `pool` (running, failed, terminated only) |
| `differently_placement_decisions_total` | `outcome` (placed, waiting, rejected), `pool` |
| `differently_provision_seconds` | `pool` |
| `differently_queue_wait_seconds` | |
| `differently_api_requests_total` | `route`, `status` |
| `differently_api_request_seconds` | `route` |
| `differently_reconcile_actions_total` | `action` (orphan_deleted, adopted, lost), `pool` |

Plus Go runtime and process collectors, structured JSON logs with request IDs
(`X-Request-Id`), and the audit log.

## 3. Hot spots found by reading the code

These are where scale testing should look first. None has been measured.

| Area | What the code does | Expected effect |
|---|---|---|
| Quota tally | Every admission and activation loads every non-terminal sandbox document of the organization (and again for the project) inside the organization row lock (`postgres.tally`) | Linear in live sandboxes per organization; serializes one tenant's creates |
| Audit append | One global transaction advisory lock, plus a `SELECT ... ORDER BY seq DESC LIMIT 1` per event | One append at a time per database |
| Dispatch | Per candidate: project read, up to three policy reads, optional snapshot read, decision, pool read, activation transaction, audit append; at most 100 candidates per 250 ms tick | Upper bound well below 400 activations/s per leader |
| Provisioning | One goroutine per `provisioning` sandbox, no concurrency limit | Bursts can open hundreds of concurrent creates against agents or providers |
| Queue read | Postgres reads the 1000 oldest runnable rows by ID, then orders them fairly | Fairness only within that window under very deep queues |
| Pool refresh | Every replica probes every pool every 5 s (Daytona's health check lists sandboxes) | Provider API calls scale with replica count |
| Wait endpoint | `GET .../wait` polls the store every 100 ms per waiting client, up to 300 s | Database load proportional to waiting clients |
| Trust report, usage, audit verify | Read all sandboxes and the whole audit chain | Slow and memory-heavy on large installations |
| Data path | Exec output and files stream through API replicas, buffered up to 100 MiB per file | Replica memory and bandwidth bound file throughput |
| Agent capacity | Counts requested vCPU and memory only; VMM overhead and host page cache are not accounted | Overcommit risk unless `--reserve-memory-mib` covers it |

## 4. Benchmark plan

Run each benchmark on a fixed, recorded environment (instance type, kernel,
Firecracker version, filesystem, Postgres sizing). Publish raw results with
the commit and configuration. Targets are hypotheses; the purpose is to
replace them with measurements.

### B1. Boot latency (Firecracker)

- **Measure:** activation → `running`, split into rootfs copy, network setup
  (eight or more `ip`/`iptables` invocations per VM), VMM configure,
  `InstanceStart`, and guest agent readiness (polled every 100 ms).
- **Vary:** image size (256 MiB, 1 GiB, 4 GiB); reflink (XFS reflink, btrfs)
  versus plain copy; `diskGiB` growth (adds `resize2fs`); concurrent creates
  per node (1, 8, 32); `.metal` versus nested virtualization.
- **Hypotheses:** with reflink, create time is independent of image size and
  p95 ≤ 3 s at 8 concurrent creates; without reflink, copy dominates and scales
  with image size; nested virtualization adds ≥ 20 % to boot time.

### B2. Density

- **Measure:** sandboxes per node until `insufficient_capacity` and until host
  memory pressure; host memory used per idle 512 MiB VM; steady-state CPU of
  idle VMs; agent memory and CPU.
- **Vary:** `--cpu-overcommit` (1, 2, 4), `--reserve-memory-mib`, guest
  memory size.
- **Hypotheses:** Firecracker overhead per VM ≤ 10 MiB beyond guest memory;
  ≥ 95 % of allocatable memory is usable; agent heartbeat (every 10 s) and
  list calls stay under 1 % CPU at 200 VMs.

### B3. Scheduling throughput

- **Measure:** admissions per second (API), activations per second (leader),
  and queue drain time for N queued sandboxes, against Postgres, with the fake
  adapter so backends are not the bottleneck.
- **Vary:** replicas (1, 3, 6), organizations (1, 10, 100), live sandboxes per
  organization (10, 1 000, 10 000), Postgres size.
- **Hypotheses:** ≥ 200 admissions/s across replicas and ≥ 50 activations/s
  per leader at 1 000 live sandboxes per organization; admission latency grows
  linearly with live sandboxes per organization; audit append is the first
  global bottleneck.

### B4. Data path

- **Measure:** exec round trip for `true`; streamed throughput for a command
  printing 1 GiB; 100 MiB upload and download time; API replica memory during
  transfers.
- **Hypotheses:** ≤ 30 ms control-plane overhead per exec; streaming
  ≥ 50 MiB/s per stream; replica memory grows by about the file size per
  concurrent transfer (buffered).

### B5. Reconciler and control loops at scale

- **Measure:** duration of one reconcile pass and of one scheduler tick with
  10 000 live sandboxes across 50 agents; database load per pass.
- **Hypothesis:** a reconcile pass finishes well within the 30 s interval.

### B6. External providers

- **Measure:** create-to-running, exec latency, snapshot time and error rates
  on live Daytona and Freestyle accounts, including 429 behaviour at plan
  limits. Part of [POC 7](proof-of-concepts.md#poc-7-external-provider-live-account-verification).
- **Hypotheses:** none beyond the providers' published claims (Daytona "under
  90 ms" code-to-execution; Freestyle p99 under 400 ms provisioning), which
  must not be repeated as Differently numbers.

## 5. Chaos tests

Each test states the expected behaviour **according to the current code**.
Where the code is expected to misbehave, the test is still worth running so
the fix can be verified.

| # | Inject | Expected today | Pass criterion |
|---|---|---|---|
| C1 | Kill the leader process | Postgres ends the session, another replica takes `dly_ldr` on its next tick; stale `provisioning` rows are adopted or requeued after `provisionTimeoutSeconds` | Dispatch resumes within 10 s; no duplicate instances; quota unchanged |
| C2 | Partition the leader host from Postgres without closing sockets | The old leader's ping fails and it stops leading; the lock is released only when Postgres detects the dead session | With server TCP keepalives configured, takeover within the keepalive window; no double activation |
| C3 | Postgres primary failover (CloudNativePG switchover and forced failover) | Requests fail during the switch; connections reconnect; leadership re-elected | No committed admission or activation lost with synchronous replication; no quota over-allocation |
| C4 | Block agent → server heartbeats for 60 s on one node | **Known issue:** after 30 s the agent drops out of `List`, the reconciler fails its sandboxes as lost, and when it returns their VMs are deleted as orphans | After the fix: sandboxes on a silent node are marked unknown, not failed, and nothing is deleted |
| C5 | Restart the agent container | VMs expected to die with the container (PID namespace); disks survive; store still says `running` (state not reconciled) | After the fix: either VMs survive, or sandboxes move to `stopped` and can start again |
| C6 | Reboot a KVM node | VMs stop; `start` fails because taps are not recreated (known issue) | After the fix: `start` succeeds from the retained disk |
| C7 | Provider returns 429, 5xx, timeouts, slow creates beyond 120 s | 429 requeues without counting attempts; 5xx and timeouts mark the pool unhealthy, back off and fail after 3 attempts; timed-out creates may leave instances that the reconciler deletes as orphans or adopts | No leaked instances after one reconcile pass; sandboxes either run or fail with a clear reason |
| C8 | Fill the node's state disk | Create fails during copy (`provider_unavailable`), pool marked unhealthy; snapshots fail | Clear error, no partial VM directories left |
| C9 | Kill an API replica mid-stream | Client stream breaks; command continues inside the VM until its timeout | Documented; no server-side leak |
| C10 | Clock skew of 60 s between replicas | Leader's clock drives `nextAttemptAt`, TTL and agent liveness; agent liveness compares heartbeats written by any replica with the reader's clock | Liveness tolerates configured skew; document the NTP requirement |
| C11 | Run `POST /v1/admin/reconcile` on a non-leader during a leader reconcile | Two reconcilers act at once (no leader check); operations are idempotent | No duplicate side effects beyond repeated deletes |
| C12 | Kill a Firecracker process under a running sandbox | Agent still reports the VM; exec fails; reconciler does not notice (existence only) | After the fix: sandbox marked failed or stopped within one reconcile pass |

Run C1 to C3 against the Postgres store with at least three replicas; run C4
to C8 and C12 on real KVM nodes (depends on [POC 1](proof-of-concepts.md#poc-1-firecracker-on-real-kvm)).

## 6. Capacity validation

Checks that the capacity model matches reality before relying on it:

1. **Node accounting.** Allocatable CPU = `(totalCPU − reserveCPU) ×
   cpuOvercommit`; allocatable memory = `totalMemory − reserveMemory`. Pending,
   running and paused VMs count; stopped VMs do not. Verify on a node that
   creating until `insufficient_capacity` never drives the host into swap or
   the OOM killer, and size the reserve to cover VMM overhead.
2. **Fragmentation.** The pool reports the node with the most free memory as
   the "largest" node and uses its CPU and memory for the fit check. Construct
   pools where CPU and memory are free on different nodes and confirm that
   placement waits rather than failing at the agent.
3. **Staleness.** Capacity is the agents' last heartbeat (every 10 s) cached by
   the pool manager (refreshed every 5 s). Under bursts, agents correctly answer
   `insufficient_capacity` and the scheduler retries; measure how many retries a
   burst causes.
4. **External pools.** Unbounded adapters rely on `maxConcurrency`. Verify
   that the cap holds under concurrent activations across replicas (the
   conformance test `PoolConcurrencyCap` covers it in-process) and that
   provider plan limits surface as `insufficient_capacity` (429) rather than
   failures.
5. **Quota math.** Repeat the store conformance suite against Postgres under
   load, and add a test for stop/start quota accounting once the known issue is
   fixed.

## 7. Release gates (proposal)

For a release candidate (`vX.Y.Z-rc.N`, [ADR 0012](adr/0012-pre-release-integration-branch-model.md)):

1. `go test ./...` with `DIFFERENTLY_TEST_POSTGRES_URL` set, so the Postgres
   conformance suite actually runs.
2. SDK test suites (TypeScript, Python) against a dev-mode server.
3. Real-KVM smoke test on at least one `.metal` node: create, exec, files,
   stop/start, pause/resume, snapshot, fork, delete, network isolation probes.
4. Chaos tests C1, C3 and C4 to C6 once their fixes land.
5. B1 and B3 results recorded and compared with the previous release.
