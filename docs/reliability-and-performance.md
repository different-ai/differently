# Reliability and performance

| | |
|---|---|
| Status | **Almost no measurements exist.** The only one: OpenSandbox's Docker runtime reached running in 0.5–0.9 s per sandbox in the adapter's live test (one arm64 Colima VM, server `release-1.2.0`, `execd` v1.1.0; [spike](research/opensandbox-spike.md)). Every other number below is a proposal or a hypothesis, labelled as such. Nothing has run on KVM through OpenSandbox or against live provider accounts. |
| Runtime change | Since [ADR 0020](adr/0020-opensandbox-replaces-node-agent.md), private sandboxes run on OpenSandbox; boot time, density and isolation are properties of the runtime the operator deploys. Differently's own numbers are the control-plane ones. |
| Related | [Architecture](architecture/README.md) · [Proofs of concept](proof-of-concepts.md) · [Backlog](backlog.md) |

## 1. Service level objectives (proposed)

SLOs apply to the control plane an operator runs; Differently cannot promise
the availability of the operator's OpenSandbox installation, hardware or
external providers.

| SLO (proposal) | SLI | Measured from today | Hypothesis target |
|---|---|---|---|
| API availability | Share of `/v1` requests not answered with 5xx caused by the control plane, over 30 days | `differently_api_requests_total{route,status}` | 99.9 % with ≥ 2 replicas and HA Postgres |
| Read latency | p99 of `GET /v1/sandboxes/{id}` and list calls | `differently_api_request_seconds{route}` | p99 ≤ 100 ms |
| Admission latency | p99 of `POST /v1/sandboxes` until `202` | `differently_api_request_seconds` | p99 ≤ 250 ms for organizations with < 1 000 live sandboxes |
| Time to running, private pool | Activation to `running` on an OpenSandbox pool with the image already cached; includes the adapter's wait for `Running` and for `execd` | `differently_provision_seconds{pool}` (adapter `Create` duration) plus `differently_queue_wait_seconds` | Docker runtime: p95 ≤ 2 s; Fast Sandbox: p95 ≤ 3 s for a 1 GiB image |
| Scheduling lag | Admission to activation when capacity and quota are available | `differently_queue_wait_seconds` | p95 ≤ 1 s with one leader |
| Exec overhead | Control-plane-added latency of `exec` for `true` | not instrumented (needs tracing, [backlog E11](backlog.md#e11-opentelemetry-tracing)) | p50 ≤ 30 ms on OpenSandbox pools, excluding OpenSandbox's own proxy |
| Drift detection | Time from a backend instance vanishing to the sandbox being marked failed | `differently_reconcile_actions_total{action="lost"}` | ≤ 2 × `reconcileIntervalSeconds` (60 s) plus the 30 s reconcile grace |
| Leader failover | Time from leader process death to the next dispatch | not instrumented | ≤ 10 s on process crash; bounded by Postgres TCP keepalives on host loss |
| Audit completeness | Share of state-changing calls with an audit event | measurable with `differently_audit_append_failures_total`; with `audit.failClosed` unaudited changes are refused | 100 %; alert on any increase of the failure counter |

Missing instrumentation worth adding before SLOs can be enforced: leader
status per replica, pool health and the age of the last successful probe,
reconcile errors, adapter call latency by operation, and end-to-end traces.

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
| `differently_audit_append_failures_total` | |
| `differently_reconcile_actions_total` | `action` (orphan_deleted, adopted, lost, stopped, failed), `pool` |

Plus Go runtime and process collectors, structured JSON logs with request IDs
(`X-Request-Id`), and the audit log. OpenSandbox exposes its own diagnostics;
Differently does not collect them.

## 3. Hot spots found by reading the code

These are where scale testing should look first. None has been measured.

| Area | What the code does | Expected effect |
|---|---|---|
| Quota tally | Every admission and activation loads every non-terminal sandbox document of the organization (and again for the project) inside the organization row lock (`postgres.tally`) | Linear in live sandboxes per organization; serializes one tenant's creates |
| Audit append | One global transaction advisory lock, plus a `SELECT ... ORDER BY seq DESC LIMIT 1` per event | One append at a time per database |
| Dispatch | Per candidate: project read, up to three policy reads, optional snapshot read, decision, pool read, activation transaction, audit append; at most 100 candidates per 250 ms tick | Upper bound well below 400 activations/s per leader |
| Provisioning | One goroutine per `provisioning` sandbox, no concurrency limit | Bursts can open hundreds of concurrent creates against OpenSandbox or providers |
| OpenSandbox create | Each `Create` first lists sandboxes by metadata (idempotency), then creates, then polls state every 500 ms and `execd` every 200 ms | Several lifecycle calls per sandbox; polling adds up to about 0.7 s to time to running |
| OpenSandbox list | Each reconcile pass pages through the installation's sandboxes, 100 per page | Lifecycle API load grows with live sandboxes per pool |
| Queue read | Postgres reads the 1000 oldest runnable rows by ID, then orders them fairly | Fairness only within that window under very deep queues |
| Pool refresh | Every replica probes every pool every 5 s (the OpenSandbox and Daytona health checks list sandboxes) | Backend API calls scale with replica count |
| Wait endpoint | `GET .../wait` polls the store every 100 ms per waiting client, up to 300 s | Database load proportional to waiting clients |
| Trust report, usage, audit verify | Read all sandboxes and the whole audit chain | Slow and memory-heavy on large installations |
| Data path | Exec output and files stream through API replicas, buffered up to 100 MiB per file; by default `execd` traffic also passes through OpenSandbox's server proxy | Replica memory and bandwidth bound file throughput; one extra hop per call |
| Synchronous exec | The `opensandbox` adapter collects all stdout and stderr in memory with no cap ([E0.35](backlog.md#e0-correctness-and-security-fixes-from-the-code-review)) | A command with large output can exhaust replica memory |
| Backend capacity | OpenSandbox reports no capacity; only `maxConcurrency` limits a pool | Without a cap, placement keeps sending work until OpenSandbox's `Create` fails |

## 4. Benchmark plan

Run each benchmark on a fixed, recorded environment (instance type, kernel,
OpenSandbox server, `execd` and runtime versions, filesystem, Postgres
sizing). Publish raw results with the commit and configuration. Targets are
hypotheses; the purpose is to replace them with measurements.

### B1. Boot latency (OpenSandbox runtimes)

- **Measure:** activation → `running`, split into the idempotency lookup,
  OpenSandbox create until `Running`, and `execd` readiness.
- **Vary:** runtime (Docker, Kubernetes, Fast Sandbox); image size (256 MiB,
  1 GiB, 4 GiB); image cached versus pulled; Fast Sandbox `templateId` versus
  image; concurrent creates (1, 8, 32); `.metal` versus nested
  virtualization for Fast Sandbox.
- **Hypotheses:** Docker runtime with a cached image p95 ≤ 2 s at 8
  concurrent creates (the single-sandbox observation is 0.5–0.9 s); Fast
  Sandbox p95 ≤ 3 s; nested virtualization adds ≥ 20 % to Fast Sandbox boot
  time.

### B2. Density

- **Measure:** sandboxes per runtime host until OpenSandbox refuses or the
  host is under memory pressure; host memory per idle 512 MiB sandbox;
  steady-state CPU of idle sandboxes; OpenSandbox server and runtime-agent
  overhead.
- **Vary:** runtime; guest memory size; OpenSandbox's own limits.
- **Hypotheses:** none for Differently, which places no per-host limits;
  record what each runtime allows so operators can set `maxConcurrency`.

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
  transfers. Through OpenSandbox's server proxy (`useServerProxy` default)
  and directly.
- **Hypotheses:** ≤ 30 ms control-plane overhead per exec; streaming
  ≥ 50 MiB/s per stream; replica memory grows by about the file size per
  concurrent transfer (buffered).

### B5. Reconciler and control loops at scale

- **Measure:** duration of one reconcile pass and of one scheduler tick with
  10 000 live sandboxes across several OpenSandbox pools; database and
  OpenSandbox API load per pass.
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
the fix can be verified. C4–C6, C8 and C12 were rewritten for OpenSandbox
pools; the agent-specific versions left with the agent
([ADR 0020](adr/0020-opensandbox-replaces-node-agent.md)).

| # | Inject | Expected today | Pass criterion |
|---|---|---|---|
| C1 | Kill the leader process | Postgres ends the session, another replica takes `dly_ldr` on its next tick; stale `provisioning` rows are adopted or requeued after `provisionTimeoutSeconds` | Dispatch resumes within 10 s; no duplicate instances; quota unchanged |
| C2 | Partition the leader host from Postgres without closing sockets | The old leader's ping fails and it stops leading; the lock is released only when Postgres detects the dead session | With server TCP keepalives configured, takeover within the keepalive window; no double activation |
| C3 | Postgres primary failover (CloudNativePG switchover and forced failover) | Requests fail during the switch; connections reconnect; leadership re-elected | No committed admission or activation lost with synchronous replication; no quota over-allocation |
| C4 | Make the OpenSandbox server unreachable for 60 s | The pool turns unhealthy within 5 s; `List` fails, so the reconciler skips the pool; new work waits or goes to another permitted pool; exec and files fail with `provider_unavailable` | No sandbox failed, nothing deleted; work resumes when the server returns |
| C5 | Restart the OpenSandbox server with sandboxes running | Unknown: depends on whether OpenSandbox keeps its sandboxes and their metadata across restarts ([POC 13](proof-of-concepts.md#poc-13-opensandbox-outage-and-recovery)) | Sandboxes still listed and usable afterwards, or the loss documented and reported as failed sandboxes |
| C6 | Reboot or drain a runtime host (Docker host, Kubernetes node, Fast Sandbox node) | Runtime-dependent; a sandbox OpenSandbox stops listing is failed as lost after the reconcile grace; there is no stop/start to recover from disk | Affected sandboxes failed with a clear reason within one reconcile pass; others unaffected |
| C7 | Backend returns 429, 5xx, timeouts, slow creates beyond 120 s | 429 requeues without counting attempts; 5xx and timeouts mark the pool unhealthy, back off and fail after 3 attempts; timed-out creates may leave instances that the reconciler deletes as orphans or adopts | No leaked instances after one reconcile pass; sandboxes either run or fail with a clear reason |
| C8 | Fill the runtime host's image or container storage | `Create` fails (`provider_unavailable` or `invalid_request`, depending on OpenSandbox's answer); snapshots fail | Clear error; no sandbox left behind in OpenSandbox |
| C9 | Kill an API replica mid-stream | Client stream breaks; command continues inside the sandbox until its timeout | Documented; no server-side leak |
| C10 | Clock skew of 60 s between replicas | The leader's clock drives `nextAttemptAt`, TTL and the reconcile grace | Behaviour within the configured skew documented; NTP requirement stated |
| C11 | Run `POST /v1/admin/reconcile` on a non-leader during a leader reconcile | Two reconcilers act at once (no leader check); operations are idempotent | No duplicate side effects beyond repeated deletes |
| C12 | Kill a sandbox's container (Docker runtime) behind OpenSandbox | OpenSandbox reports it `Failed` or no longer lists it; the reconciler fails the sandbox (backend message, or "backend instance disappeared") | Sandbox marked failed within one reconcile pass plus the 30 s grace |

Run C1 to C3 against the Postgres store with at least three replicas; run C4
to C8 and C12 against a real OpenSandbox server, first with the Docker runtime
and then with the runtimes of
[POC 11](proof-of-concepts.md#poc-11-fast-sandbox-through-the-adapter) and
[POC 12](proof-of-concepts.md#poc-12-opensandbox-kubernetes-runtime).

## 6. Capacity validation

Checks that the capacity model matches reality before relying on it:

1. **Private OpenSandbox pools are unbounded for placement.** The adapter
   reports no capacity, so only `maxConcurrency` limits a pool. Create
   sandboxes until OpenSandbox refuses, record the limit and the error it
   returns (429 becomes `insufficient_capacity` and requeues; other errors
   count against pool health), and set `maxConcurrency` below it. Without a
   cap, `overflow` placement never moves work to an external pool while the
   private pool is healthy.
2. **Resource limits.** Check that the runtime enforces the CPU and memory
   limits Differently sends, and record what disk a sandbox actually gets
   (`diskGiB` is not sent).
3. **Staleness.** Health and capabilities are refreshed every 5 s per
   replica. Under bursts, measure how many retries a burst causes when
   OpenSandbox starts refusing.
4. **External pools.** Unbounded adapters rely on `maxConcurrency`. Verify
   that the cap holds under concurrent activations across replicas (the
   conformance test `PoolConcurrencyCap` covers it in-process) and that
   provider plan limits surface as `insufficient_capacity` (429) rather than
   failures.
5. **Quota math.** Repeat the store conformance suite against Postgres under
   load, including `ConcurrentStartRespectsCPUQuota` for backends with
   stop/start.

## 7. Release gates (proposal)

For a release candidate (`vX.Y.Z-rc.N`, [ADR 0012](adr/0012-pre-release-integration-branch-model.md)):

1. `go test ./...` with `DIFFERENTLY_TEST_POSTGRES_URL` set, so the Postgres
   conformance suite actually runs.
2. SDK test suites (TypeScript, Python) against a dev-mode server.
3. `TestLiveOpenSandbox` (`go test -tags live`) and the end-to-end suite
   (`test/e2e`, the real control plane against a real OpenSandbox server with
   the Docker runtime) at the pinned OpenSandbox versions
   ([E21](backlog.md#e21-opensandbox-version-pinning-and-upgrade-testing)).
   Once [E18](backlog.md#e18-fast-sandbox-spike) lands, the same against Fast
   Sandbox on a KVM target.
4. Chaos tests C1, C3, C4, C5 and C12.
5. B1 and B3 results recorded and compared with the previous release.
