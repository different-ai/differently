# ADR 0005: Derive quota usage from sandbox state; admit and activate atomically

- Status: Accepted
- Date: 2026-10-28

## Context

Quotas are per organization and per project: active sandboxes, queued
sandboxes, CPU, memory, disk, maximum TTL, snapshots. Several API replicas and
a scheduler change sandbox state concurrently. Counter-based quota systems
drift when a process crashes between "change the resource" and "change the
counter", and fixing drift needs a repair job. Two moments matter:

- **Admission**: can this request ever be accepted, and is the tenant's queue
  full?
- **Activation**: may this sandbox start consuming capacity now?

A tenant at its concurrency limit should queue, not fail.

## Decision

- Quota usage is never stored. `store.Tally` computes it from the sandbox rows
  every time (`internal/store/store.go`).
- `AdmitSandbox` is one store operation: lock the organization row, replay a
  matching `Idempotency-Key` within the project, run `CheckAdmission` for the
  organization and the project, insert the row as `queued`. Admission rejects
  requests larger than a per-tenant limit, a TTL above `maxTTLSeconds`, and a
  full queue (`maxQueued`). It does not check concurrency.
- `ActivateSandbox` is one store operation: lock organization, then pool (when
  the pool has a concurrency cap), then the sandbox row; verify the sandbox is
  still `queued`; run `CheckActivation` (active sandboxes, CPU, memory, disk)
  for organization and project; count pool slots; move the row to
  `provisioning`.
- Releasing usage needs no counter update: terminating a sandbox is just a
  status change.
- The conformance suite proves the properties concurrently:
  `ConcurrentAdmissionRespectsQueueLimit`, `ConcurrentActivationRespectsQuota`,
  `PoolConcurrencyCap`, `Idempotency`.

## Consequences

- No drift and no repair job; a crash leaves usage correct by construction.
- Quota errors carry a `dimension` detail (`activeSandboxes`, `cpu`,
  `memoryMiB`, `diskGiB`, `ttlSeconds`, `queued`, `snapshots`).
- Each admission and activation reads every non-terminal sandbox document of
  the organization under its row lock. Cost grows with the number of live
  sandboxes per organization; large tenants will need an aggregate query or a
  maintained summary that is still updated in the same transaction.
- Paths that **add** usage must go through a quota check. Today one path does
  not: starting a stopped sandbox (`stopped` releases CPU and memory, and
  `POST .../start` re-reserves them without `CheckActivation`). This is a known
  issue ([backlog E0](../backlog.md#e0-correctness-and-security-fixes-from-the-code-review)).
- The snapshot limit is checked outside a transaction and only at organization
  level, so concurrent snapshot requests can exceed it slightly.

## Alternatives considered

- **Counters updated alongside state.** Rejected: drift on crash, and a repair
  job to write and trust.
- **Optimistic concurrency (compare-and-swap on a version column).** Workable,
  but retries under contention on one hot tenant are worse than a short row
  lock.
- **Check quota only at admission.** Rejected: a tenant at its limit would be
  refused instead of queued, and quota released by deletions would not let
  queued work start.
