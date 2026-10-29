# ADR 0016: Single scheduler leader elected with a Postgres advisory lock

- Status: Accepted
- Date: 2026-10-28

## Context

API replicas must scale horizontally and survive individual failures. The
scheduler's loops (dispatch, provisioning, termination, expiry,
reconciliation) are simpler and cheaper to reason about with one active
instance: fair ordering is computed over the whole queue, provisioning
goroutines are tracked in one in-flight set, and the reconciler compares one
view of the store with each backend. A separate coordination service would
contradict [ADR 0004](0004-postgres-only-production-dependency.md).

## Decision

- Every replica runs the scheduler loop (`internal/scheduler/scheduler.go`
  `Run`), but each pass first calls `Store.TryLeader`.
- The Postgres store takes `pg_try_advisory_lock(dly_ldr)` on a connection
  held for the process lifetime and pings that connection on every check; a
  failed ping releases it and leadership is lost
  (`internal/store/postgres/postgres.go` `TryLeader`).
- Correctness does not depend on exclusive leadership: activation is an
  atomic, status-checked transaction, and `adapter.Create` is idempotent on
  the sandbox ID, so a brief overlap of two leaders converges.
- Pool health probes (`Pools.Refresh`, every 5 s) run on every replica, because
  every replica serves exec and lifecycle calls.

## Consequences

- No extra infrastructure; failover is automatic once Postgres ends the old
  session.
- Takeover time depends on how fast Postgres notices a dead client. A crashed
  process closes its socket immediately; a partitioned host may hold the lock
  until server-side TCP keepalives fire. Operators should configure
  `tcp_keepalives_idle`, `tcp_keepalives_interval` and `tcp_keepalives_count`.
- Scheduling throughput is one process's throughput. Dispatch handles at most
  100 candidates per tick (250 ms default).
- `Scheduler.Wake()` is local, so a create on a non-leader replica is picked up
  on the leader's next tick rather than immediately.
- `POST /v1/admin/reconcile` runs the reconciler on whichever replica serves
  the request, without the leader check, so it can overlap with the leader's
  own reconcile pass. The operations are idempotent, but this should be routed
  to the leader ([backlog E0](../backlog.md#e0-correctness-and-security-fixes-from-the-code-review)).
- Every replica probing every pool multiplies provider API calls (Daytona's
  health check lists sandboxes) by the replica count.

## Alternatives considered

- **Kubernetes Lease objects.** Ties the control plane to Kubernetes
  ([ADR 0003](0003-kubernetes-as-substrate-not-api.md)).
- **Sharded schedulers (by organization or pool).** More throughput, more
  complexity; revisit when one leader is measured to be the bottleneck.
- **Every replica schedules with row-level claims (`FOR UPDATE SKIP LOCKED`).**
  Viable and possibly the next step; fairness across organizations would then
  be approximate.
